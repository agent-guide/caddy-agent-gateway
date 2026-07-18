package builtin

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	basemcp "github.com/agent-guide/agent-gateway/pkg/mcp"
)

// interactiveAgent is a single-node agent with one MCP tool and interactive
// permissions.
func interactiveAgent(id string) agent.Agent {
	a := testAgent(id)
	a.Runtime.Builtin.Tools = []agent.BuiltinToolSelection{{MCPServiceID: "docs", Tools: []string{"fetch_doc"}}}
	a.Resources.MCPServiceIDs = []string{"docs"}
	a.Runtime.Builtin.Permissions = &agent.BuiltinPermissions{Mode: agent.PermissionModeInteractive}
	return a
}

// gatedToolModel calls fetch_doc once, then answers with the tool result
// echoed so tests can assert on what the model saw.
func gatedToolModel() *fakeChatModel {
	var mu sync.Mutex
	calls := 0
	return &fakeChatModel{script: func(input []*schema.Message) *schema.Message {
		mu.Lock()
		defer mu.Unlock()
		last := input[len(input)-1]
		if last.Role == schema.Tool {
			return schema.AssistantMessage("result: "+last.Content, nil)
		}
		calls++
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID:       fmt.Sprintf("call-%d", calls),
			Type:     "function",
			Function: schema.FunctionCall{Name: "fetch_doc", Arguments: `{"id":"x"}`},
		}})
	}}
}

func fetchDocCaller(output string) *fakeToolCaller {
	return &fakeToolCaller{
		tools: []basemcp.Tool{{Name: "fetch_doc", Description: "Fetch a document", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}},
		}}},
		result: &basemcp.ToolResult{Content: []any{map[string]any{"type": "text", "text": output}}},
	}
}

type permissionEventPayload struct {
	RequestID string                  `json:"request_id"`
	ExpiresAt time.Time               `json:"expires_at"`
	Calls     []PendingPermissionCall `json:"calls"`
}

// suspendOneTurn runs one input turn to the permission interrupt and returns
// the session id and the permission payload.
func suspendOneTurn(t *testing.T, host *Host, agentID string) (string, permissionEventPayload) {
	t.Helper()
	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), agentID, TurnRequest{Input: "do it"}, sink.sink); err != nil {
		t.Fatalf("interactive turn error = %v", err)
	}
	names := sink.names()
	dones := sink.byName(EventDone)
	if len(dones) != 1 || dones[0].StopReason != StopReasonPermissionRequired {
		t.Fatalf("events = %v (done=%+v), want done with stop_reason permission_required", names, dones)
	}
	perms := sink.byName(EventPermission)
	if len(perms) != 1 {
		t.Fatalf("events = %v, want exactly one permission event", names)
	}
	var payload permissionEventPayload
	if err := json.Unmarshal(perms[0].Data, &payload); err != nil {
		t.Fatalf("permission payload %s: %v", perms[0].Data, err)
	}
	if payload.RequestID == "" || len(payload.Calls) == 0 {
		t.Fatalf("permission payload = %+v, want request id and calls", payload)
	}
	return sink.byName(EventSession)[0].SessionID, payload
}

