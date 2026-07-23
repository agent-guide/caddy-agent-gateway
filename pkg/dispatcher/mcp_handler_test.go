package dispatcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/configstore"
	configstoreschema "github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	"github.com/agent-guide/agent-gateway/pkg/gateway/mcproute"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	basemcp "github.com/agent-guide/agent-gateway/pkg/mcp"
	mcpruntime "github.com/agent-guide/agent-gateway/pkg/mcp/runtime"
	"github.com/agent-guide/agent-gateway/pkg/mcp/transport"
)

type testConfigStoreBackend struct {
	store configstore.ConfigStore
}

func (b *testConfigStoreBackend) Register(string, configstore.StoreSchema) error {
	return nil
}

func (b *testConfigStoreBackend) Get(name string) (configstore.ConfigStore, error) {
	switch name {
	case configstoreschema.StoreProviders,
		configstoreschema.StoreCredentials,
		configstoreschema.StoreRoutes,
		configstoreschema.StoreVirtualKeys,
		configstoreschema.StoreManagedModels,
		configstoreschema.StoreMCPServices,
		configstoreschema.StoreACPServices,
		configstoreschema.StoreAgents:
		return b.store, nil
	default:
		return nil, configstore.ErrUnknownStoreName
	}
}

type emptyConfigStore struct{}

func (emptyConfigStore) List(context.Context) ([]any, error)                    { return nil, nil }
func (emptyConfigStore) ListByTag(context.Context, string) ([]any, error)       { return nil, nil }
func (emptyConfigStore) ListByTagPrefix(context.Context, string) ([]any, error) { return nil, nil }
func (emptyConfigStore) Create(context.Context, any) error                      { return nil }
func (emptyConfigStore) Update(context.Context, any) error                      { return nil }
func (emptyConfigStore) Delete(context.Context, ...any) error                   { return nil }
func (emptyConfigStore) Get(context.Context, ...any) (any, error) {
	return nil, configstore.ErrNotFound
}
func (emptyConfigStore) GetByIndex(context.Context, string, any) (any, error) {
	return nil, configstore.ErrNotFound
}

type singleVirtualKeyStore struct {
	emptyConfigStore
	keyID      string
	key        string
	route      string
	rateLimits *virtualkeypkg.VirtualKeyRateLimits
}

func (s singleVirtualKeyStore) GetByIndex(_ context.Context, _ string, value any) (any, error) {
	key, _ := value.(string)
	if key != s.key {
		return nil, configstore.ErrNotFound
	}
	return &virtualkeypkg.VirtualKey{
		ID:              s.keyID,
		Key:             s.key,
		AllowedRouteIDs: []string{s.route},
		RateLimits:      s.rateLimits,
	}, nil
}

func TestIsNotification(t *testing.T) {
	t.Parallel()

	if !isNotification(transport.Message{Method: "notifications/progress"}) {
		t.Fatal("expected notifications/progress without id to be treated as notification")
	}
	if isNotification(transport.Message{ID: 1, Method: "notifications/progress"}) {
		t.Fatal("expected notification with id to be treated as request")
	}
	if isNotification(transport.Message{Method: "tools/list"}) {
		t.Fatal("expected request method to not be treated as notification")
	}
}

func TestDecodeCompletionParamsPromptRef(t *testing.T) {
	t.Parallel()

	ref, argument, args, err := decodeCompletionParams(map[string]any{
		"ref": map[string]any{
			"type": "ref/prompt",
			"name": "summarize",
		},
		"argument": map[string]any{
			"name":  "language",
			"value": "zh",
		},
		"arguments": map[string]any{
			"tone": "formal",
		},
	})
	if err != nil {
		t.Fatalf("decodeCompletionParams returned error: %v", err)
	}
	if ref != (basemcp.CompletionReference{Type: "ref/prompt", Name: "summarize"}) {
		t.Fatalf("unexpected ref: %#v", ref)
	}
	if argument != (basemcp.CompletionArgument{Name: "language", Value: "zh"}) {
		t.Fatalf("unexpected argument: %#v", argument)
	}
	if args["tone"] != "formal" {
		t.Fatalf("unexpected arguments: %#v", args)
	}
}

func TestDecodeCompletionParamsRejectsInvalidArgumentsMap(t *testing.T) {
	t.Parallel()

	_, _, _, err := decodeCompletionParams(map[string]any{
		"ref": map[string]any{
			"type": "ref/resource",
			"uri":  "file:///tmp/{name}",
		},
		"argument": map[string]any{
			"name": "name",
		},
		"arguments": map[string]any{
			"limit": 10,
		},
	})
	if err == nil {
		t.Fatal("expected error for non-string completion arguments")
	}
}

