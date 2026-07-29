package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	baseacp "github.com/agent-guide/agent-gateway/pkg/acp"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi/runtimeapitest"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	configschema "github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	configstoresqlite "github.com/agent-guide/agent-gateway/pkg/configstore/sqlite"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	"github.com/agent-guide/agent-gateway/pkg/gateway/agentroute"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

type agentHandlerCapabilityBackend struct {
	*runtimeapitest.Backend
	permissionCalls []runtimeapi.PermissionDecision
	transcriptCalls []runtimeapi.TranscriptRequest
}

func (b *agentHandlerCapabilityBackend) ResolvePermission(_ context.Context, _ agentpkg.Agent, decision runtimeapi.PermissionDecision) error {
	b.permissionCalls = append(b.permissionCalls, decision)
	return nil
}

func (b *agentHandlerCapabilityBackend) LoadTranscript(_ context.Context, _ agentpkg.Agent, req runtimeapi.TranscriptRequest) (runtimeapi.TranscriptResponse, error) {
	b.transcriptCalls = append(b.transcriptCalls, req)
	return runtimeapi.TranscriptResponse{
		SessionID: req.SessionID,
		Messages:  []runtimeapi.TranscriptMessage{{Role: "assistant", Text: "saved reply"}},
	}, nil
}

func createAgentRoute(t *testing.T, gw *gateway.AgentGateway, agentID, pathPrefix string, requireVirtualKey bool) string {
	t.Helper()
	cfg := agentroute.AgentRouteConfig{
		AgentRouteBaseConfig: agentroute.AgentRouteBaseConfig{
			MatchPolicy: agentroute.RouteMatch{PathPrefix: pathPrefix},
			AuthPolicy:  agentroute.RouteAuthPolicy{RequireVirtualKey: requireVirtualKey},
		},
		AgentID: agentID,
	}
	cfg.Normalize()
	stored, err := cfg.ToConfig()
	if err != nil {
		t.Fatalf("agent route ToConfig: %v", err)
	}
	if err := gw.AgentRouteResolver().CreateConfig(t.Context(), stored, "test"); err != nil {
		t.Fatalf("create agent route: %v", err)
	}
	return cfg.ID
}