func TestServeTurnInteractiveInterruptAndResumeAllow(t *testing.T) {
	tools := fetchDocCaller("TOOL OUTPUT MARKER")
	// Recording happens inside the script closure: with tools configured, ADK
	// binds via WithTools and the bound copy does not share the root model's
	// request log.
	var mu sync.Mutex
	var requests [][]*schema.Message
	calls := 0
	model := &fakeChatModel{script: func(input []*schema.Message) *schema.Message {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, input)
		last := input[len(input)-1]
		if last.Role == schema.Tool {
			return schema.AssistantMessage("result: "+last.Content, nil)
		}
		calls++
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID:       fmt.Sprintf("call-%d", calls),
			Type:     "function",
			Function: schema.FunctionCall{Name: "fetch_doc", Arguments: `{"id":"x"}`},
		}})
	}}
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"gated": interactiveAgent("gated")}},
		Models: &fakeModelResolver{model: model},
		Tools:  tools,
	})

	sessionID, payload := suspendOneTurn(t, host, "gated")
	if tools.calls != 0 {
		t.Fatalf("tool calls before decision = %d, want 0", tools.calls)
	}
	call := payload.Calls[0]
	if call.ToolName != "fetch_doc" || call.MCPServiceID != "docs" || call.CallID == "" {
		t.Fatalf("pending call = %+v, want fetch_doc on docs with a call id", call)
	}

	// The suspended turn holds no turn slot and shows up in the runtime view.
	view := host.Runtime()
	if len(view.PendingPermissions) != 1 || view.PendingPermissions[0].RequestID != payload.RequestID {
		t.Fatalf("runtime pending = %+v, want the suspended request", view.PendingPermissions)
	}
	if state := view.Agents["gated"]; state.InflightTurns != 0 {
		t.Fatalf("inflight turns while suspended = %d, want 0", state.InflightTurns)
	}

	// New input on the suspended session is rejected, fail-closed.
	err := host.ServeTurn(t.Context(), "gated", TurnRequest{SessionID: sessionID, Input: "more"}, (&collectedEvents{}).sink)
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), payload.RequestID) {
		t.Fatalf("input on suspended session error = %v, want pending-permission rejection", err)
	}

	resume := &collectedEvents{}
	err = host.ServeTurn(t.Context(), "gated", TurnRequest{
		SessionID: sessionID,
		Permission: &TurnPermission{
			RequestID: payload.RequestID,
			Decisions: []TurnPermissionDecision{{CallID: call.CallID, Outcome: "allow"}},
		},
	}, resume.sink)
	if err != nil {
		t.Fatalf("resume error = %v", err)
	}
	if tools.calls != 1 {
		t.Fatalf("tool calls after allow = %d, want 1", tools.calls)
	}
	dones := resume.byName(EventDone)
	if len(dones) != 1 || dones[0].StopReason != "end_turn" {
		t.Fatalf("resume done = %+v, want end_turn", dones)
	}
	contents := resume.byName(EventContent)
	if len(contents) == 0 || !strings.Contains(contents[len(contents)-1].Text, "TOOL OUTPUT MARKER") {
		t.Fatalf("resume contents = %+v, want the executed tool output echoed", contents)
	}

	// One-shot: the pending entry and checkpoint are gone.
	if view := host.Runtime(); len(view.PendingPermissions) != 0 {
		t.Fatalf("pending after resume = %+v, want none", view.PendingPermissions)
	}
	if _, ok, _ := host.checkpoints.Get(t.Context(), payload.RequestID); ok {
		t.Fatal("checkpoint survived a completed resume")
	}

	// The committed exchange is visible to the next turn on the session (the
	// follow-up interrupts again — interactive mode — but its model input
	// must already carry the completed first exchange).
	next := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "gated", TurnRequest{SessionID: sessionID, Input: "again"}, next.sink); err != nil {
		t.Fatalf("follow-up turn error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	lastInput := requests[len(requests)-1]
	committed := false
	for _, m := range lastInput {
		if m.Role == schema.Assistant && strings.Contains(m.Content, "TOOL OUTPUT MARKER") {
			committed = true
		}
	}
	if !committed {
		t.Fatalf("follow-up model input misses the committed exchange: %+v", messageRoles(lastInput))
	}
}

func TestServeTurnInteractiveDenyAndUnansweredAreRefused(t *testing.T) {
	// The model asks for two parallel tool calls; one is denied explicitly,
	// the other is left unanswered (fail-closed deny).
	var mu sync.Mutex
	rounds := 0
	var toolInputs []*schema.Message
	model := &fakeChatModel{script: func(input []*schema.Message) *schema.Message {
		mu.Lock()
		defer mu.Unlock()
		if input[len(input)-1].Role == schema.Tool {
			for _, m := range input {
				if m.Role == schema.Tool {
					toolInputs = append(toolInputs, m)
				}
			}
			return schema.AssistantMessage("done", nil)
		}
		rounds++
		return schema.AssistantMessage("", []schema.ToolCall{
			{ID: "call-a", Type: "function", Function: schema.FunctionCall{Name: "fetch_doc", Arguments: `{"id":"a"}`}},
			{ID: "call-b", Type: "function", Function: schema.FunctionCall{Name: "fetch_doc", Arguments: `{"id":"b"}`}},
		})
	}}
	tools := fetchDocCaller("SHOULD NOT RUN")
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"gated": interactiveAgent("gated")}},
		Models: &fakeModelResolver{model: model},
		Tools:  tools,
	})

	sessionID, payload := suspendOneTurn(t, host, "gated")
	if len(payload.Calls) != 2 {
		t.Fatalf("pending calls = %+v, want both parallel calls", payload.Calls)
	}

	resume := &collectedEvents{}
	err := host.ServeTurn(t.Context(), "gated", TurnRequest{
		SessionID: sessionID,
		Permission: &TurnPermission{
			RequestID: payload.RequestID,
			Decisions: []TurnPermissionDecision{{CallID: payload.Calls[0].CallID, Outcome: "deny"}},
		},
	}, resume.sink)
	if err != nil {
		t.Fatalf("resume error = %v", err)
	}
	if tools.calls != 0 {
		t.Fatalf("tool calls after deny = %d, want 0", tools.calls)
	}
	dones := resume.byName(EventDone)
	if len(dones) != 1 || dones[0].StopReason != "end_turn" {
		t.Fatalf("resume done = %+v, want end_turn (deny continues the turn)", dones)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(toolInputs) != 2 {
		t.Fatalf("tool messages seen by model = %d, want 2", len(toolInputs))
	}
	for _, m := range toolInputs {
		if !strings.Contains(m.Content, "denied by the operator") {
			t.Fatalf("tool message %q, want the denial text", m.Content)
		}
	}
}

