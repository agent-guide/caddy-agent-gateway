package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	"github.com/agent-guide/agent-gateway/pkg/acp/runtimeconfig"
	"github.com/agent-guide/agent-gateway/pkg/agent"
	builtinhost "github.com/agent-guide/agent-gateway/pkg/agent/builtin"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	basemcp "github.com/agent-guide/agent-gateway/pkg/mcp"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
)

func inlineACPRuntime(cfg runtimeconfig.Config) *agent.ACPRuntime {
	return &agent.ACPRuntime{
		AgentType: cfg.AgentType, CWD: cfg.CWD, AllowedRoots: append([]string(nil), cfg.AllowedRoots...),
		DefaultModel: cfg.DefaultModel, Env: cloneStringMap(cfg.Env), ConfigOverrides: cloneStringMap(cfg.ConfigOverrides),
		IdleTTL: cfg.IdleTTL, MaxInstances: cfg.MaxInstances, PermissionMode: cfg.PermissionMode, Codex: cloneCodexConfig(cfg.Codex),
	}
}

type captureACPRuntime struct {
	t     *testing.T
	owner string
	cfg   acpruntime.RuntimeConfig
	req   acpruntime.TurnRequest
}

type preNativeRegistrationACPRuntime struct {
	entered chan struct{}
	release chan struct{}
}

func (r *preNativeRegistrationACPRuntime) ServeConfiguredTurn(context.Context, string, acpruntime.RuntimeConfig, acpruntime.TurnRequest, acpruntime.EventSink) error {
	close(r.entered)
	<-r.release
	return nil
}

