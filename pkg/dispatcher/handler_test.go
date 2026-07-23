package dispatcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-guide/agent-gateway/internal/httpcapture"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

type stubLLMApiHandler struct{}

func (stubLLMApiHandler) Name() string { return "stub" }

func (stubLLMApiHandler) MatchLLMApi(*http.Request) bool { return true }

func (stubLLMApiHandler) PrepareLLMApiRequest(*http.Request) (*PreparedLLMApiRequest, llmroutepkg.RequestRequirements, error) {
	return &PreparedLLMApiRequest{
		Type:        provider.LLMApiRequestTypeChat,
		ChatRequest: &provider.ChatRequest{},
	}, llmroutepkg.RequestRequirements{}, nil
}

func (stubLLMApiHandler) ServeLLMApi(w http.ResponseWriter, _ *http.Request, _ provider.Provider, _ *PreparedLLMApiRequest) error {
	w.WriteHeader(http.StatusAccepted)
	return nil
}

type nonMatchingLLMApiHandler struct {
	stubLLMApiHandler
}

func (nonMatchingLLMApiHandler) MatchLLMApi(*http.Request) bool { return false }

type nextHandler struct {
	called bool
}

func (h *nextHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) error {
	h.called = true
	w.WriteHeader(http.StatusTeapot)
	return nil
}

func TestHandlerRequiresVirtualKeyBeforeLLMApiMatch(t *testing.T) {
	gw := gateway.NewAgentGateway()
	if err := gw.Bootstrap(context.Background(), gateway.BootstrapOptions{
		StaticLLMRoutes: mustRouteConfigs(t, []llmroutepkg.LLMRoute{{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{
				ID:          "broad-route",
				Protocol:    llmroutepkg.RouteProtocol("stub"),
				MatchPolicy: llmroutepkg.RouteMatchPolicy{PathPrefix: "/"},
				AuthPolicy:  llmroutepkg.RouteAuthPolicy{RequireVirtualKey: true},
			},
			TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
				ProviderTarget: llmroutepkg.DirectProviderTarget{
					ProviderID: "openai",
				},
			},
		}}),
	}); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	handler := NewHandler(gw, map[string]LLMApiHandler{"stub": nonMatchingLLMApiHandler{}}, nil, HandlerOptions{})
	next := &nextHandler{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)

	if err := handler.Dispatch(rec, req, next); err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if next.called {
		t.Fatal("next handler should not be called when virtual key is required")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandlerRateLimitsBeforeLLMApiMatchAndRecordsOutcome(t *testing.T) {
	sink := &builtinCaptureSink{}
	gw := gateway.NewAgentGateway()
	if err := gw.Bootstrap(context.Background(), gateway.BootstrapOptions{
		ConfigStoreBackend: &testConfigStoreBackend{store: singleVirtualKeyStore{
			keyID: "vk-1",
			key:   "secret-key",
			route: "broad-route",
			rateLimits: &virtualkey.VirtualKeyRateLimits{
				LLM: &virtualkey.RateLimit{RequestsPerMinute: 1, Burst: 1},
			},
		}},
		StaticLLMRoutes: mustRouteConfigs(t, []llmroutepkg.LLMRoute{{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{
				ID:          "broad-route",
				Protocol:    llmroutepkg.RouteProtocol("stub"),
				MatchPolicy: llmroutepkg.RouteMatchPolicy{PathPrefix: "/"},
				AuthPolicy:  llmroutepkg.RouteAuthPolicy{RequireVirtualKey: true},
			},
			TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
				ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "unused"},
			},
		}}),
		UsageObserver: usage.NewObserver(sink),
	}); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	handler := NewHandler(gw, map[string]LLMApiHandler{"stub": nonMatchingLLMApiHandler{}}, nil, HandlerOptions{})
	firstNext := &nextHandler{}
	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	firstReq.Header.Set("Authorization", "Bearer secret-key")
	if err := handler.Dispatch(first, firstReq, firstNext); err != nil {
		t.Fatalf("first Dispatch returned error: %v", err)
	}
	if !firstNext.called || first.Code != http.StatusTeapot {
		t.Fatalf("first request next=%v status=%d, want true/%d", firstNext.called, first.Code, http.StatusTeapot)
	}

	secondNext := &nextHandler{}
	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	secondReq.Header.Set("Authorization", "Bearer secret-key")
	if err := handler.Dispatch(second, secondReq, secondNext); err != nil {
		t.Fatalf("second Dispatch returned error: %v", err)
	}
	if secondNext.called {
		t.Fatal("rate-limited request reached next handler")
	}
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", second.Code, http.StatusTooManyRequests)
	}
	if second.Header().Get("Retry-After") == "" || second.Header().Get("Retry-After") == "0" {
		t.Fatalf("Retry-After = %q, want >= 1", second.Header().Get("Retry-After"))
	}
	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.events))
	}
	event, ok := sink.events[0].(usage.LLMUsageEvent)
	if !ok {
		t.Fatalf("event type = %T, want usage.LLMUsageEvent", sink.events[0])
	}
	if event.StatusCode != http.StatusTooManyRequests || event.ErrorType != "rate_limited" || event.VirtualKeyID != "vk-1" {
		t.Fatalf("unexpected rate-limit event: %+v", event)
	}
}

