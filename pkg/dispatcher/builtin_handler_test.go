package dispatcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	configschema "github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	configstoresqlite "github.com/agent-guide/agent-gateway/pkg/configstore/sqlite"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	"github.com/agent-guide/agent-gateway/pkg/gateway/builtinroute"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

type builtinFakeProvider struct{}

func (builtinFakeProvider) Chat(_ context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	msg := schema.AssistantMessage("builtin says hi", nil)
	msg.ResponseMeta = &schema.ResponseMeta{
		FinishReason: "stop",
		Usage:        &schema.TokenUsage{PromptTokens: 5, CompletionTokens: 4, TotalTokens: 9},
	}
	return &provider.ChatResponse{Message: msg}, nil
}

func (p builtinFakeProvider) StreamChat(ctx context.Context, req *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(resp.Message, nil)
	sw.Close()
	return sr, nil
}

func (builtinFakeProvider) ListModels(context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (builtinFakeProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{}
}
func (builtinFakeProvider) Config() provider.ProviderConfig {
	return provider.ProviderConfig{Id: "fake", ProviderType: "fake"}
}

type builtinCaptureSink struct {
	events []any
}

func (s *builtinCaptureSink) Enqueue(v any) bool {
	s.events = append(s.events, v)
	return true
}

func TestDispatchBuiltinTurnEndToEnd(t *testing.T) {
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
		ID:   "triage",
		Name: "Triage",
		Runtime: agentpkg.Runtime{
			Type: agentpkg.RuntimeTypeBuiltin,
			Builtin: &agentpkg.BuiltinRuntime{
				Model:        agentpkg.BuiltinModel{LLMRouteID: "chat-main"},
				SystemPrompt: "You are a triage agent.",
				Topology:     agentpkg.BuiltinTopology{Kind: agentpkg.TopologyKindSingle},
			},
		},
		Routes: agentpkg.Routes{LLMRouteIDs: []string{"chat-main"}},
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	routeCfg := builtinroute.BuiltinRouteConfig{
		AgentRouteConfig: builtinroute.AgentRouteConfig{
			MatchPolicy: builtinroute.RouteMatch{PathPrefix: "/agents/triage"},
		},
		AgentID: "triage",
	}
	routeCfg.Normalize()
	stored, err := routeCfg.ToConfig()
	if err != nil {
		t.Fatalf("builtin route ToConfig: %v", err)
	}
	if err := gw.BuiltinRouteResolver().CreateConfig(ctx, stored, "test"); err != nil {
		t.Fatalf("create builtin route: %v", err)
	}

	handler := NewHandler(gw, nil, zap.NewNop(), HandlerOptions{EnableBuiltin: true})
	req := httptest.NewRequest(http.MethodPost, "/agents/triage/turn", strings.NewReader(`{"input":"hello"}`))
	rec := httptest.NewRecorder()
	if err := handler.Dispatch(rec, req, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q body = %q, want SSE", ct, rec.Body.String())
	}
	body := rec.Body.String()
	for _, marker := range []string{"event: session", "event: content", "builtin says hi", "event: usage", "event: done"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("SSE body missing %q:\n%s", marker, body)
		}
	}

	var builtinEvents []usage.BuiltinUsageEvent
	var llmEvents []usage.LLMUsageEvent
	for _, ev := range sink.events {
		switch typed := ev.(type) {
		case usage.BuiltinUsageEvent:
			builtinEvents = append(builtinEvents, typed)
		case usage.LLMUsageEvent:
			llmEvents = append(llmEvents, typed)
		}
	}
	if len(builtinEvents) != 1 {
		t.Fatalf("builtin events = %d, want 1", len(builtinEvents))
	}
	turn := builtinEvents[0]
	if turn.AgentID != "triage" || turn.Operation != "turn" || turn.ResultStatus != "success" {
		t.Fatalf("turn event = %+v, want triage/turn/success", turn)
	}
	if !runtimeapi.ValidRunID(turn.RunID) || turn.RuntimeType != "builtin" || turn.InteractionEvent.RunID != turn.RunID {
		t.Fatalf("turn common identity = %+v", turn)
	}
	if !turn.Success || turn.TopologyKind != "single" || turn.ModelSteps == 0 {
		t.Fatalf("turn event = %+v, want successful single-topology turn with model steps", turn)
	}
	if len(llmEvents) == 0 {
		t.Fatalf("llm child events = 0, want inner model call recorded (events: %#v)", sink.events)
	}
	inner := llmEvents[0]
	if inner.AgentID != "triage" || inner.ParentSpanID != turn.SpanID || inner.TraceID != turn.TraceID || inner.RunID != turn.RunID || inner.RuntimeType != "builtin" {
		t.Fatalf("inner llm event = %+v, want parented under the turn span with agent attribution", inner)
	}
	if inner.TotalTokens != 9 {
		t.Fatalf("inner llm tokens = %d, want 9", inner.TotalTokens)
	}

	// Pre-stream failures return real HTTP status codes, never a 200 SSE
	// stream: unknown target agent -> 404.
	badRoute := builtinroute.BuiltinRouteConfig{
		AgentRouteConfig: builtinroute.AgentRouteConfig{MatchPolicy: builtinroute.RouteMatch{PathPrefix: "/agents/ghost"}},
		AgentID:          "ghost",
	}
	badRoute.Normalize()
	storedBad, _ := badRoute.ToConfig()
	if err := gw.BuiltinRouteResolver().CreateConfig(ctx, storedBad, "test"); err != nil {
		t.Fatalf("create ghost route: %v", err)
	}
	reqBad := httptest.NewRequest(http.MethodPost, "/agents/ghost/turn", strings.NewReader(`{"input":"hello"}`))
	recBad := httptest.NewRecorder()
	if err := handler.Dispatch(recBad, reqBad, nil); err != nil {
		t.Fatalf("Dispatch ghost: %v", err)
	}
	if recBad.Code != http.StatusNotFound {
		t.Fatalf("ghost turn status = %d body = %q, want 404", recBad.Code, recBad.Body.String())
	}
	ghostEvents := eventsOfType[usage.BuiltinUsageEvent](sink.events)
	last := ghostEvents[len(ghostEvents)-1]
	if last.Success || last.StatusCode != http.StatusNotFound || last.ErrorType != "agent_not_found" {
		t.Fatalf("ghost turn event = %+v, want failed 404 with agent_not_found", last)
	}

	// Disabled agent -> 400, also counted as a failure. Route-binding
	// uniqueness means it needs its own LLM route binding.
	altRoute := llmroutepkg.LLMRoute{
		AgentRouteConfig: llmroutepkg.AgentRouteConfig{
			ID:          "chat-alt",
			Kind:        routecore.RouteKindLLM,
			Protocol:    llmroutepkg.RouteProtocolOpenAI,
			MatchPolicy: llmroutepkg.RouteMatchPolicy{PathPrefix: "/v1-alt"},
		},
		TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
			ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "fake"},
		},
	}
	altCfg, err := altRoute.ToConfig()
	if err != nil {
		t.Fatalf("alt llm route ToConfig: %v", err)
	}
	if err := gw.LLMRouteResolver().CreateConfig(ctx, altCfg, "test"); err != nil {
		t.Fatalf("create alt llm route: %v", err)
	}
	disabled := agentpkg.Agent{
		ID:   "off",
		Name: "Off",
		Runtime: agentpkg.Runtime{
			Type: agentpkg.RuntimeTypeBuiltin,
			Builtin: &agentpkg.BuiltinRuntime{
				Model:    agentpkg.BuiltinModel{LLMRouteID: "chat-alt"},
				Topology: agentpkg.BuiltinTopology{Kind: agentpkg.TopologyKindSingle},
			},
		},
		Routes:   agentpkg.Routes{LLMRouteIDs: []string{"chat-alt"}},
		Disabled: true,
	}
	if err := gw.AgentManager().Create(ctx, disabled); err != nil {
		t.Fatalf("create disabled agent: %v", err)
	}
	offRoute := builtinroute.BuiltinRouteConfig{
		AgentRouteConfig: builtinroute.AgentRouteConfig{MatchPolicy: builtinroute.RouteMatch{PathPrefix: "/agents/off"}},
		AgentID:          "off",
	}
	offRoute.Normalize()
	storedOff, _ := offRoute.ToConfig()
	if err := gw.BuiltinRouteResolver().CreateConfig(ctx, storedOff, "test"); err != nil {
		t.Fatalf("create off route: %v", err)
	}
	reqOff := httptest.NewRequest(http.MethodPost, "/agents/off/turn", strings.NewReader(`{"input":"hello"}`))
	recOff := httptest.NewRecorder()
	if err := handler.Dispatch(recOff, reqOff, nil); err != nil {
		t.Fatalf("Dispatch off: %v", err)
	}
	if recOff.Code != http.StatusBadRequest {
		t.Fatalf("disabled turn status = %d body = %q, want 400", recOff.Code, recOff.Body.String())
	}

	// Client-correctable pre-host failures carry precise error types on their
	// usage events instead of the internal_error fallback.
	reqEmpty := httptest.NewRequest(http.MethodPost, "/agents/triage/turn", strings.NewReader(`{"input":"  "}`))
	recEmpty := httptest.NewRecorder()
	if err := handler.Dispatch(recEmpty, reqEmpty, nil); err != nil {
		t.Fatalf("Dispatch empty input: %v", err)
	}
	if recEmpty.Code != http.StatusBadRequest {
		t.Fatalf("empty input status = %d, want 400", recEmpty.Code)
	}
	events := eventsOfType[usage.BuiltinUsageEvent](sink.events)
	emptyEv := events[len(events)-1]
	if emptyEv.Success || emptyEv.ErrorType != "invalid_request" {
		t.Fatalf("empty-input event = %+v, want failed invalid_request", emptyEv)
	}

	reqGet := httptest.NewRequest(http.MethodGet, "/agents/triage/turn", nil)
	recGet := httptest.NewRecorder()
	if err := handler.Dispatch(recGet, reqGet, nil); err != nil {
		t.Fatalf("Dispatch GET: %v", err)
	}
	if recGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", recGet.Code)
	}
	events = eventsOfType[usage.BuiltinUsageEvent](sink.events)
	getEv := events[len(events)-1]
	if getEv.Success || getEv.ErrorType != "method_not_allowed" {
		t.Fatalf("GET event = %+v, want failed method_not_allowed", getEv)
	}

	// A request under the route prefix that is not the turn endpoint passes
	// through to the next handler and must not emit a usage event.
	countBefore := len(sink.events)
	reqOther := httptest.NewRequest(http.MethodPost, "/agents/triage/other", strings.NewReader(`{}`))
	recOther := httptest.NewRecorder()
	if err := handler.Dispatch(recOther, reqOther, nil); err != nil {
		t.Fatalf("Dispatch passthrough: %v", err)
	}
	if len(sink.events) != countBefore {
		t.Fatalf("passthrough emitted %d new events, want none", len(sink.events)-countBefore)
	}
}

func eventsOfType[T any](events []any) []T {
	var out []T
	for _, ev := range events {
		if typed, ok := ev.(T); ok {
			out = append(out, typed)
		}
	}
	return out
}
