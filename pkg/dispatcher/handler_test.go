package dispatcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
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