// TestHandlerRateLimitsStreamingRequestConsumesOneToken covers the §14
// requirement that a streaming request consumes exactly one admission token.
// Admission runs before the protocol payload is parsed, so a stream=true body
// is admitted once and the next (streaming) request against the exhausted
// bucket is rejected — stream identity neither bypasses nor multiplies
// admission.
func TestHandlerRateLimitsStreamingRequestConsumesOneToken(t *testing.T) {
	sink := &builtinCaptureSink{}
	gw := gateway.NewAgentGateway()
	if err := gw.Bootstrap(context.Background(), gateway.BootstrapOptions{
		ConfigStoreBackend: &testConfigStoreBackend{store: singleVirtualKeyStore{
			keyID: "vk-1",
			key:   "secret-key",
			route: "broad-route",
			rateLimits: &virtualkey.VirtualKeyRateLimits{
				LLM: &virtualkey.RateLimit{RequestsPerMinute: 1, Burst: 1},
			},
		}},
		StaticLLMRoutes: mustRouteConfigs(t, []llmroutepkg.LLMRoute{{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{
				ID:          "broad-route",
				Protocol:    llmroutepkg.RouteProtocol("stub"),
				MatchPolicy: llmroutepkg.RouteMatchPolicy{PathPrefix: "/"},
				AuthPolicy:  llmroutepkg.RouteAuthPolicy{RequireVirtualKey: true},
			},
			TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
				ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "unused"},
			},
		}}),
		UsageObserver: usage.NewObserver(sink),
	}); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	handler := NewHandler(gw, map[string]LLMApiHandler{"stub": stubLLMApiHandler{}}, nil, HandlerOptions{})

	// First streaming request is admitted and reaches the LLM API handler.
	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	firstReq.Header.Set("Authorization", "Bearer secret-key")
	if err := handler.Dispatch(first, firstReq, nil); err != nil {
		t.Fatalf("first Dispatch returned error: %v", err)
	}
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusAccepted)
	}

	// Second streaming request is rejected at admission — proving the first
	// consumed the lone token and stream identity grants no extra capacity.
	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	secondReq.Header.Set("Authorization", "Bearer secret-key")
	if err := handler.Dispatch(second, secondReq, nil); err != nil {
		t.Fatalf("second Dispatch returned error: %v", err)
	}
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", second.Code, http.StatusTooManyRequests)
	}
}