func (*preNativeRegistrationACPRuntime) CancelRun(string, string) error {
	return acpruntime.ErrRunNotFound
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

func TestACPBackendTranslatesOptionsIntoIdentityFreeRuntime(t *testing.T) {
	config := runtimeconfig.Config{
		AgentType: "opencode", CWD: "/workspace",
		AllowedRoots: []string{"/workspace"}, DefaultModel: "model-a", Env: map[string]string{"A": "B"},
		ConfigOverrides: map[string]string{"base": "true"}, PermissionMode: "interactive",
	}
	native := &captureACPRuntime{t: t}
	backend := NewACPBackend(native)
	a := agent.Agent{ID: "agent-1", Runtime: agent.Runtime{Type: agent.RuntimeTypeACP, ACP: inlineACPRuntime(config)}}
	backend.RefreshRuntimeConfigs(t.Context(), []agent.Agent{a})
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
	if native.owner != a.ID || native.cfg.AgentType != config.AgentType || native.cfg.CWD != config.CWD {
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
	config.Env["A"] = "changed"
	if native.cfg.Env["A"] != "B" {
		t.Fatal("runtime config aliases Agent definition map")
	}
	if _, err := backend.Capabilities(t.Context(), a); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
}

func TestACPBackendCancelBeforeNativeRegistrationIsRetryable(t *testing.T) {
	config := runtimeconfig.Config{AgentType: "opencode", CWD: "/workspace", AllowedRoots: []string{"/workspace"}}
	native := &preNativeRegistrationACPRuntime{entered: make(chan struct{}), release: make(chan struct{})}
	backend := NewACPBackend(native, RuntimeControls{Runs: runtimeapi.NewRunRegistry()})
	a := agent.Agent{ID: "agent-1", Runtime: agent.Runtime{Type: agent.RuntimeTypeACP, ACP: inlineACPRuntime(config)}}
	backend.RefreshRuntimeConfigs(t.Context(), []agent.Agent{a})
	runID := "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	done := make(chan error, 1)
	go func() {
		done <- backend.ServeTurn(t.Context(), a, runtimeapi.TurnRequest{
			RunID:   runID,
			Input:   "hello",
			Options: runtimeapi.TurnOptions{Version: runtimeapi.TurnOptionsVersionV1, Runtime: json.RawMessage(`{"thread_id":"thread-1"}`)},
		}, func(runtimeapi.TurnEvent) error { return nil })
	}()
	select {
	case <-native.entered:
	case <-time.After(time.Second):
		t.Fatal("ServeConfiguredTurn was not entered")
	}
	_, err := backend.CancelRun(t.Context(), a, runtimeapi.CancelRequest{RunID: runID, Mode: runtimeapi.CancelModeForce})
	if !errors.Is(err, runtimeapi.ErrBackendUnavailable) {
		t.Fatalf("pre-native cancellation error=%v, want backend_unavailable", err)
	}
	close(native.release)
	if err := <-done; err != nil {
		t.Fatalf("ServeTurn: %v", err)
	}
}

type checkpointBuiltinHost struct {
	t       *testing.T
	runID   string
	cursor  builtinhost.ContinuationCursor
	expired []string
}

type cancelOrderBuiltinHost struct {
	*checkpointBuiltinHost
	order []string
}

func (h *cancelOrderBuiltinHost) ExpirePermission(agentID, requestID string) bool {
	h.order = append(h.order, "expire")
	return h.checkpointBuiltinHost.ExpirePermission(agentID, requestID)
}

func (h *cancelOrderBuiltinHost) CancelRun(string, string, builtinhost.CancelMode) (bool, error) {
	h.order = append(h.order, "cancel")
	return false, nil
}

func (h *checkpointBuiltinHost) ExpirePermission(agentID, requestID string) bool {
	h.expired = append(h.expired, agentID+":"+requestID)
	return true
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
		if err := emit(builtinhost.TurnEvent{Event: builtinhost.EventPermission, RequestID: "perm-1", Data: json.RawMessage(`{"calls":[{"call_id":"call-1","name":"fetch_doc"}]}`)}); err != nil {
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
	broker := runtimeapi.NewPermissionBroker()
	t.Cleanup(func() { broker.Close(runtimeapi.WithPermissionSource(context.Background(), "test_cleanup")) })
	backend := NewBuiltinBackend(host, RuntimeControls{Runs: runtimeapi.NewRunRegistry(), Permissions: broker})
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

func TestBuiltinExactRunCancelDrainsPermissionBeforeNativeCancel(t *testing.T) {
	host := &cancelOrderBuiltinHost{checkpointBuiltinHost: &checkpointBuiltinHost{t: t}}
	broker := runtimeapi.NewPermissionBroker()
	t.Cleanup(func() { broker.Close(runtimeapi.WithPermissionSource(context.Background(), "test_cleanup")) })
	backend := NewBuiltinBackend(host, RuntimeControls{Runs: runtimeapi.NewRunRegistry(), Permissions: broker})
	a := agent.Agent{ID: "a1", Runtime: agent.Runtime{Type: agent.RuntimeTypeBuiltin, Builtin: &agent.BuiltinRuntime{}}}
	runID := "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ctx := builtinBackendTestContext(t.Context(), a.ID, runID)
	if err := backend.ServeTurn(ctx, a, runtimeapi.TurnRequest{RunID: runID, SessionID: "session-1", Input: "hello"}, func(runtimeapi.TurnEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	result, err := backend.CancelRun(t.Context(), a, runtimeapi.CancelRequest{RunID: runID, Mode: runtimeapi.CancelModeForce})
	if err != nil || result.State != runtimeapi.RunStateCancelled {
		t.Fatalf("cancel result=%+v err=%v", result, err)
	}
	if got := strings.Join(host.order, ","); got != "expire,cancel" {
		t.Fatalf("cancel order=%q, want expire,cancel", got)
	}
}

func TestBuiltinAdminDecisionKeepsExpiryAndFailsClosed(t *testing.T) {
	backend := NewBuiltinBackend(&checkpointBuiltinHost{t: t}, RuntimeControls{Permissions: runtimeapi.NewPermissionBroker()})
	t.Cleanup(func() {
		backend.permissions.Close(runtimeapi.WithPermissionSource(context.Background(), "test_cleanup"))
	})
	a := agent.Agent{ID: "a1", Runtime: agent.Runtime{Type: agent.RuntimeTypeBuiltin, Builtin: &agent.BuiltinRuntime{}}}
	decision := runtimeapi.PermissionDecision{RequestID: "perm-expired", Decisions: []runtimeapi.PermissionActionDecision{{ActionID: "call-1", Outcome: "allow"}}}
	backend.storeBuiltinContinuation("cont-expired", builtinPermissionContinuation{agentID: a.ID, runID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", requestID: decision.RequestID})
	if err := backend.ResolveContinuation(t.Context(), "cont-expired", decision, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	err := backend.ServeTurn(t.Context(), a, runtimeapi.TurnRequest{RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Permission: &runtimeapi.PermissionDecision{RequestID: decision.RequestID}}, func(runtimeapi.TurnEvent) error { return nil })
	if !errors.Is(err, runtimeapi.ErrPermissionExpired) {
		t.Fatalf("expired decided continuation error=%v", err)
	}
	if len(backend.decided) != 0 {
		t.Fatalf("expired decided entries=%+v", backend.decided)
	}
}

func TestBuiltinPermissionShapeIsValidatedBeforeBrokerClaim(t *testing.T) {
	host := &checkpointBuiltinHost{t: t}
	broker := runtimeapi.NewPermissionBroker()
	t.Cleanup(func() { broker.Close(runtimeapi.WithPermissionSource(context.Background(), "test_cleanup")) })
	backend := NewBuiltinBackend(host, RuntimeControls{Permissions: broker})
	runID := "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	info := runtimeapi.PendingPermission{
		RequestID: "perm-1", AgentID: "a1", RuntimeType: agent.RuntimeTypeBuiltin, RunID: runID,
		Actions: []runtimeapi.PermissionAction{{ActionID: "call-1"}}, ExpiresAt: time.Now().Add(time.Minute),
	}
	backend.storeBuiltinContinuation("cont-1", builtinPermissionContinuation{agentID: info.AgentID, runID: runID, requestID: info.RequestID})
	if _, err := broker.Register(info, "cont-1", backend); err != nil {
		t.Fatal(err)
	}
	err := broker.Resolve(t.Context(), info.AgentID, runtimeapi.PermissionDecision{
		RequestID: info.RequestID, Decisions: []runtimeapi.PermissionActionDecision{{ActionID: "unknown", Outcome: "allow"}},
	})
	if !errors.Is(err, runtimeapi.ErrInvalidRequest) || len(broker.List(info.AgentID)) != 1 || len(backend.decided) != 0 {
		t.Fatalf("invalid resolve: err=%v pending=%d decided=%+v", err, len(broker.List(info.AgentID)), backend.decided)
	}
	if err := broker.Resolve(t.Context(), info.AgentID, runtimeapi.PermissionDecision{
		RequestID: info.RequestID, Decisions: []runtimeapi.PermissionActionDecision{{ActionID: "call-1", Outcome: "allow"}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(broker.List(info.AgentID)) != 0 || backend.decided[info.RequestID].runID != runID {
		t.Fatalf("valid resolve: pending=%d decided=%+v", len(broker.List(info.AgentID)), backend.decided)
	}
}

func TestBuiltinDecidedPermissionIsScopedToRun(t *testing.T) {
	host := &checkpointBuiltinHost{t: t}
	broker := runtimeapi.NewPermissionBroker()
	t.Cleanup(func() { broker.Close(runtimeapi.WithPermissionSource(context.Background(), "test_cleanup")) })
	backend := NewBuiltinBackend(host, RuntimeControls{Permissions: broker})
	a := agent.Agent{ID: "a1", Runtime: agent.Runtime{Type: agent.RuntimeTypeBuiltin, Builtin: &agent.BuiltinRuntime{}}}
	backend.decided["perm-1"] = decidedPermission{
		agentID: a.ID, runID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		decision:  runtimeapi.PermissionDecision{RequestID: "perm-1", Outcome: "cancel"},
		expiresAt: time.Now().Add(time.Minute),
	}
	err := backend.ServeTurn(t.Context(), a, runtimeapi.TurnRequest{
		RunID:      "run-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Permission: &runtimeapi.PermissionDecision{RequestID: "perm-1"},
	}, func(runtimeapi.TurnEvent) error { return nil })
	if !errors.Is(err, runtimeapi.ErrPermissionNotFound) || len(backend.decided) != 1 {
		t.Fatalf("cross-run resume: err=%v decided=%+v", err, backend.decided)
	}
}

func TestBuiltinResumeMapsMissingRunToPermissionNotFound(t *testing.T) {
	host := &checkpointBuiltinHost{t: t}
	backend := NewBuiltinBackend(host, RuntimeControls{Runs: runtimeapi.NewRunRegistry(), Permissions: runtimeapi.NewPermissionBroker()})
	a := agent.Agent{ID: "a1", Runtime: agent.Runtime{Type: agent.RuntimeTypeBuiltin, Builtin: &agent.BuiltinRuntime{}}}
	runID := "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	backend.decided["perm-lost"] = decidedPermission{
		agentID: a.ID, runID: runID,
		decision:  runtimeapi.PermissionDecision{RequestID: "perm-lost", Outcome: "cancel"},
		expiresAt: time.Now().Add(time.Minute),
	}
	err := backend.ServeTurn(t.Context(), a, runtimeapi.TurnRequest{
		RunID: runID, Permission: &runtimeapi.PermissionDecision{RequestID: "perm-lost"},
	}, func(runtimeapi.TurnEvent) error { return nil })
	if !errors.Is(err, runtimeapi.ErrPermissionNotFound) {
		t.Fatalf("missing run resume error=%v", err)
	}
	if len(host.expired) != 1 || host.expired[0] != "a1:perm-lost" {
		t.Fatalf("expired continuations=%v", host.expired)
	}
	audits := backend.permissions.Audits(a.ID)
	if len(audits) != 1 || audits[0].Result != "continuation_lost" || audits[0].Source != "builtin_resume" {
		t.Fatalf("continuation audits=%+v", audits)
	}
}

func TestACPSummaryAndHealthDoNotReadServiceStore(t *testing.T) {
	backend := NewACPBackend(&captureACPRuntime{})
	a := agent.Agent{ID: "a1", Runtime: agent.Runtime{Type: agent.RuntimeTypeACP, ACP: &agent.ACPRuntime{AgentType: "codex", CWD: "/workspace"}}}
	if _, err := backend.RuntimeSummary(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Health(t.Context(), a); err != nil {
		t.Fatal(err)
	}
}

func TestACPPermissionActionsAreNormalized(t *testing.T) {
	payload := json.RawMessage(`{"toolCall":{"toolCallId":"tc-1","title":"Write file","rawInput":{"path":"a.txt"}},"options":[{"optionId":"allow-once","kind":"allow_once","name":"Allow once"}]}`)
	actions := acpPermissionActions(payload)
	if len(actions) != 1 || actions[0].ActionID != "tc-1" || actions[0].Name != "Write file" {
		t.Fatalf("actions=%+v", actions)
	}
	options := acpPermissionOptions(payload)
	if len(options) != 1 || options[0].OptionID != "allow-once" || options[0].Kind != "allow_once" || options[0].Name != "Allow once" {
		t.Fatalf("options=%+v", options)
	}
	raw, err := json.Marshal(struct {
		Actions []runtimeapi.PermissionAction `json:"actions"`
		Options []runtimeapi.PermissionOption `json:"options"`
	}{actions, options})
	if err != nil || strings.Contains(string(raw), "rawInput") || strings.Contains(string(raw), "a.txt") {
		t.Fatalf("normalized actions leaked native input: %s, err=%v", raw, err)
	}
}

type permissionResolverStub struct {
	calls int
	err   error
}

func (r *permissionResolverStub) ResolvePermission(acpruntime.PermissionDecision) error {
	r.calls++
	return r.err
}

func TestACPLostContinuationUsesSpecificAuditResult(t *testing.T) {
	broker := runtimeapi.NewPermissionBroker()
	t.Cleanup(func() { broker.Close(runtimeapi.WithPermissionSource(context.Background(), "test_cleanup")) })
	native := &permissionResolverStub{err: acpruntime.ErrPermissionNotFound}
	backend := NewACPBackend(nil)
	info := runtimeapi.PendingPermission{RequestID: "perm-lost", AgentID: "a1", RuntimeType: agent.RuntimeTypeACP, RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExpiresAt: time.Now().Add(time.Minute)}
	backend.storeACPContinuation("cont-lost", acpPermissionContinuation{runtime: native, requestID: info.RequestID})
	if _, err := broker.Register(info, "cont-lost", backend); err != nil {
		t.Fatal(err)
	}
	err := broker.Resolve(runtimeapi.WithPermissionSource(t.Context(), "agent_admin"), info.AgentID, runtimeapi.PermissionDecision{RequestID: info.RequestID, Outcome: "cancelled"})
	if !errors.Is(err, runtimeapi.ErrPermissionNotFound) {
		t.Fatalf("lost continuation error = %v", err)
	}
	audits := broker.Audits(info.AgentID)
	if len(audits) != 1 || audits[0].Result != "continuation_lost" || audits[0].Source != "agent_admin" {
		t.Fatalf("lost continuation audits = %+v", audits)
	}
}

func TestBuiltinPermissionRegistrationFailureRollsBackNativeContinuation(t *testing.T) {
	host := &checkpointBuiltinHost{t: t}
	broker := runtimeapi.NewPermissionBroker()
	broker.Close(runtimeapi.WithPermissionSource(context.Background(), "test_setup"))
	backend := NewBuiltinBackend(host, RuntimeControls{Permissions: broker})
	a := agent.Agent{ID: "a1", Runtime: agent.Runtime{Type: agent.RuntimeTypeBuiltin, Builtin: &agent.BuiltinRuntime{}}}
	runID := "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ctx := builtinBackendTestContext(t.Context(), a.ID, runID)
	err := backend.ServeTurn(ctx, a, runtimeapi.TurnRequest{RunID: runID, SessionID: "session-1", Input: "hello"}, func(runtimeapi.TurnEvent) error { return nil })
	if !errors.Is(err, runtimeapi.ErrBackendUnavailable) {
		t.Fatalf("registration failure error = %v", err)
	}
	if len(host.expired) != 1 || host.expired[0] != "a1:perm-1" || len(backend.continuations) != 0 {
		t.Fatalf("rollback: expired=%v continuations=%v", host.expired, backend.continuations)
	}
}

func TestBuiltinPermissionDeliveryFailureRollsBackBothStores(t *testing.T) {
	host := &checkpointBuiltinHost{t: t}
	broker := runtimeapi.NewPermissionBroker()
	t.Cleanup(func() { broker.Close(runtimeapi.WithPermissionSource(context.Background(), "test_cleanup")) })
	backend := NewBuiltinBackend(host, RuntimeControls{Permissions: broker})
	a := agent.Agent{ID: "a1", Runtime: agent.Runtime{Type: agent.RuntimeTypeBuiltin, Builtin: &agent.BuiltinRuntime{}}}
	runID := "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ctx := builtinBackendTestContext(t.Context(), a.ID, runID)
	deliveryErr := errors.New("client disconnected")
	err := backend.ServeTurn(ctx, a, runtimeapi.TurnRequest{RunID: runID, SessionID: "session-1", Input: "hello"}, func(ev runtimeapi.TurnEvent) error {
		if ev.Event == runtimeapi.EventPermission {
			return deliveryErr
		}
		return nil
	})
	if !errors.Is(err, deliveryErr) {
		t.Fatalf("delivery failure error = %v", err)
	}
	if len(host.expired) != 1 || len(broker.List(a.ID)) != 0 || len(backend.continuations) != 0 {
		t.Fatalf("delivery rollback: expired=%v pending=%v continuations=%v", host.expired, broker.List(a.ID), backend.continuations)
	}
}

func TestAgentGatewayResetDrainsPendingAndDecidedPermissions(t *testing.T) {
	g := NewAgentGateway()
	oldBroker := g.permissionBroker
	host := &checkpointBuiltinHost{t: t}
	backend := NewBuiltinBackend(host, RuntimeControls{Permissions: oldBroker})
	if err := g.runtimeRegistry.Register(backend); err != nil {
		t.Fatal(err)
	}
	runID := "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	backend.storeBuiltinContinuation("cont-pending", builtinPermissionContinuation{agentID: "a1", runID: runID, requestID: "perm-pending"})
	if _, err := oldBroker.Register(runtimeapi.PendingPermission{RequestID: "perm-pending", AgentID: "a1", RuntimeType: agent.RuntimeTypeBuiltin, RunID: runID, ExpiresAt: time.Now().Add(time.Hour)}, "cont-pending", backend); err != nil {
		t.Fatal(err)
	}
	backend.decided["perm-decided"] = decidedPermission{agentID: "a1", runID: runID, decision: runtimeapi.PermissionDecision{RequestID: "perm-decided"}, expiresAt: time.Now().Add(time.Hour)}

	g.Reset()
	if len(host.expired) != 2 || len(oldBroker.List("a1")) != 0 {
		t.Fatalf("reset cleanup: expired=%v pending=%v", host.expired, oldBroker.List("a1"))
	}
	if _, err := oldBroker.Register(runtimeapi.PendingPermission{RequestID: "perm-late", AgentID: "a1", RuntimeType: agent.RuntimeTypeBuiltin, RunID: runID, ExpiresAt: time.Now().Add(time.Hour)}, "cont-late", backend); !errors.Is(err, runtimeapi.ErrBackendUnavailable) {
		t.Fatalf("old broker accepted late registration: %v", err)
	}
	audits := oldBroker.Audits("a1")
	if len(audits) != 1 || audits[0].Source != "process_shutdown" || audits[0].Result != "cancelled" {
		t.Fatalf("reset audits = %+v", audits)
	}
	if g.permissionBroker == oldBroker {
		t.Fatal("reset did not install a fresh broker")
	}
	g.Close()
}

func builtinBackendTestContext(ctx context.Context, agentID, runID string) context.Context {
	ctx = runtimeapi.WithIdentities(ctx, runtimeapi.Identities{AgentID: agentID, RuntimeType: agent.RuntimeTypeBuiltin, RunID: runID})
	return usage.ContextWithDimensions(ctx, usage.InteractionDimensions{AgentID: agentID, RuntimeType: agent.RuntimeTypeBuiltin, RunID: runID})
}

func TestACPPermissionShapeIsValidatedBeforeBrokerClaim(t *testing.T) {
	broker := runtimeapi.NewPermissionBroker()
	t.Cleanup(func() { broker.Close(runtimeapi.WithPermissionSource(context.Background(), "test_cleanup")) })
	native := &permissionResolverStub{}
	backend := NewACPBackend(nil)
	info := runtimeapi.PendingPermission{
		RequestID: "perm-acp", AgentID: "a1", RuntimeType: agent.RuntimeTypeACP,
		RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExpiresAt: time.Now().Add(time.Minute),
		Options: []runtimeapi.PermissionOption{{OptionID: "allow-once", Kind: "allow_once", Name: "Allow once"}},
	}
	backend.storeACPContinuation("cont-acp", acpPermissionContinuation{runtime: native, requestID: info.RequestID})
	if _, err := broker.Register(info, "cont-acp", backend); err != nil {
		t.Fatal(err)
	}
	err := broker.Resolve(runtimeapi.WithPermissionSource(t.Context(), "test"), "a1", runtimeapi.PermissionDecision{RequestID: info.RequestID, Outcome: "selected", OptionID: "not-advertised"})
	if !errors.Is(err, runtimeapi.ErrInvalidRequest) || native.calls != 0 || len(broker.List("a1")) != 1 {
		t.Fatalf("invalid resolve: err=%v calls=%d pending=%d", err, native.calls, len(broker.List("a1")))
	}
	if err := broker.Resolve(t.Context(), "a1", runtimeapi.PermissionDecision{RequestID: info.RequestID, Outcome: "selected", OptionID: "allow-once"}); err != nil {
		t.Fatal(err)
	}
	if native.calls != 1 || len(broker.List("a1")) != 0 {
		t.Fatalf("valid resolve: calls=%d pending=%d", native.calls, len(broker.List("a1")))
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
	broker := runtimeapi.NewPermissionBroker()
	t.Cleanup(func() { broker.Close(runtimeapi.WithPermissionSource(context.Background(), "test_cleanup")) })
	backend := NewBuiltinBackend(host, RuntimeControls{Runs: runtimeapi.NewRunRegistry(), Permissions: broker})
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

type retireRecordingACPRuntime struct {
	captureACPRuntime
	retirements []string
}

type expiryRecordingResolver struct {
	mu      sync.Mutex
	expired int
}

func (*expiryRecordingResolver) ValidateContinuationDecision(string, runtimeapi.PendingPermission, runtimeapi.PermissionDecision) error {
	return nil
}

func (*expiryRecordingResolver) ResolveContinuation(context.Context, string, runtimeapi.PermissionDecision, time.Time) error {
	return nil
}

func (r *expiryRecordingResolver) ExpireContinuation(context.Context, string) error {
	r.mu.Lock()
	r.expired++
	r.mu.Unlock()
	return nil
}

func (r *expiryRecordingResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expired
}

func (r *retireRecordingACPRuntime) RetireOwner(ownerID, keep string) int {
	r.retirements = append(r.retirements, ownerID+"|"+keep)
	return 0
}

func (r *retireRecordingACPRuntime) RetireOwnerDeferred(ownerID, keep string) (int, func()) {
	r.retirements = append(r.retirements, ownerID+"|"+keep)
	return 0, func() {}
}

func TestACPRefreshRuntimeConfigsIsAtomicAndRetiresFingerprints(t *testing.T) {
	config := runtimeconfig.Config{
		AgentType: "opencode", CWD: "/workspace",
		AllowedRoots: []string{"/workspace"}, DefaultModel: "model-a",
	}
	native := &retireRecordingACPRuntime{captureACPRuntime: captureACPRuntime{t: t}}
	permissions := runtimeapi.NewPermissionBroker()
	defer permissions.Close(context.Background())
	backend := NewACPBackend(native, RuntimeControls{Permissions: permissions})
	a := agent.Agent{ID: "agent-1", Runtime: agent.Runtime{Type: agent.RuntimeTypeACP, ACP: inlineACPRuntime(config)}}

	backend.RefreshRuntimeConfigs(t.Context(), []agent.Agent{a})
	if len(native.retirements) != 1 {
		t.Fatalf("initial refresh did not establish the accepted owner fingerprint: %v", native.retirements)
	}
	entry, err := backend.agentRuntimeConfig(a.ID)
	if err != nil || entry.config.DefaultModel != "model-a" {
		t.Fatalf("snapshot entry = %+v, err=%v", entry, err)
	}

	// Mutating the caller-owned Agent object without a definition refresh must
	// not alias or change the published snapshot.
	a.Runtime.ACP.DefaultModel = "model-b"
	permissionResolver := &expiryRecordingResolver{}
	if _, err := permissions.Register(runtimeapi.PendingPermission{
		RequestID: "perm-1", AgentID: a.ID, RuntimeType: agent.RuntimeTypeACP,
		RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TTL: time.Minute,
	}, "cont-1", permissionResolver); err != nil {
		t.Fatalf("register permission: %v", err)
	}
	stale, err := backend.agentRuntimeConfig(a.ID)
	if err != nil || stale.config.DefaultModel != "model-a" {
		t.Fatalf("snapshot changed without refresh: %+v, err=%v", stale, err)
	}

	// One refresh atomically replaces the snapshot and retires the prior
	// fingerprint, keeping only the new one.
	backend.RefreshRuntimeConfigs(t.Context(), []agent.Agent{a})
	fresh, err := backend.agentRuntimeConfig(a.ID)
	if err != nil || fresh.config.DefaultModel != "model-b" {
		t.Fatalf("snapshot after refresh = %+v, err=%v", fresh, err)
	}
	if len(native.retirements) != 2 || native.retirements[1] != "agent-1|"+fresh.fingerprint {
		t.Fatalf("retirements = %v, want refreshed keep=%s", native.retirements, fresh.fingerprint)
	}
	if got := permissions.List(a.ID); len(got) != 0 || permissionResolver.count() != 1 {
		t.Fatalf("retired fingerprint left permission claimable: pending=%v expired=%d", got, permissionResolver.count())
	}

	// Disabled is management state rather than part of RuntimeConfig, but its
	// transition still retires every Agent-owned instance and rejects execution.
	a.Disabled = true
	backend.RefreshRuntimeConfigs(t.Context(), []agent.Agent{a})
	if _, err := backend.Capabilities(t.Context(), a); !errors.Is(err, runtimeapi.ErrAgentDisabled) {
		t.Fatalf("disabled capabilities error = %v", err)
	}
	if len(native.retirements) != 3 || native.retirements[2] != "agent-1|" {
		t.Fatalf("disabled retirements = %v, want retire-all", native.retirements)
	}

	// Invalid inline config remains diagnosable while accepting no fingerprint.
	a.Disabled = false
	a.Runtime.ACP = &agent.ACPRuntime{}
	backend.RefreshRuntimeConfigs(t.Context(), []agent.Agent{a})
	summary, err := backend.RuntimeSummary(t.Context(), a)
	if err != nil || summary.Healthy || summary.State != runtimeapi.RuntimeStateUnhealthy || !strings.Contains(string(summary.Details), "config_missing") {
		t.Fatalf("missing-config summary = %+v, err=%v", summary, err)
	}
	if len(native.retirements) != 4 || native.retirements[3] != "agent-1|" {
		t.Fatalf("missing-config retirements = %v, want retire-all", native.retirements)
	}

	// Removing the agent retires every owner instance and drops the entry.
	backend.RefreshRuntimeConfigs(t.Context(), nil)
	if _, err := backend.agentRuntimeConfig(a.ID); !errors.Is(err, runtimeapi.ErrBackendUnavailable) {
		t.Fatalf("removed agent config error = %v, want backend_unavailable", err)
	}
	if len(native.retirements) != 5 || native.retirements[4] != "agent-1|" {
		t.Fatalf("retirements = %v, want trailing retire-all", native.retirements)
	}
}
