package builtin

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/agent"
)

// upstreamStatusError carries an HTTP status through the statuserr.StatusCoder
// seam, like provider.UpstreamError does in production.
type upstreamStatusError struct {
	status int
}

func (e *upstreamStatusError) Error() string { return "upstream failure" }

func (e *upstreamStatusError) StatusCode() int { return e.status }

func retryAgent(id string, maxRetries int) agent.Agent {
	a := testAgent(id)
	a.Runtime.Builtin.Model.Retry = &agent.BuiltinModelRetry{MaxRetries: maxRetries}
	return a
}

func TestServeTurnModelRetryRecoversTransientErrors(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	model := &fakeChatModel{scriptErr: func(_ []*schema.Message) (*schema.Message, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts <= 2 {
			return nil, &upstreamStatusError{status: 502}
		}
		return schema.AssistantMessage("recovered", nil), nil
	}}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"retry": retryAgent("retry", 3)}},
		Models: &fakeModelResolver{model: model},
	})

	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "retry", TurnRequest{Input: "go"}, sink.sink); err != nil {
		t.Fatalf("ServeTurn() error = %v, want the third attempt to succeed", err)
	}
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 3 {
		t.Fatalf("model attempts = %d, want 3 (two retries then success)", got)
	}
	contents := sink.byName(EventContent)
	if len(contents) == 0 || !strings.Contains(contents[len(contents)-1].Text, "recovered") {
		t.Fatalf("contents = %+v, want the recovered reply", contents)
	}
}

func TestServeTurnModelRetrySkipsClientErrors(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	model := &fakeChatModel{scriptErr: func(_ []*schema.Message) (*schema.Message, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		return nil, &upstreamStatusError{status: 400}
	}}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"retry": retryAgent("retry", 3)}},
		Models: &fakeModelResolver{model: model},
	})

	if err := host.ServeTurn(t.Context(), "retry", TurnRequest{Input: "go"}, (&collectedEvents{}).sink); err == nil {
		t.Fatal("ServeTurn() = nil, want the client error to fail the turn")
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Fatalf("model attempts = %d, want 1 (a 4xx must not burn retries)", attempts)
	}
}

func TestServeTurnModelRetryExhaustionFailsTheTurn(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	model := &fakeChatModel{scriptErr: func(_ []*schema.Message) (*schema.Message, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		return nil, &upstreamStatusError{status: 503}
	}}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"retry": retryAgent("retry", 2)}},
		Models: &fakeModelResolver{model: model},
	})

	err := host.ServeTurn(t.Context(), "retry", TurnRequest{Input: "go"}, (&collectedEvents{}).sink)
	if err == nil {
		t.Fatal("ServeTurn() = nil, want exhaustion to fail the turn")
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 {
		t.Fatalf("model attempts = %d, want 3 (initial call plus two retries)", attempts)
	}
}

func TestServeTurnPlanExecuteRejectsInheritedRetry(t *testing.T) {
	a := retryAgent("pe", 2)
	a.Runtime.Builtin.Topology = agent.BuiltinTopology{Kind: agent.TopologyKindPlanExecute}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"pe": a}},
		Models: &fakeModelResolver{model: replyModel("x")},
	})

	err := host.ServeTurn(t.Context(), "pe", TurnRequest{Input: "go"}, (&collectedEvents{}).sink)
	if err == nil || !strings.Contains(err.Error(), "model.retry is not supported for planexecute roles") {
		t.Fatalf("ServeTurn() error = %v, want the materialization backstop", err)
	}
	if errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("materialization failure must not map to a client-correctable error: %v", err)
	}
}
