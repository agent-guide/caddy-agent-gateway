package builtin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/agent"
)

func TestParseCancelMode(t *testing.T) {
	cases := map[string]struct {
		want    CancelMode
		wantErr bool
	}{
		"":         {want: CancelModeForce},
		"force":    {want: CancelModeForce},
		"graceful": {want: CancelModeGraceful},
		" force ":  {want: CancelModeForce},
		"nope":     {wantErr: true},
	}
	for in, want := range cases {
		got, err := ParseCancelMode(in)
		if want.wantErr {
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("ParseCancelMode(%q) err = %v, want ErrInvalidRequest", in, err)
			}
			continue
		}
		if err != nil || got != want.want {
			t.Fatalf("ParseCancelMode(%q) = %q, %v; want %q, nil", in, got, err, want.want)
		}
	}
}

func TestCancelTurnUnknownReturnsFalse(t *testing.T) {
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"triage": testAgent("triage")}},
		Models: &fakeModelResolver{model: replyModel("x")},
	})
	cancelled, err := host.CancelTurn("triage", "missing-session", CancelModeForce)
	if err != nil {
		t.Fatalf("CancelTurn() err = %v", err)
	}
	if cancelled {
		t.Fatal("CancelTurn() = true for an unknown turn, want false")
	}
	if len(host.ListInFlight()) != 0 {
		t.Fatal("ListInFlight() not empty for an idle host")
	}
}

func TestActivityRegistryDeregisterPreservesReplacement(t *testing.T) {
	r := newActivityRegistry()
	old := &inflightTurn{agentID: "triage", sessionID: "session-1"}
	newer := &inflightTurn{agentID: "triage", sessionID: "session-1"}
	r.register(old)
	r.register(newer)

	r.deregister(old)

	r.mu.Lock()
	got := r.turns[activityKey("triage", "session-1")]
	r.mu.Unlock()
	if got != newer {
		t.Fatalf("replacement entry = %p, want %p", got, newer)
	}
}

func TestActivityRegistryCancelReportsContribution(t *testing.T) {
	r := newActivityRegistry()
	r.register(&inflightTurn{
		agentID:   "triage",
		sessionID: "session-1",
		cancel: func(...adk.AgentCancelOption) (*adk.CancelHandle, bool) {
			return nil, false
		},
	})

	if r.cancel("triage", "session-1", CancelModeForce) {
		t.Fatal("cancel() = true when ADK reports that execution already ended")
	}
}

// TestServeTurnForceCancel drives a turn whose model call blocks, force-cancels
// it through the operator surface, and asserts the turn ends with a cancelled
// done, commits nothing, and is deregistered.
func TestServeTurnForceCancel(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	model := &fakeChatModel{
		block:   block,
		started: started,
		script:  func(_ []*schema.Message) *schema.Message { return schema.AssistantMessage("late", nil) },
	}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"triage": testAgent("triage")}},
		Models: &fakeModelResolver{model: model},
	})

	sink := &collectedEvents{}
	done := make(chan error, 1)
	go func() {
		done <- host.ServeTurn(context.Background(), "triage", TurnRequest{Input: "hi"}, sink.sink)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(block)
		t.Fatal("model call did not start")
	}

	sessionID := ""
	for i := 0; i < 200; i++ {
		if inflight := host.ListInFlight(); len(inflight) == 1 {
			sessionID = inflight[0].SessionID
			if inflight[0].Operation != "turn" {
				t.Fatalf("in-flight operation = %q, want %q", inflight[0].Operation, "turn")
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sessionID == "" {
		close(block)
		t.Fatal("no in-flight turn registered")
	}

	cancelled, err := host.CancelTurn("triage", sessionID, CancelModeForce)
	if err != nil || !cancelled {
		close(block)
		t.Fatalf("CancelTurn() = %v, %v; want true, nil", cancelled, err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled ServeTurn() returned err = %v", err)
		}
	case <-time.After(3 * time.Second):
		close(block)
		t.Fatal("ServeTurn did not return after force cancel")
	}
	close(block) // release the abandoned model goroutine

	dones := sink.byName(EventDone)
	if len(dones) != 1 || dones[0].StopReason != StopReasonCancelled {
		t.Fatalf("done events = %+v, want one with stop_reason %q", dones, StopReasonCancelled)
	}
	if len(sink.byName(EventContent)) != 0 {
		t.Fatalf("cancelled turn emitted content events: %+v", sink.byName(EventContent))
	}
	if got := host.ListInFlight(); len(got) != 0 {
		t.Fatalf("ListInFlight() = %+v after cancel, want empty", got)
	}

	// The aborted exchange committed nothing: the session history stays empty.
	host.sessions.mu.Lock()
	sess := host.sessions.agents["triage"][sessionID]
	msgs := 0
	if sess != nil {
		msgs = len(sess.messages)
	}
	host.sessions.mu.Unlock()
	if msgs != 0 {
		t.Fatalf("session history has %d messages after cancel, want 0", msgs)
	}
}

func TestServeTurnGracefulCancelPropagatesToSequentialChild(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	model := &fakeChatModel{
		block:   block,
		started: started,
		script:  func(_ []*schema.Message) *schema.Message { return schema.AssistantMessage("late", nil) },
	}
	a := testAgent("triage")
	a.Runtime.Builtin.Topology = agent.BuiltinTopology{
		Kind:      agent.TopologyKindSequential,
		SubAgents: []agent.BuiltinSubAgent{{Name: "worker-1"}, {Name: "worker-2"}},
	}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"triage": a}},
		Models: &fakeModelResolver{model: model},
	})

	sink := &collectedEvents{}
	done := make(chan error, 1)
	go func() {
		done <- host.ServeTurn(context.Background(), "triage", TurnRequest{Input: "hi"}, sink.sink)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(block)
		t.Fatal("child model call did not start")
	}
	inflight := host.ListInFlight()
	if len(inflight) != 1 {
		close(block)
		t.Fatalf("ListInFlight() = %+v, want one turn", inflight)
	}
	cancelled, err := host.CancelTurn("triage", inflight[0].SessionID, CancelModeGraceful)
	if err != nil || !cancelled {
		close(block)
		t.Fatalf("CancelTurn() = %v, %v; want true, nil", cancelled, err)
	}
	close(block)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("gracefully cancelled ServeTurn() returned err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeTurn did not stop at the child model safe point")
	}
	dones := sink.byName(EventDone)
	if len(dones) != 1 || dones[0].StopReason != StopReasonCancelled {
		t.Fatalf("done events = %+v, want one with stop_reason %q", dones, StopReasonCancelled)
	}
}