func TestCancelRequestCancelsInFlightRequest(t *testing.T) {
	t.Parallel()

	handler := &Handler{gateway: gateway.NewAgentGateway()}
	route := &mcproute.MCPRoute{AgentRouteConfig: mcproute.AgentRouteConfig{ID: "route-1"}, ServiceID: "svc-1"}
	msg := transport.Message{
		JSONRPC: "2.0",
		ID:      "req-1",
		Method:  "tools/call",
		Params: map[string]any{
			"_meta": map[string]any{"progressToken": "p1"},
		},
	}
	ctx, finish := handler.beginRequest(context.Background(), route, msg)
	defer finish(nil)

	cancelled, err := handler.cancelRequest(route, "req-1", "user requested")
	if err != nil {
		t.Fatalf("cancelRequest returned error: %v", err)
	}
	if !cancelled {
		t.Fatal("expected in-flight request to be cancelled")
	}
	if reason := handler.cancelReason(route, "req-1"); reason != "user requested" {
		t.Fatalf("unexpected cancel reason: %q", reason)
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("expected context to be cancelled, got %v", ctx.Err())
	}
}

func TestCancelRequestRejectsInitialize(t *testing.T) {
	t.Parallel()

	handler := &Handler{gateway: gateway.NewAgentGateway()}
	route := &mcproute.MCPRoute{AgentRouteConfig: mcproute.AgentRouteConfig{ID: "route-1"}, ServiceID: "svc-1"}
	_, finish := handler.beginRequest(context.Background(), route, transport.Message{
		JSONRPC: "2.0",
		ID:      "init-1",
		Method:  "initialize",
	})
	defer finish(nil)

	cancelled, err := handler.cancelRequest(route, "init-1", "bad client")
	if err == nil {
		t.Fatal("expected initialize cancellation to be rejected")
	}
	if cancelled {
		t.Fatal("expected initialize request to remain uncancelled")
	}
}

func TestHandleProgressNotificationStoresState(t *testing.T) {
	t.Parallel()

	handler := &Handler{gateway: gateway.NewAgentGateway()}
	route := &mcproute.MCPRoute{AgentRouteConfig: mcproute.AgentRouteConfig{ID: "route-1"}, ServiceID: "svc-1"}
	_, finish := handler.beginRequest(context.Background(), route, transport.Message{
		JSONRPC: "2.0",
		ID:      "req-2",
		Method:  "tools/call",
		Params: map[string]any{
			"_meta": map[string]any{"progressToken": "progress-1"},
		},
	})
	defer finish(nil)

	if err := handler.handleProgressNotification(route, map[string]any{
		"progressToken": "progress-1",
		"progress":      float64(2),
		"total":         float64(5),
		"message":       "working",
	}); err != nil {
		t.Fatalf("handleProgressNotification returned error: %v", err)
	}

	progresses := handler.gateway.MCPRuntimeRegistry().ListProgress()
	if len(progresses) != 1 {
		t.Fatalf("expected one progress notification, got %#v", progresses)
	}
	progress := progresses[0]
	if progress.ProgressTokenKey != mcpruntime.RouteProgressTokenKey("route-1", "progress-1") {
		t.Fatal("expected progress notification to be recorded")
	}
	if progress.RequestKey != mcpruntime.RouteRequestKey("route-1", "req-2") {
		t.Fatalf("unexpected request key: %q", progress.RequestKey)
	}
	if progress.Message != "working" {
		t.Fatalf("unexpected progress message: %q", progress.Message)
	}
	if progress.Total == nil || *progress.Total != 5 {
		t.Fatalf("unexpected total: %#v", progress.Total)
	}
}

func TestDispatchJSONRPCUsesGatewayVirtualKeyResolution(t *testing.T) {
	t.Parallel()

	gw := gateway.NewAgentGateway()
	if err := gw.Bootstrap(context.Background(), gateway.BootstrapOptions{
		ConfigStoreBackend: &testConfigStoreBackend{store: singleVirtualKeyStore{
			keyID: "vk-1",
			key:   "secret-key",
			route: "mcp-route",
		}},
	}); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	handler := NewHandler(gw, nil, nil, HandlerOptions{})
	handler.mcpEnabled = true

	body := `{"jsonrpc":"2.0","id":"1","method":"ping"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-key")
	rec := httptest.NewRecorder()
	route := &mcproute.MCPRoute{
		AgentRouteConfig: mcproute.AgentRouteConfig{
			ID:         "mcp-route",
			AuthPolicy: mcproute.RouteAuthPolicy{RequireVirtualKey: true},
		},
		ServiceID: "svc-1",
	}

	if err := handler.dispatchJSONRPC(rec, req, route); err != nil {
		t.Fatalf("dispatchJSONRPC returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `"result":{}`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}