// TestDispatchAgentRouteEndToEnd covers the M4 AgentRoute acceptance surface:
// one kind=agent route executes a builtin Agent through the common envelope,
// keeps its route id/URL/VirtualKey when the Agent's runtime type changes,
// fails closed for disabled and non-executable targets without invoking a
// backend, and rejects unknown wire fields with unsupported_option.
func TestDispatchAgentRouteEndToEnd(t *testing.T) {
	ctx := t.Context()
	backend, err := configstore.OpenBackend(ctx, "sqlite", configstoresqlite.Config{SQLitePath: t.TempDir() + "/config.db"}, nil)
	if err != nil {
		t.Fatalf("open sqlite backend: %v", err)
	}
	if err := configschema.RegisterDefaultStores(backend); err != nil {
		t.Fatalf("register default stores: %v", err)
	}

	llmRoute := llmroutepkg.LLMRoute{
		AgentRouteConfig: llmroutepkg.AgentRouteConfig{
			ID:          "chat-main",
			Kind:        routecore.RouteKindLLM,
			Protocol:    llmroutepkg.RouteProtocolOpenAI,
			MatchPolicy: llmroutepkg.RouteMatchPolicy{PathPrefix: "/v1"},
		},
		TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
			ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "fake"},
		},
	}
	llmRouteCfg, err := llmRoute.ToConfig()
	if err != nil {
		t.Fatalf("llm route ToConfig: %v", err)
	}

	sink := &builtinCaptureSink{}
	gw := gateway.NewAgentGateway()
	if err := gw.Bootstrap(ctx, gateway.BootstrapOptions{
		StaticLLMRoutes:    []routecore.AgentRouteConfig{llmRouteCfg},
		StaticProviders:    map[string]provider.Provider{"fake": builtinFakeProvider{}},
		ConfigStoreBackend: backend,
		UsageObserver:      usage.NewObserver(sink),
	}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if err := gw.AgentManager().Create(ctx, agentpkg.Agent{
		ID:   "unified",
		Name: "Unified",
		Runtime: agentpkg.Runtime{
			Type: agentpkg.RuntimeTypeBuiltin,
			Builtin: &agentpkg.BuiltinRuntime{
				Model:    agentpkg.BuiltinModel{LLMRouteID: "chat-main"},
				Topology: agentpkg.BuiltinTopology{Kind: agentpkg.TopologyKindSingle},
			},
		},
		Routes: agentpkg.Routes{LLMRouteIDs: []string{"chat-main"}},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	// AgentRoute target validation: an unknown agent is rejected, the existing
	// one is accepted.
	ghostCfg := agentroute.AgentRouteConfig{
		AgentRouteBaseConfig: agentroute.AgentRouteBaseConfig{MatchPolicy: agentroute.RouteMatch{PathPrefix: "/agents/ghost"}},
		AgentID:              "ghost",
	}
	ghostCfg.Normalize()
	storedGhost, _ := ghostCfg.ToConfig()
	if err := gw.AgentRouteResolver().CreateConfig(ctx, storedGhost, "test"); err == nil {
		t.Fatal("AgentRoute create accepted an unknown target agent")
	}
	routeID := createAgentRoute(t, gw, "unified", "/agents/unified", true)
	if routeID != "agent:unified:agents-unified" {
		t.Fatalf("route id = %q", routeID)
	}

	virtualKey := virtualkeypkg.VirtualKey{ID: "vk-1", Key: "vk-secret", AllowedRouteIDs: []string{routeID}}
	if err := gw.VirtualKeyManager().Create(ctx, virtualKey); err != nil {
		t.Fatalf("create virtual key: %v", err)
	}

	handler := NewHandler(gw, nil, zap.NewNop(), HandlerOptions{EnableAgent: true})
	turn := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/agents/unified/turn", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer vk-secret")
		rec := httptest.NewRecorder()
		if err := handler.Dispatch(rec, req, nil); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		return rec
	}

	// Missing virtual key fails before any agent resolution.
	noKey := httptest.NewRequest(http.MethodPost, "/agents/unified/turn", strings.NewReader(`{"input":"hello"}`))
	recNoKey := httptest.NewRecorder()
	if err := handler.Dispatch(recNoKey, noKey, nil); err != nil {
		t.Fatalf("Dispatch without key: %v", err)
	}
	if recNoKey.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want 401", recNoKey.Code)
	}

	// Builtin execution over the common envelope.
	rec := turn(`{"input":"hello"}`)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q body = %q, want SSE", ct, rec.Body.String())
	}
	body := rec.Body.String()
	for _, marker := range []string{"event: content", "builtin says hi", "event: done"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("SSE body missing %q:\n%s", marker, body)
		}
	}
	// The AgentRoute wire is the common envelope: agent_id, run_id, sequence.
	var envelope struct {
		AgentID  string `json:"agent_id"`
		RunID    string `json:"run_id"`
		Sequence uint64 `json:"sequence"`
	}
	firstData := strings.SplitN(strings.SplitN(body, "data: ", 2)[1], "\n", 2)[0]
	if err := json.Unmarshal([]byte(firstData), &envelope); err != nil {
		t.Fatalf("decode envelope %q: %v", firstData, err)
	}
	if envelope.AgentID != "unified" || !runtimeapi.ValidRunID(envelope.RunID) || envelope.Sequence != 1 {
		t.Fatalf("common envelope = %+v", envelope)
	}
	// M6 keeps route_kind/protocol unified while selecting the typed builtin
	// event store from runtime_type.
	turnEvents := eventsOfType[usage.BuiltinUsageEvent](sink.events)
	if len(turnEvents) == 0 {
		t.Fatalf("no interaction events recorded: %#v", sink.events)
	}
	turnEvent := turnEvents[len(turnEvents)-1]
	if turnEvent.AgentID != "unified" || turnEvent.RunID != envelope.RunID || turnEvent.RouteKind != "agent" || turnEvent.RuntimeType != "builtin" {
		t.Fatalf("interaction event = %+v", turnEvent)
	}
	if !turnEvent.Success || turnEvent.ResultStatus != "success" {
		t.Fatalf("successful turn status = %+v", turnEvent)
	}
	// The inner LLM call is parented under the agent turn with attribution.
	llmEvents := eventsOfType[usage.LLMUsageEvent](sink.events)
	if len(llmEvents) == 0 || llmEvents[0].AgentID != "unified" || llmEvents[0].RunID != envelope.RunID {
		t.Fatalf("inner llm events = %+v", llmEvents)
	}

	// Unknown top-level fields and foreign runtime options fail closed.
	if rec := turn(`{"input":"hello","thread_id":"t1"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown top-level field status = %d, want 400", rec.Code)
	}
	failedTurnEvents := eventsOfType[usage.BuiltinUsageEvent](sink.events)
	failedTurn := failedTurnEvents[len(failedTurnEvents)-1]
	if failedTurn.Success || failedTurn.ResultStatus != "error" || failedTurn.ErrorType != string(runtimeapi.ErrorUnsupportedOption) {
		t.Fatalf("pre-stream decode event = %+v, want typed error status", failedTurn)
	}
	if rec := turn(`{"input":"hello","options":{"version":"v1","runtime":{"cwd":"/tmp"}}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("foreign runtime option status = %d, want 400", rec.Code)
	}
	if rec := turn(`{"input":"hello","options":{"version":"v2"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown options version status = %d, want 400", rec.Code)
	}

	// Optional operations fail closed on a backend without the capability.
	sessions := httptest.NewRequest(http.MethodGet, "/agents/unified/sessions", nil)
	sessions.Header.Set("Authorization", "Bearer vk-secret")
	recSessions := httptest.NewRecorder()
	if err := handler.Dispatch(recSessions, sessions, nil); err != nil {
		t.Fatalf("Dispatch sessions: %v", err)
	}
	if recSessions.Code != http.StatusNotImplemented {
		t.Fatalf("builtin sessions status = %d, want 501", recSessions.Code)
	}
	permission := httptest.NewRequest(http.MethodPost, "/agents/unified/permission", strings.NewReader(`{"request_id":"perm-1"}`))
	permission.Header.Set("Authorization", "Bearer vk-secret")
	recPermission := httptest.NewRecorder()
	if err := handler.Dispatch(recPermission, permission, nil); err != nil {
		t.Fatalf("Dispatch permission: %v", err)
	}
	if recPermission.Code != http.StatusNotImplemented {
		t.Fatalf("builtin route permission status = %d, want 501 (new_stream continues on /turn)", recPermission.Code)
	}
	capabilityEvents := eventsOfType[usage.BuiltinUsageEvent](sink.events)
	capabilityEvent := capabilityEvents[len(capabilityEvents)-1]
	if capabilityEvent.Success || capabilityEvent.ResultStatus != "error" || capabilityEvent.ErrorType != string(runtimeapi.ErrorCapabilityNotSupported) {
		t.Fatalf("capability rejection event = %+v, want typed error status", capabilityEvent)
	}

	wrongMethod := httptest.NewRequest(http.MethodGet, "/agents/unified/turn", nil)
	wrongMethod.Header.Set("Authorization", "Bearer vk-secret")
	recWrongMethod := httptest.NewRecorder()
	if err := handler.Dispatch(recWrongMethod, wrongMethod, nil); err != nil {
		t.Fatalf("Dispatch wrong method: %v", err)
	}
	methodEvents := eventsOfType[usage.BuiltinUsageEvent](sink.events)
	methodEvent := methodEvents[len(methodEvents)-1]
	if recWrongMethod.Code != http.StatusMethodNotAllowed || methodEvent.Success || methodEvent.ResultStatus != "error" || methodEvent.ErrorType != "method_not_allowed" {
		t.Fatalf("method rejection status/event = %d/%+v", recWrongMethod.Code, methodEvent)
	}

	// Changing runtime.type keeps the route id, URL, and VirtualKey allowlist:
	// the same request now selects the ACP backend using inline runtime config,
	// without touching the route or key.
	if err := gw.AgentManager().Update(ctx, "unified", agentpkg.Agent{
		ID:      "unified",
		Name:    "Unified",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeACP, ACP: &agentpkg.ACPRuntime{ServiceID: "svc-missing"}},
	}); err != nil {
		t.Fatalf("switch runtime type: %v", err)
	}
	if rec := turn(`{"input":"hello","options":{"version":"v1","runtime":{"thread_id":"t1"}}}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("acp-runtime turn status = %d body = %q, want 502 from unavailable test adapter", rec.Code, rec.Body.String())
	}

	// An identity-only runtime (http) persists and routes, but dispatch fails
	// with runtime_not_executable before invoking any backend.
	if err := gw.AgentManager().Update(ctx, "unified", agentpkg.Agent{
		ID:      "unified",
		Name:    "Unified",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeHTTP, HTTP: &agentpkg.HTTPRuntime{Endpoint: "https://example.com/agent"}},
	}); err != nil {
		t.Fatalf("switch to http runtime: %v", err)
	}
	if rec := turn(`{"input":"hello"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("http-runtime turn status = %d, want 501 runtime_not_executable", rec.Code)
	}

	// A disabled Agent stays a valid route target and fails pre-stream.
	if err := gw.AgentManager().Update(ctx, "unified", agentpkg.Agent{
		ID:       "unified",
		Name:     "Unified",
		Runtime:  agentpkg.Runtime{Type: agentpkg.RuntimeTypeHTTP, HTTP: &agentpkg.HTTPRuntime{Endpoint: "https://example.com/agent"}},
		Disabled: true,
	}); err != nil {
		t.Fatalf("disable agent: %v", err)
	}
	if rec := turn(`{"input":"hello"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("disabled turn status = %d, want 400 agent_disabled", rec.Code)
	}

	// An Agent targeted by an AgentRoute cannot be deleted.
	if err := gw.AgentManager().Delete(ctx, "unified"); err == nil {
		t.Fatal("delete targeted agent succeeded, want conflict")
	}

	// With agent dispatch disabled, kind=agent routes pass through untouched.
	disabledHandler := NewHandler(gw, nil, zap.NewNop(), HandlerOptions{})
	reqDisabled := httptest.NewRequest(http.MethodPost, "/agents/unified/turn", strings.NewReader(`{"input":"hello"}`))
	reqDisabled.Header.Set("Authorization", "Bearer vk-secret")
	recDisabled := httptest.NewRecorder()
	if err := disabledHandler.Dispatch(recDisabled, reqDisabled, nil); err != nil {
		t.Fatalf("Dispatch with agent disabled: %v", err)
	}
	if recDisabled.Code != http.StatusNotFound {
		t.Fatalf("agent-disabled dispatch status = %d, want passthrough 404", recDisabled.Code)
	}
}

