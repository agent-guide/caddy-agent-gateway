package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	"github.com/agent-guide/agent-gateway/pkg/agent"
	builtinhost "github.com/agent-guide/agent-gateway/pkg/agent/builtin"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	basemcp "github.com/agent-guide/agent-gateway/pkg/mcp"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
)

type staticACPServices struct{ cfg acpservice.ServiceConfig }

func (s staticACPServices) Get(_ context.Context, id string) (acpservice.ServiceConfig, error) {
	if id != s.cfg.ID {
		return acpservice.ServiceConfig{}, acpservice.ErrServiceNotConfigured
	}
	return s.cfg, nil
}

type captureACPRuntime struct {
	t     *testing.T
	owner string
	cfg   acpruntime.RuntimeConfig
	req   acpruntime.TurnRequest
}

func (r *captureACPRuntime) ServeConfiguredTurn(ctx context.Context, owner string, cfg acpruntime.RuntimeConfig, req acpruntime.TurnRequest, emit acpruntime.EventSink) error {
	r.owner, r.cfg, r.req = owner, cfg, req
	ids, _ := runtimeapi.IdentitiesFromContext(ctx)
	dims, _ := usage.DimensionsFromContext(ctx)
	if ids.AgentID != owner || ids.RunID == "" || dims.RunID != ids.RunID || dims.RuntimeType != agent.RuntimeTypeACP {
		r.t.Fatalf("ACP identity bridge ids=%+v dims=%+v", ids, dims)
	}
	if err := emit(acpruntime.TurnEvent{Event: runtimeapi.EventSession, SessionID: "native-session"}); err != nil {
		return err
	}
	if err := emit(acpruntime.TurnEvent{Event: runtimeapi.EventContent, Text: "answer"}); err != nil {
		return err
	}
	return emit(acpruntime.TurnEvent{Event: runtimeapi.EventDone, StopReason: "end_turn"})
}