// TestHandlerRateLimit429WrittenThroughResponseRecorder covers the §8/§14
// requirement that the 429 is written through the dispatcher's httpcapture
// response recorder (the same rec the deferred span-finish reads), so the
// finished interaction records status 429 and the rate_limited annotation is
// preserved. It uses the production ResponseRecorder rather than the test
// httptest.ResponseRecorder, and asserts the recorder type's own StatusCode()
// and the recorded interaction event in one pass.
func TestHandlerRateLimit429WrittenThroughResponseRecorder(t *testing.T) {
	sink := &builtinCaptureSink{}
	gw := gateway.NewAgentGateway()
	if err := gw.Bootstrap(context.Background(), gateway.BootstrapOptions{
		ConfigStoreBackend: &testConfigStoreBackend{store: singleVirtualKeyStore{
			keyID: "vk-1",
			key:   "secret-key",
			route: "broad-route",
			rateLimits: &virtualkey.VirtualKeyRateLimits{
				LLM: &virtualkey.RateLimit{RequestsPerMinute: 1, Burst: 1},
			},
		}},
		StaticLLMRoutes: mustRouteConfigs(t, []llmroutepkg.LLMRoute{{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{
				ID:          "broad-route",
				Protocol:    llmroutepkg.RouteProtocol("stub"),
				MatchPolicy: llmroutepkg.RouteMatchPolicy{PathPrefix: "/"},
				AuthPolicy:  llmroutepkg.RouteAuthPolicy{RequireVirtualKey: true},
			},
			TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
				ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "unused"},
			},
		}}),
		UsageObserver: usage.NewObserver(sink),
	}); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	handler := NewHandler(gw, map[string]LLMApiHandler{"stub": stubLLMApiHandler{}}, nil, HandlerOptions{})

	// First request consumes the lone token via the LLM API handler.
	firstBase := httptest.NewRecorder()
	if err := handler.Dispatch(firstBase, admitRequest("secret-key"), nil); err != nil {
		t.Fatalf("first Dispatch returned error: %v", err)
	}

	// Second request is rejected at admission. It is served through the same
	// production ResponseRecorder the dispatcher builds internally; assert on
	// the recorder type's StatusCode() and Retry-After directly.
	secondBase := httptest.NewRecorder()
	second := httpcapture.NewResponseRecorder(secondBase)
	if err := handler.Dispatch(second, admitRequest("secret-key"), nil); err != nil {
		t.Fatalf("second Dispatch returned error: %v", err)
	}
	if got := second.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("recorder StatusCode = %d, want %d (must read 429 via recorder, not raw writer)", got, http.StatusTooManyRequests)
	}
	if secondBase.Code != http.StatusTooManyRequests {
		t.Fatalf("underlying writer status = %d, want %d", secondBase.Code, http.StatusTooManyRequests)
	}
	if retry := second.Header().Get("Retry-After"); retry == "" || retry == "0" {
		t.Fatalf("Retry-After = %q, want >= 1", retry)
	}

	// The recorded interaction reflects the recorder: status 429 with the
	// rate_limited annotation, proving the deferred span-finish read the 429
	// through the recorder rather than treating the request as successful.
	var event usage.LLMUsageEvent
	for _, ev := range sink.events {
		if llm, ok := ev.(usage.LLMUsageEvent); ok && llm.StatusCode == http.StatusTooManyRequests {
			event = llm
			break
		}
	}
	if event.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("no 429 interaction event recorded; events = %+v", sink.events)
	}
	if event.ErrorType != "rate_limited" || event.VirtualKeyID != "vk-1" {
		t.Fatalf("unexpected rate-limit event: %+v", event)
	}
}