// agentRouteACPRuntime is a fake native ACP turn server injected through
// BootstrapOptions.ACPRuntime. It captures the owner key, translated runtime
// config, and turn request, and emits a minimal native event sequence.
type agentRouteACPRuntime struct {
	owner       string
	cfg         acpruntime.RuntimeConfig
	req         acpruntime.TurnRequest
	retirements []string
}

func (r *agentRouteACPRuntime) ServeConfiguredTurn(_ context.Context, owner string, cfg acpruntime.RuntimeConfig, req acpruntime.TurnRequest, emit acpruntime.EventSink) error {
	r.owner, r.cfg, r.req = owner, cfg, req
	if err := emit(acpruntime.TurnEvent{Event: runtimeapi.EventSession, SessionID: "native-session"}); err != nil {
		return err
	}
	if err := emit(acpruntime.TurnEvent{Event: runtimeapi.EventContent, Text: "acp answer"}); err != nil {
		return err
	}
	return emit(acpruntime.TurnEvent{Event: runtimeapi.EventDone, StopReason: "end_turn"})
}

func (r *agentRouteACPRuntime) RetireOwnerDeferred(ownerID, keep string) (int, func()) {
	r.retirements = append(r.retirements, ownerID+"|"+keep)
	return 0, func() {}
}