func TestServeTurnPermissionCancelDiscardsTheTurn(t *testing.T) {
	model := gatedToolModel()
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"gated": interactiveAgent("gated")}},
		Models: &fakeModelResolver{model: model},
		Tools:  fetchDocCaller("x"),
	})

	sessionID, payload := suspendOneTurn(t, host, "gated")
	resume := &collectedEvents{}
	err := host.ServeTurn(t.Context(), "gated", TurnRequest{
		SessionID:  sessionID,
		Permission: &TurnPermission{RequestID: payload.RequestID, Outcome: "cancel"},
	}, resume.sink)
	if err != nil {
		t.Fatalf("cancel error = %v", err)
	}
	dones := resume.byName(EventDone)
	if len(dones) != 1 || dones[0].StopReason != StopReasonCancelled {
		t.Fatalf("cancel done = %+v, want stop_reason cancelled", dones)
	}
	if _, ok, _ := host.checkpoints.Get(t.Context(), payload.RequestID); ok {
		t.Fatal("checkpoint survived a cancel")
	}
	// Nothing was committed: the next turn's model input carries no history
	// beyond the fresh user message (plus the system prompt ADK renders).
	if err := host.ServeTurn(t.Context(), "gated", TurnRequest{SessionID: sessionID, Input: "fresh"}, (&collectedEvents{}).sink); err != nil {
		t.Fatalf("post-cancel turn error = %v", err)
	}
}