// TestHandlerRateLimitConsumesTokenOnServeNextOrNotFound covers the §7/§14
// requirement that a matched request which later passes through (the LLM API
// handler does not match the rewritten path, so the dispatcher falls through to
// serveNextOrNotFound with no next handler) still consumes a token. Admission
// runs before protocol parsing at a single central point, so an admitted-but-
// not-dispatched request spends its token; when the bucket is exhausted the
// dispatcher returns 429 instead of passing the request through.
func TestHandlerRateLimitConsumesTokenOnServeNextOrNotFound(t *testing.T) {
	sink := &builtinCaptureSink{}
	gw := gateway.NewAgentGateway()
	if err := gw.Bootstrap(context.Background(), gateway.BootstrapOptions{
		ConfigStoreBackend: &testConfigStoreBackend{store: singleVirtualKeyStore{
			keyID: "vk-1",
			key:   "secret-key",
			route: "broad-route",
			rateLimits: &virtualkey.VirtualKeyRateLimits{
				LLM: &virtualkey.RateLimit{RequestsPerMinute: 1, Burst: 1},
			},
		}},
		StaticLLMRoutes: mustRouteConfigs(t, []llmroutepkg.LLMRoute{{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{
				ID:          "broad-route",
				Protocol:    llmroutepkg.RouteProtocol("stub"),
				MatchPolicy: llmroutepkg.RouteMatchPolicy{PathPrefix: "/"},
				AuthPolicy:  llmroutepkg.RouteAuthPolicy{RequireVirtualKey: true},
			},
			TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
				ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "unused"},
			},
		}}),
		UsageObserver: usage.NewObserver(sink),
	}); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	// nonMatchingLLMApiHandler makes the dispatcher admit the request (route
	// matches), then fail MatchLLMApi after the path rewrite. With no next
	// handler, the request reaches serveNextOrNotFound -> http.NotFound. The
	// admission token is already consumed before that fallthrough.
	handler := NewHandler(gw, map[string]LLMApiHandler{"stub": nonMatchingLLMApiHandler{}}, nil, HandlerOptions{})

	first := httptest.NewRecorder()
	if err := handler.Dispatch(first, admitRequest("secret-key"), nil); err != nil {
		t.Fatalf("first Dispatch returned error: %v", err)
	}
	if first.Code != http.StatusNotFound {
		t.Fatalf("first status = %d, want %d (serveNextOrNotFound with nil next)", first.Code, http.StatusNotFound)
	}

	// The token is spent: the second matched-and-fallthrough request is now
	// rejected at admission with 429 rather than reaching serveNextOrNotFound.
	second := httptest.NewRecorder()
	if err := handler.Dispatch(second, admitRequest("secret-key"), nil); err != nil {
		t.Fatalf("second Dispatch returned error: %v", err)
	}
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", second.Code, http.StatusTooManyRequests)
	}
}

func admitRequest(bearer string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+bearer)
	return req
}

func TestRewriteLLMRoutePathStripsMatchedPrefix(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/tenant/v1/chat/completions", nil)
	rewritten := RewriteLLMRoutePath(req, "/tenant")

	if rewritten.URL.Path != "/v1/chat/completions" {
		t.Fatalf("rewritten path = %q, want /v1/chat/completions", rewritten.URL.Path)
	}
	if req.URL.Path != "/tenant/v1/chat/completions" {
		t.Fatalf("original path mutated to %q", req.URL.Path)
	}
}

func TestHandlerRejectsAgentDepthOverConfiguredLimit(t *testing.T) {
	gw := gateway.NewAgentGateway()
	if err := gw.Bootstrap(context.Background(), gateway.BootstrapOptions{
		StaticLLMRoutes: mustRouteConfigs(t, []llmroutepkg.LLMRoute{{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{
				ID:          "chat",
				Protocol:    llmroutepkg.RouteProtocol("stub"),
				MatchPolicy: llmroutepkg.RouteMatchPolicy{PathPrefix: "/"},
			},
			TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
				ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "missing"},
			},
		}}),
		UsageConfig: usage.Config{MaxAgentDepth: 2},
	}); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	handler := NewHandler(gw, map[string]LLMApiHandler{"stub": stubLLMApiHandler{}}, nil, HandlerOptions{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Agent-Depth", "2")

	if err := handler.Dispatch(rec, req, nil); err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := rec.Header().Get("X-Agent-Depth"); got != "3" {
		t.Fatalf("X-Agent-Depth = %q, want 3", got)
	}
}

func TestHandlerValidateAllowsMCPOnly(t *testing.T) {
	handler := NewHandler(nil, nil, nil, HandlerOptions{EnableMCP: true})
	if err := handler.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func mustRouteConfigs(t *testing.T, routes []llmroutepkg.LLMRoute) []llmroutepkg.AgentRouteConfig {
	t.Helper()

	out := make([]llmroutepkg.AgentRouteConfig, 0, len(routes))
	for _, route := range routes {
		cfg, err := route.ToConfig()
		if err != nil {
			t.Fatalf("ToConfig returned error: %v", err)
		}
		out = append(out, cfg)
	}
	return out
}