// TestDispatchAgentRouteACPTurnSuccess proves the same AgentRoute schema that
// executes a builtin Agent also executes an ACP Agent: dispatch resolves the
// Agent from the definition snapshot, the ACP backend translates the
// canonical agent-keyed runtime config snapshot (the per-request service-store
// read is pinned absent by gateway-level tests), decodes the v1 runtime
// options, and the turn streams the common event envelope.
func TestDispatchAgentRouteACPTurnSuccess(t *testing.T) {
	ctx := t.Context()
	backend, err := configstore.OpenBackend(ctx, "sqlite", configstoresqlite.Config{SQLitePath: t.TempDir() + "/config.db"}, nil)
	if err != nil {
		t.Fatalf("open sqlite backend: %v", err)
	}
	if err := configschema.RegisterDefaultStores(backend); err != nil {
		t.Fatalf("register default stores: %v", err)
	}

	native := &agentRouteACPRuntime{}
	sink := &builtinCaptureSink{}
	gw := gateway.NewAgentGateway()
	if err := gw.Bootstrap(ctx, gateway.BootstrapOptions{
		ConfigStoreBackend: backend,
		UsageObserver:      usage.NewObserver(sink),
		ACPRuntime:         native,
	}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	cwd := t.TempDir()
	if err := gw.AgentManager().Create(ctx, agentpkg.Agent{
		ID: "acp-agent", Name: "ACP Agent",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeACP, ACP: &agentpkg.ACPRuntime{
			AgentType: baseacp.AgentTypeOpencode, CWD: cwd,
			AllowedRoots: []string{cwd}, DefaultModel: "model-a",
		}},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	// Agent creation committed one definition generation, so the canonical
	// snapshot established this owner's accepted config fingerprint.
	if len(native.retirements) != 1 || native.retirements[0] == "acp-agent|" {
		t.Fatalf("accepted fingerprint was not established at agent create: %v", native.retirements)
	}

	createAgentRoute(t, gw, "acp-agent", "/agents/acp", false)
	handler := NewHandler(gw, nil, zap.NewNop(), HandlerOptions{EnableAgent: true})

	body := `{"input":"hello","options":{"version":"v1","runtime":{"thread_id":"thread-1","model":"model-b"}}}`
	req := httptest.NewRequest(http.MethodPost, "/agents/acp/turn", strings.NewReader(body))
	rec := httptest.NewRecorder()
	if err := handler.Dispatch(rec, req, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q body = %q, want SSE", ct, rec.Body.String())
	}
	sseBody := rec.Body.String()
	for _, marker := range []string{"event: session", "event: content", "acp answer", "event: done"} {
		if !strings.Contains(sseBody, marker) {
			t.Fatalf("SSE body missing %q:\n%s", marker, sseBody)
		}
	}

	// Every frame carries the common envelope with a monotonic run sequence.
	var envelope struct {
		AgentID  string `json:"agent_id"`
		RunID    string `json:"run_id"`
		Sequence uint64 `json:"sequence"`
	}
	frames := strings.Split(strings.TrimSpace(sseBody), "\n\n")
	sequences := make([]uint64, 0, len(frames))
	for _, frame := range frames {
		data := strings.SplitN(strings.SplitN(frame, "data: ", 2)[1], "\n", 2)[0]
		if err := json.Unmarshal([]byte(data), &envelope); err != nil {
			t.Fatalf("decode envelope %q: %v", data, err)
		}
		if envelope.AgentID != "acp-agent" || !runtimeapi.ValidRunID(envelope.RunID) {
			t.Fatalf("common envelope = %+v", envelope)
		}
		sequences = append(sequences, envelope.Sequence)
	}
	if len(sequences) < 3 {
		t.Fatalf("SSE frame count = %d, want at least session/content/done", len(sequences))
	}
	for i := 1; i < len(sequences); i++ {
		if sequences[i] != sequences[i-1]+1 {
			t.Fatalf("sequence is not monotonic across the run: %v", sequences)
		}
	}

	// The native runtime received the agent_id owner key, the canonical config
	// translated from inline Agent.runtime.acp, and the decoded v1 options.
	if native.owner != "acp-agent" {
		t.Fatalf("native owner = %q, want the agent_id owner key", native.owner)
	}
	if native.cfg.AgentType != baseacp.AgentTypeOpencode || native.cfg.DefaultModel != "model-a" || len(native.cfg.AllowedRoots) != 1 || native.cfg.AllowedRoots[0] != cwd {
		t.Fatalf("native runtime config = %+v", native.cfg)
	}
	if native.req.ThreadID != "thread-1" || native.req.Model != "model-b" || native.req.Input != "hello" {
		t.Fatalf("native turn request = %+v", native.req)
	}
	if native.req.RunID == "" {
		t.Fatal("native turn request carries no gateway run id")
	}

	// kind=agent attribution persists in the ACP typed family without restoring
	// the removed service identity.
	turnEvents := eventsOfType[usage.ACPUsageEvent](sink.events)
	if len(turnEvents) == 0 {
		t.Fatalf("no interaction events recorded: %#v", sink.events)
	}
	turnEvent := turnEvents[len(turnEvents)-1]
	if turnEvent.AgentID != "acp-agent" || turnEvent.RunID != native.req.RunID || turnEvent.RouteKind != "agent" || turnEvent.RuntimeType != "acp" {
		t.Fatalf("interaction event = %+v", turnEvent)
	}
	if turnEvent.ServiceID != "" || turnEvent.Operation != "turn" || turnEvent.ThreadID != "thread-1" || turnEvent.SessionID != "native-session" {
		t.Fatalf("typed acp extension = %+v", turnEvent)
	}
}

func TestServeAgentPermissionDecodeFailureMarksTypedError(t *testing.T) {
	sink := &usage.InMemorySink{}
	span, ctx := usage.NewObserver(sink).Begin(t.Context(), usage.InteractionDimensions{
		RouteKind: "agent", AgentID: "acp-agent", RuntimeType: agentpkg.RuntimeTypeACP,
	})
	a := agentpkg.Agent{
		ID: "acp-agent",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeACP, ACP: &agentpkg.ACPRuntime{
			AgentType: baseacp.AgentTypeOpencode,
		}},
	}
	backend := &agentHandlerCapabilityBackend{Backend: runtimeapitest.NewBackend(agentpkg.RuntimeTypeACP)}
	caps := runtimeapi.Capabilities{Executable: true, Permissions: runtimeapi.PermissionCapabilities{
		Interactive: true, ResumeMode: runtimeapi.PermissionResumeActiveStream,
	}}
	req := httptest.NewRequest(http.MethodPost, "/agents/acp/permission", strings.NewReader(`{"request_id":`)).WithContext(ctx)
	rec := httptest.NewRecorder()

	err := (&Handler{}).serveAgentPermission(rec, req, backend, a, caps)
	if err != nil {
		t.Fatalf("serveAgentPermission: %v", err)
	}
	span.Finish(usage.InteractionOutcome{Success: false, StatusCode: rec.Code, ErrorType: string(runtimeapi.ErrorInvalidRequest)})

	event := sink.Events[0].(usage.ACPUsageEvent)
	if rec.Code != http.StatusBadRequest || event.Success || event.Operation != "permission" || event.ResultStatus != "error" || event.ErrorType != string(runtimeapi.ErrorInvalidRequest) {
		t.Fatalf("malformed permission status/event = %d/%+v", rec.Code, event)
	}
}

func TestDispatchAgentOptionalCapabilitiesAndPreBackendRejections(t *testing.T) {
	ctx := t.Context()
	store, err := configstore.OpenBackend(ctx, "sqlite", configstoresqlite.Config{SQLitePath: t.TempDir() + "/config.db"}, nil)
	if err != nil {
		t.Fatalf("open sqlite backend: %v", err)
	}
	if err := configschema.RegisterDefaultStores(store); err != nil {
		t.Fatalf("register default stores: %v", err)
	}

	gw := gateway.NewAgentGateway()
	if err := gw.Bootstrap(ctx, gateway.BootstrapOptions{ConfigStoreBackend: store}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	fake := &agentHandlerCapabilityBackend{Backend: runtimeapitest.NewBackend(agentpkg.RuntimeTypeHTTP)}
	fake.CapabilitiesResult = runtimeapi.Capabilities{
		Executable: true,
		Sessions:   runtimeapi.SessionCapabilities{Transcript: true},
		Permissions: runtimeapi.PermissionCapabilities{
			Interactive: true,
			ResumeMode:  runtimeapi.PermissionResumeActiveStream,
		},
	}
	if err := gw.RuntimeRegistry().Register(fake); err != nil {
		t.Fatalf("register fake HTTP backend: %v", err)
	}

	a := agentpkg.Agent{
		ID:      "capable",
		Name:    "Capable",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeHTTP, HTTP: &agentpkg.HTTPRuntime{Endpoint: "https://example.com/agent"}},
	}
	if err := gw.AgentManager().Create(ctx, a); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	routeID := createAgentRoute(t, gw, a.ID, "/agents/capable", false)
	handler := NewHandler(gw, nil, zap.NewNop(), HandlerOptions{EnableAgent: true})

	permission := httptest.NewRequest(http.MethodPost, "/agents/capable/permission", strings.NewReader(`{"request_id":" perm-1 ","outcome":" selected ","option_id":" allow "}`))
	recPermission := httptest.NewRecorder()
	if err := handler.Dispatch(recPermission, permission, nil); err != nil {
		t.Fatalf("Dispatch permission: %v", err)
	}
	if recPermission.Code != http.StatusOK || len(fake.permissionCalls) != 1 {
		t.Fatalf("permission status/calls = %d/%d, body = %q", recPermission.Code, len(fake.permissionCalls), recPermission.Body.String())
	}
	if got := fake.permissionCalls[0]; got.RequestID != "perm-1" || got.Outcome != "selected" || got.OptionID != "allow" {
		t.Fatalf("permission decision = %+v, want trimmed active-stream decision", got)
	}

	transcript := httptest.NewRequest(http.MethodGet, "/agents/capable/sessions/session-1/transcript?cwd=%2Fworkspace", nil)
	recTranscript := httptest.NewRecorder()
	if err := handler.Dispatch(recTranscript, transcript, nil); err != nil {
		t.Fatalf("Dispatch transcript: %v", err)
	}
	if recTranscript.Code != http.StatusOK || len(fake.transcriptCalls) != 1 {
		t.Fatalf("transcript status/calls = %d/%d, body = %q", recTranscript.Code, len(fake.transcriptCalls), recTranscript.Body.String())
	}
	if got := fake.transcriptCalls[0]; got.SessionID != "session-1" || got.CWD != "/workspace" {
		t.Fatalf("transcript request = %+v", got)
	}
	var transcriptBody runtimeapi.TranscriptResponse
	if err := json.Unmarshal(recTranscript.Body.Bytes(), &transcriptBody); err != nil {
		t.Fatalf("decode transcript response: %v", err)
	}
	if transcriptBody.SessionID != "session-1" || len(transcriptBody.Messages) != 1 || transcriptBody.Messages[0].Text != "saved reply" {
		t.Fatalf("transcript response = %+v", transcriptBody)
	}

	fake.SetCapabilities(runtimeapi.Capabilities{Executable: false}, nil)
	nonExecutableTurn := httptest.NewRequest(http.MethodPost, "/agents/capable/turn", strings.NewReader(`{"input":"hello"}`))
	recNonExecutable := httptest.NewRecorder()
	if err := handler.Dispatch(recNonExecutable, nonExecutableTurn, nil); err != nil {
		t.Fatalf("Dispatch non-executable turn: %v", err)
	}
	if recNonExecutable.Code != http.StatusNotImplemented || len(fake.TurnCalls()) != 0 {
		t.Fatalf("non-executable status/ServeTurn calls = %d/%d, body = %q", recNonExecutable.Code, len(fake.TurnCalls()), recNonExecutable.Body.String())
	}
	fake.SetCapabilities(runtimeapi.Capabilities{Executable: true}, nil)

	a.Disabled = true
	if err := gw.AgentManager().Update(ctx, a.ID, a); err != nil {
		t.Fatalf("disable agent: %v", err)
	}
	disabledTurn := httptest.NewRequest(http.MethodPost, "/agents/capable/turn", strings.NewReader(`{"input":"hello"}`))
	recDisabled := httptest.NewRecorder()
	if err := handler.Dispatch(recDisabled, disabledTurn, nil); err != nil {
		t.Fatalf("Dispatch disabled turn: %v", err)
	}
	if recDisabled.Code != http.StatusBadRequest || len(fake.TurnCalls()) != 0 {
		t.Fatalf("disabled status/ServeTurn calls = %d/%d, body = %q", recDisabled.Code, len(fake.TurnCalls()), recDisabled.Body.String())
	}

	if err := gw.AgentRouteResolver().DeleteConfig(ctx, routeID); err != nil {
		t.Fatalf("delete targeting route: %v", err)
	}
	if err := gw.AgentManager().Delete(ctx, a.ID); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	missingTurn := httptest.NewRequest(http.MethodPost, "/agents/capable/turn", strings.NewReader(`{"input":"hello"}`))
	recMissing := httptest.NewRecorder()
	if err := handler.Dispatch(recMissing, missingTurn, nil); err != nil {
		t.Fatalf("Dispatch missing-agent turn: %v", err)
	}
	if recMissing.Code != http.StatusNotFound || len(fake.TurnCalls()) != 0 {
		t.Fatalf("missing status/ServeTurn calls = %d/%d, body = %q", recMissing.Code, len(fake.TurnCalls()), recMissing.Body.String())
	}
}