func TestACPBackendTranslatesLegacyOptionsIntoIdentityFreeRuntime(t *testing.T) {
	service := acpservice.ServiceConfig{
		ID: "svc-1", Name: "Service", AgentType: "opencode", CWD: "/workspace",
		AllowedRoots: []string{"/workspace"}, DefaultModel: "model-a", Env: map[string]string{"A": "B"},
		ConfigOverrides: map[string]string{"base": "true"}, PermissionMode: "interactive",
	}
	native := &captureACPRuntime{t: t}
	backend := NewACPBackend(staticACPServices{cfg: service}, native)
	a := agent.Agent{ID: "agent-1", Runtime: agent.Runtime{Type: agent.RuntimeTypeACP, ACP: &agent.ACPRuntime{ServiceID: service.ID}}}
	raw := json.RawMessage(`{"thread_id":"thread-1","cwd":"/workspace/repo","model":"model-b","fresh_session":true,"config_overrides":{"turn":"yes"}}`)
	req := runtimeapi.TurnRequest{Input: "hello", SessionID: "session-1", Options: runtimeapi.TurnOptions{Version: runtimeapi.TurnOptionsVersionV1, Runtime: raw}}
	run, err := runtimeapi.NewTurnSequencer(t.Context(), backend, a, req)
	if err != nil {
		t.Fatalf("NewTurnSequencer: %v", err)
	}
	req.RunID = run.RunID()
	ctx := usage.ContextWithDimensions(t.Context(), usage.InteractionDimensions{TraceID: "trace", SpanID: "span", AgentID: a.ID})
	var events []runtimeapi.TurnEvent
	if _, err := run.ServeSegment(ctx, backend, a, req, func(ev runtimeapi.TurnEvent) error { events = append(events, ev); return nil }); err != nil {
		t.Fatalf("ServeSegment: %v", err)
	}
	if native.owner != a.ID || native.cfg.AgentType != service.AgentType || native.cfg.CWD != service.CWD {
		t.Fatalf("native owner/config = %q %+v", native.owner, native.cfg)
	}
	if native.req.ThreadID != "thread-1" || native.req.SessionID != "session-1" || native.req.CWD != "/workspace/repo" || native.req.Model != "model-b" || !native.req.FreshSession || native.req.ConfigOverrides["turn"] != "yes" {
		t.Fatalf("native request = %+v", native.req)
	}
	if len(events) != 3 || events[0].Sequence != 1 || events[2].Event != runtimeapi.EventDone || events[2].RunID != req.RunID {
		t.Fatalf("common events = %+v", events)
	}
	var terminal struct {
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(events[2].Data, &terminal); err != nil || terminal.StopReason != "end_turn" {
		t.Fatalf("terminal data = %s, err=%v", events[2].Data, err)
	}
	service.Env["A"] = "changed"
	if native.cfg.Env["A"] != "B" {
		t.Fatal("runtime config aliases service map")
	}
}

type checkpointBuiltinHost struct {
	t      *testing.T
	runID  string
	cursor builtinhost.ContinuationCursor
}

func (h *checkpointBuiltinHost) LoadContinuationCursor(agentID, requestID string) (builtinhost.ContinuationCursor, bool) {
	return h.cursor, agentID == "a1" && requestID == "perm-1" && h.cursor.RunID != ""
}

func (h *checkpointBuiltinHost) StoreContinuationCursor(agentID, requestID string, cursor builtinhost.ContinuationCursor) bool {
	if agentID != "a1" || requestID != "perm-1" || cursor.RunID == "" {
		return false
	}
	h.cursor = cursor
	return true
}

func (h *checkpointBuiltinHost) ServeTurn(ctx context.Context, agentID string, req builtinhost.TurnRequest, emit builtinhost.EventSink) error {
	h.t.Helper()
	ids, ok := runtimeapi.IdentitiesFromContext(ctx)
	if !ok || ids.AgentID != agentID || ids.RunID != req.RunID || ids.RuntimeType != agent.RuntimeTypeBuiltin {
		h.t.Fatalf("runtime identities = %+v, request = %+v", ids, req)
	}
	dims, ok := usage.DimensionsFromContext(ctx)
	if !ok || dims.AgentID != ids.AgentID || dims.RunID != ids.RunID || dims.RuntimeType != ids.RuntimeType {
		h.t.Fatalf("usage dimensions = %+v, identities = %+v", dims, ids)
	}
	if req.Permission == nil {
		h.runID = req.RunID
		if err := emit(builtinhost.TurnEvent{Event: builtinhost.EventSession, SessionID: "session-1"}); err != nil {
			return err
		}
		if err := emit(builtinhost.TurnEvent{Event: builtinhost.EventPermission, RequestID: "perm-1", Data: json.RawMessage(`{"calls":[]}`)}); err != nil {
			return err
		}
		return emit(builtinhost.TurnEvent{Event: builtinhost.EventDone, RequestID: "perm-1", StopReason: builtinhost.StopReasonPermissionRequired})
	}
	if req.RunID != h.runID || req.Permission.RequestID != "perm-1" {
		h.t.Fatalf("resume request = %+v, want run %q permission perm-1", req, h.runID)
	}
	h.cursor = builtinhost.ContinuationCursor{}
	if err := emit(builtinhost.TurnEvent{Event: builtinhost.EventSession, SessionID: "session-1"}); err != nil {
		return err
	}
	if err := emit(builtinhost.TurnEvent{Event: builtinhost.EventContent, Text: "resumed"}); err != nil {
		return err
	}
	return emit(builtinhost.TurnEvent{Event: builtinhost.EventDone, StopReason: "end_turn"})
}

func TestBuiltinBackendContinuationUsesCommonRunSequencer(t *testing.T) {
	host := &checkpointBuiltinHost{t: t}
	backend := NewBuiltinBackend(host)
	a := agent.Agent{ID: "a1", Name: "A1", Runtime: agent.Runtime{Type: agent.RuntimeTypeBuiltin, Builtin: &agent.BuiltinRuntime{}}}
	ctx := usage.ContextWithDimensions(t.Context(), usage.InteractionDimensions{TraceID: "trace-1", SpanID: "span-1"})
	var events []runtimeapi.TurnEvent
	cursorVisibleAtDone := false
	sink := func(ev runtimeapi.TurnEvent) error {
		events = append(events, ev)
		if ev.Event == runtimeapi.EventDone && ev.RequestID == "perm-1" {
			cursor, err := backend.LoadContinuationCursor(ctx, a, "perm-1")
			cursorVisibleAtDone = err == nil && cursor.RunID == ev.RunID && cursor.NextSequence == ev.Sequence+1 && cursor.NextSegment == ev.SegmentIndex+1
		}
		return nil
	}

	fresh := runtimeapi.TurnRequest{Input: "hello", SessionID: "session-1"}
	run, err := runtimeapi.NewTurnSequencer(ctx, backend, a, fresh)
	if err != nil {
		t.Fatalf("NewTurnSequencer(fresh): %v", err)
	}
	fresh.RunID = run.RunID()
	if _, err := run.ServeSegment(ctx, backend, a, fresh, sink); err != nil {
		t.Fatalf("ServeSegment(fresh): %v", err)
	}

	resume := runtimeapi.TurnRequest{SessionID: "session-1", Permission: &runtimeapi.PermissionDecision{RequestID: "perm-1", Decisions: []runtimeapi.PermissionActionDecision{{ActionID: "call-1", Outcome: "allow"}}}}
	restored, err := runtimeapi.NewTurnSequencer(ctx, backend, a, resume)
	if err != nil {
		t.Fatalf("NewTurnSequencer(resume): %v", err)
	}
	resume.RunID = restored.RunID()
	if _, err := restored.ServeSegment(ctx, backend, a, resume, sink); err != nil {
		t.Fatalf("ServeSegment(resume): %v", err)
	}

	if len(events) != 6 {
		t.Fatalf("events = %d, want 6: %+v", len(events), events)
	}
	if !cursorVisibleAtDone {
		t.Fatal("permission continuation cursor was not visible before done reached the downstream sink")
	}
	for i, ev := range events {
		if ev.RunID != fresh.RunID || ev.Sequence != uint64(i+1) {
			t.Fatalf("event[%d] = %+v", i, ev)
		}
		wantSegment := uint32(0)
		if i >= 3 {
			wantSegment = 1
		}
		if ev.SegmentIndex != wantSegment {
			t.Fatalf("event[%d].segment = %d, want %d", i, ev.SegmentIndex, wantSegment)
		}
	}
	if _, err := runtimeapi.NewTurnSequencer(ctx, backend, a, resume); !errors.Is(err, runtimeapi.ErrPermissionNotFound) {
		t.Fatalf("second resume error = %v, want permission_not_found", err)
	}
}

func TestRuntimeBackendRejectsForeignOptionsAndDisabledAgent(t *testing.T) {
	backend := NewBuiltinBackend(&checkpointBuiltinHost{t: t})
	a := agent.Agent{ID: "a1", Runtime: agent.Runtime{Type: agent.RuntimeTypeBuiltin, Builtin: &agent.BuiltinRuntime{}}}
	err := backend.ServeTurn(t.Context(), a, runtimeapi.TurnRequest{Options: runtimeapi.TurnOptions{Runtime: json.RawMessage(`{"cwd":"/tmp"}`)}}, func(runtimeapi.TurnEvent) error { return nil })
	if !errors.Is(err, runtimeapi.ErrUnsupportedOption) {
		t.Fatalf("foreign option error = %v", err)
	}
	a.Disabled = true
	if _, err := backend.Capabilities(t.Context(), a); !errors.Is(err, runtimeapi.ErrAgentDisabled) {
		t.Fatalf("disabled capabilities error = %v", err)
	}
}

type hitlAgentSource struct{ a agent.Agent }

func (s hitlAgentSource) Get(_ context.Context, id string) (agent.Agent, error) {
	if id != s.a.ID {
		return agent.Agent{}, agent.ErrAgentNotConfigured
	}
	return s.a, nil
}

type hitlModelResolver struct {
	model einomodel.ToolCallingChatModel
}

func (r hitlModelResolver) ResolveChatModel(context.Context, string, string, bool) (einomodel.ToolCallingChatModel, error) {
	return r.model, nil
}

type hitlModel struct {
	mu    sync.Mutex
	calls int
	tools []*schema.ToolInfo
}

func (m *hitlModel) Generate(_ context.Context, input []*schema.Message, _ ...einomodel.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	last := input[len(input)-1]
	if last.Role == schema.Tool {
		return schema.AssistantMessage("result: "+last.Content, nil), nil
	}
	m.calls++
	return schema.AssistantMessage("", []schema.ToolCall{{ID: fmt.Sprintf("call-%d", m.calls), Type: "function", Function: schema.FunctionCall{Name: "fetch_doc", Arguments: `{"id":"x"}`}}}), nil
}

func (m *hitlModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	writer.Send(msg, nil)
	writer.Close()
	return reader, nil
}

func (m *hitlModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	m.tools = tools
	return m, nil
}

type hitlTools struct{ calls int }

func (t *hitlTools) ListTools(context.Context, string) ([]basemcp.Tool, error) {
	return []basemcp.Tool{{Name: "fetch_doc", Description: "Fetch", InputSchema: map[string]any{"type": "object"}}}, nil
}

func (t *hitlTools) CallTool(context.Context, string, string, map[string]any, chan<- mcpservice.UpstreamProgress) (*basemcp.ToolResult, error) {
	t.calls++
	return &basemcp.ToolResult{Content: []any{map[string]any{"type": "text", "text": "tool output"}}}, nil
}

func TestBuiltinBackendRealCheckpointResumePreservesCommonSequenceAndSpanLink(t *testing.T) {
	a := agent.Agent{
		ID: "gated", Name: "Gated", UpdatedAt: time.Now().UTC(),
		Runtime: agent.Runtime{Type: agent.RuntimeTypeBuiltin, Builtin: &agent.BuiltinRuntime{
			Model:       agent.BuiltinModel{LLMRouteID: "chat-main"},
			Topology:    agent.BuiltinTopology{Kind: agent.TopologyKindSingle},
			Tools:       []agent.BuiltinToolSelection{{MCPServiceID: "docs", Tools: []string{"fetch_doc"}}},
			Permissions: &agent.BuiltinPermissions{Mode: agent.PermissionModeInteractive},
		}},
		Routes:    agent.Routes{LLMRouteIDs: []string{"chat-main"}},
		Resources: agent.Resources{MCPServiceIDs: []string{"docs"}},
	}
	usageSink := &usage.InMemorySink{}
	observer := usage.NewObserver(usageSink)
	host := builtinhost.NewHost(builtinhost.Config{
		Agents: hitlAgentSource{a: a}, Models: hitlModelResolver{model: &hitlModel{}},
		Tools: &hitlTools{}, Observer: observer,
	})
	backend := NewBuiltinBackend(host)
	var events []runtimeapi.TurnEvent
	eventSink := func(ev runtimeapi.TurnEvent) error { events = append(events, ev); return nil }

	freshSpan, freshCtx := observer.Begin(t.Context(), usage.InteractionDimensions{
		TraceID: "11111111111111111111111111111111", SpanID: "2222222222222222",
		RouteID: "builtin:gated:turn", RouteKind: "builtin", AgentID: a.ID, RuntimeType: agent.RuntimeTypeBuiltin,
	})
	fresh := runtimeapi.TurnRequest{Input: "do it"}
	run, err := runtimeapi.NewTurnSequencer(freshCtx, backend, a, fresh)
	if err != nil {
		t.Fatalf("fresh sequencer: %v", err)
	}
	fresh.RunID = run.RunID()
	if _, err := run.ServeSegment(freshCtx, backend, a, fresh, eventSink); err != nil {
		t.Fatalf("fresh segment: %v", err)
	}
	freshSpan.Finish(usage.InteractionOutcome{Success: true, StatusCode: 200})
	var permission runtimeapi.TurnEvent
	for _, ev := range events {
		if ev.Event == runtimeapi.EventPermission {
			permission = ev
		}
	}
	if permission.RequestID == "" || permission.SessionID == "" {
		t.Fatalf("permission event = %+v", permission)
	}
	var payload struct {
		Calls []struct {
			CallID string `json:"call_id"`
		} `json:"calls"`
	}
	if err := json.Unmarshal(permission.Data, &payload); err != nil || len(payload.Calls) != 1 {
		t.Fatalf("permission payload = %s, err=%v", permission.Data, err)
	}
	badResume := runtimeapi.TurnRequest{SessionID: permission.SessionID, Permission: &runtimeapi.PermissionDecision{RequestID: permission.RequestID, Outcome: "invalid"}}
	badRun, err := runtimeapi.NewTurnSequencer(freshCtx, backend, a, badResume)
	if err != nil {
		t.Fatalf("bad resume sequencer: %v", err)
	}
	badResume.RunID = badRun.RunID()
	if result, err := badRun.ServeSegment(freshCtx, backend, a, badResume, eventSink); !errors.Is(err, runtimeapi.ErrInvalidRequest) || result.Started {
		t.Fatalf("bad resume result=%+v err=%v, want pre-stream invalid_request", result, err)
	}

	resumeSpan, resumeCtx := observer.Begin(t.Context(), usage.InteractionDimensions{
		TraceID: "33333333333333333333333333333333", SpanID: "4444444444444444",
		RouteID: "builtin:gated:turn", RouteKind: "builtin", AgentID: a.ID, RuntimeType: agent.RuntimeTypeBuiltin,
	})
	resume := runtimeapi.TurnRequest{SessionID: permission.SessionID, Permission: &runtimeapi.PermissionDecision{
		RequestID: permission.RequestID, Decisions: []runtimeapi.PermissionActionDecision{{ActionID: payload.Calls[0].CallID, Outcome: "allow"}},
	}}
	restored, err := runtimeapi.NewTurnSequencer(resumeCtx, backend, a, resume)
	if err != nil {
		t.Fatalf("resume sequencer: %v", err)
	}
	resume.RunID = restored.RunID()
	if _, err := restored.ServeSegment(resumeCtx, backend, a, resume, eventSink); err != nil {
		t.Fatalf("resume segment: %v", err)
	}
	resumeSpan.Finish(usage.InteractionOutcome{Success: true, StatusCode: 200})

	for index, ev := range events {
		if ev.RunID != fresh.RunID || ev.Sequence != uint64(index+1) {
			t.Fatalf("event[%d] = %+v", index, ev)
		}
		if index > 0 && ev.Sequence <= events[index-1].Sequence {
			t.Fatalf("non-monotonic events: %+v", events)
		}
	}
	if events[len(events)-1].SegmentIndex != 1 {
		t.Fatalf("last segment = %d, want 1", events[len(events)-1].SegmentIndex)
	}
	var resumed usage.BuiltinUsageEvent
	inner := map[string]bool{}
	for _, raw := range usageSink.Events {
		switch ev := raw.(type) {
		case usage.BuiltinUsageEvent:
			if ev.Operation == "resume" {
				resumed = ev
			}
		case usage.LLMUsageEvent:
			if ev.TraceID == "33333333333333333333333333333333" && ev.RunID == fresh.RunID {
				inner["llm"] = true
			}
		case usage.MCPUsageEvent:
			if ev.TraceID == "33333333333333333333333333333333" && ev.RunID == fresh.RunID {
				inner["mcp"] = true
			}
		}
	}
	if resumed.RunID != fresh.RunID || resumed.LinkTraceID != "11111111111111111111111111111111" || resumed.LinkSpanID != "2222222222222222" {
		t.Fatalf("resumed usage = %+v", resumed)
	}
	if !inner["llm"] || !inner["mcp"] {
		t.Fatalf("resumed inner usage = %v", inner)
	}
}