func TestServeTurnPermissionExpiryFailsClosed(t *testing.T) {
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"gated": interactiveAgent("gated")}},
		Models: &fakeModelResolver{model: gatedToolModel()},
		Tools:  fetchDocCaller("x"),
	})
	sessionID, payload := suspendOneTurn(t, host, "gated")

	base := time.Now()
	nowFunc = func() time.Time { return base.Add(defaultPermissionTimeout + time.Minute) }
	defer func() { nowFunc = time.Now }()

	err := host.ServeTurn(t.Context(), "gated", TurnRequest{
		SessionID:  sessionID,
		Permission: &TurnPermission{RequestID: payload.RequestID},
	}, (&collectedEvents{}).sink)
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "not found or expired") {
		t.Fatalf("expired resume error = %v, want not-found-or-expired rejection", err)
	}
	if _, ok, _ := host.checkpoints.Get(t.Context(), payload.RequestID); ok {
		t.Fatal("checkpoint survived expiry")
	}
	// The session is no longer suspended: new input proceeds.
	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "gated", TurnRequest{SessionID: sessionID, Input: "retry"}, sink.sink); err != nil {
		t.Fatalf("post-expiry turn error = %v", err)
	}
	if dones := sink.byName(EventDone); len(dones) != 1 || dones[0].StopReason != StopReasonPermissionRequired {
		t.Fatalf("post-expiry done = %+v, want a fresh permission interrupt", dones)
	}
}

func TestServeTurnPermissionDefinitionUpdateInvalidates(t *testing.T) {
	source := &fakeAgentSource{agents: map[string]agent.Agent{"gated": interactiveAgent("gated")}}
	host := NewHost(Config{
		Agents: source,
		Models: &fakeModelResolver{model: gatedToolModel()},
		Tools:  fetchDocCaller("x"),
	})
	sessionID, payload := suspendOneTurn(t, host, "gated")

	updated := source.agents["gated"]
	updated.UpdatedAt = updated.UpdatedAt.Add(time.Minute)
	source.agents["gated"] = updated

	err := host.ServeTurn(t.Context(), "gated", TurnRequest{
		SessionID:  sessionID,
		Permission: &TurnPermission{RequestID: payload.RequestID},
	}, (&collectedEvents{}).sink)
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "definition changed") {
		t.Fatalf("resume after definition update error = %v, want invalidation", err)
	}
	if _, ok, _ := host.checkpoints.Get(t.Context(), payload.RequestID); ok {
		t.Fatal("checkpoint survived definition-update invalidation")
	}
}

func TestServeTurnPermissionCapacityFailsClosed(t *testing.T) {
	a := interactiveAgent("gated")
	a.Runtime.Builtin.Permissions.MaxPending = 1
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"gated": a}},
		Models: &fakeModelResolver{model: gatedToolModel()},
		Tools:  fetchDocCaller("x"),
	})
	suspendOneTurn(t, host, "gated")

	sink := &collectedEvents{}
	err := host.ServeTurn(t.Context(), "gated", TurnRequest{Input: "second session"}, sink.sink)
	if !errors.Is(err, ErrPermissionCapacity) {
		t.Fatalf("over-capacity turn error = %v, want ErrPermissionCapacity", err)
	}
	if view := host.Runtime(); len(view.PendingPermissions) != 1 {
		t.Fatalf("pending after capacity rejection = %d, want 1", len(view.PendingPermissions))
	}
}

func TestServeTurnAutoApproveToolsBypassTheGate(t *testing.T) {
	a := interactiveAgent("gated")
	a.Runtime.Builtin.Permissions.AutoApproveTools = []string{"docs/fetch_doc"}
	tools := fetchDocCaller("AUTO OK")
	host := NewHost(Config{
		Agents: &fakeAgentSource{agents: map[string]agent.Agent{"gated": a}},
		Models: &fakeModelResolver{model: gatedToolModel()},
		Tools:  tools,
	})

	sink := &collectedEvents{}
	if err := host.ServeTurn(t.Context(), "gated", TurnRequest{Input: "go"}, sink.sink); err != nil {
		t.Fatalf("auto-approved turn error = %v", err)
	}
	if tools.calls != 1 {
		t.Fatalf("tool calls = %d, want 1 without any interrupt", tools.calls)
	}
	dones := sink.byName(EventDone)
	if len(dones) != 1 || dones[0].StopReason != "end_turn" {
		t.Fatalf("done = %+v, want end_turn", dones)
	}
	if perms := sink.byName(EventPermission); len(perms) != 0 {
		t.Fatalf("permission events = %d, want 0 for an allowlisted tool", len(perms))
	}
}
