package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	configstoreschema "github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	"github.com/agent-guide/agent-gateway/pkg/credential"
	credentialscheduler "github.com/agent-guide/agent-gateway/pkg/credential/scheduler"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	mcproutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/mcproute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type testProvider struct {
	cfg provider.ProviderConfig
}

type testContextCaptureProvider struct {
	cfg        provider.ProviderConfig
	credential *credential.Credential
}

type testCredentialAwareProvider struct {
	cfg      provider.ProviderConfig
	attempts *[]string
	failures map[string]error
}

type testGatewayCredentialRefresher struct {
	err error
}

func (r testGatewayCredentialRefresher) Refresh(context.Context, *credential.Credential) (*credential.Credential, error) {
	return nil, r.err
}

type testGatewayProviderConfigResolver struct {
	configs map[string]provider.ProviderConfig
}

func (p testProvider) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, nil
}

func (p testProvider) StreamChat(context.Context, *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}

func (p testProvider) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{
		ID:           "gpt-4.1-mini",
		DisplayName:  "gpt-4.1-mini",
		Capabilities: provider.ModelCapabilities{Streaming: true},
	}}, nil
}

func (p testProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{Streaming: true}
}

func (p testProvider) Config() provider.ProviderConfig {
	return p.cfg
}

func TestRequestSeparatesAnthropicReasoningFromFullNativeState(t *testing.T) {
	msg := provider.AttachReasoningParts(&schema.Message{Role: schema.Assistant},
		provider.NewReasoningOutputPart("inspect", "authentic-signature", nil))
	req := &provider.ChatRequest{Messages: []*schema.Message{msg}}
	if requestHasAnthropicNativeState(req) {
		t.Fatal("modeled signed reasoning unnecessarily required full native-content support")
	}
	if !requestHasAnthropicReasoningState(req) {
		t.Fatal("signed reasoning did not require Anthropic reasoning replay")
	}
}

func TestRoutedProviderLogsDialectAffinity(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	routed := &RoutedProvider{
		route:  &llmroutepkg.LLMRoute{AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "claude-route"}},
		logger: zap.New(core),
	}
	requirements := llmroutepkg.RequestRequirements{}.
		WithNativeDialect(provider.ProtocolDialectAnthropic).
		WithReasoningDialect(provider.ProtocolDialectAnthropic)
	routed.logDialectAffinity("claude-sonnet", requirements)

	entries := logs.FilterMessage("request protocol state restricts provider fallback").All()
	if len(entries) != 1 {
		t.Fatalf("affinity log entries = %d, want 1", len(entries))
	}
	context := entries[0].ContextMap()
	if context["route_id"] != "claude-route" || context["model"] != "claude-sonnet" {
		t.Fatalf("affinity log context = %+v", context)
	}
}

func (p *testContextCaptureProvider) Chat(ctx context.Context, _ *provider.ChatRequest) (*provider.ChatResponse, error) {
	p.credential, _ = provider.CredentialFromContext(ctx)
	return &provider.ChatResponse{}, nil
}

func (p *testContextCaptureProvider) StreamChat(context.Context, *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}

func (p *testContextCaptureProvider) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return nil, nil
}

func (p *testContextCaptureProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{}
}

func (p *testContextCaptureProvider) Config() provider.ProviderConfig {
	return p.cfg
}

func (p *testCredentialAwareProvider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	cred, _ := provider.CredentialFromContext(ctx)
	apiKey := ""
	if cred != nil {
		apiKey = cred.APIKey()
	}
	attempt := req.Model + "|" + apiKey
	if p.attempts != nil {
		*p.attempts = append(*p.attempts, attempt)
	}
	if err := p.failures[attempt]; err != nil {
		return nil, err
	}
	return &provider.ChatResponse{}, nil
}

func (p *testCredentialAwareProvider) StreamChat(context.Context, *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}

func (p *testCredentialAwareProvider) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return nil, nil
}

func (p *testCredentialAwareProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{}
}

func (p *testCredentialAwareProvider) Config() provider.ProviderConfig {
	return p.cfg
}

func (r testGatewayProviderConfigResolver) GetConfig(_ context.Context, providerID string) (provider.ProviderConfig, error) {
	return r.configs[providerID], nil
}

func TestNewRoutedProviderModelTargetRewritesDuringExecution(t *testing.T) {
	route := llmroutepkg.LLMRoute{
		AgentRouteConfig: llmroutepkg.AgentRouteConfig{
			ID:          "chat-prod",
			Protocol:    llmroutepkg.RouteProtocolOpenAI,
			MatchPolicy: llmroutepkg.RouteMatchPolicy{PathPrefix: "/v1"},
		},
		TargetPolicy: &llmroutepkg.RouteLogicalModelTargetPolicy{
			ModelTargets: []llmroutepkg.RouteModelTarget{{
				Name: "chat-fast",
				Candidates: []llmroutepkg.RouteModelCandidate{{
					ProviderID:    "openai",
					UpstreamModel: "gpt-4.1-mini",
				}},
			}},
			DefaultModel: "chat-fast",
		},
	}
	gw := NewAgentGateway()
	if err := gw.Bootstrap(context.Background(), BootstrapOptions{
		StaticLLMRoutes: mustLLMRouteConfigs(t, route),
		StaticProviders: map[string]provider.Provider{
			"openai": testProvider{cfg: provider.ProviderConfig{Id: "openai", ProviderType: "openai"}},
		},
		ConfigStoreBackend: &testGatewayConfigStore{
			modelStore: &testGatewayManagedModelStore{
				items: map[string]*modelcatalog.ManagedModel{
					"openai/gpt-4.1-mini": {
						ProviderID:    "openai",
						UpstreamModel: "gpt-4.1-mini",
						Enabled:       true,
					},
				},
			},
		},
	}); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	resolvedRoute, err := gw.ResolveRoute(context.Background(), req)
	if err != nil {
		t.Fatalf("ResolveRoute returned error: %v", err)
	}
	routedProvider, err := gw.NewRoutedProvider(resolvedRoute, llmroutepkg.RequestRequirements{})
	if err != nil {
		t.Fatalf("NewRoutedProvider returned error: %v", err)
	}
	if routedProvider == nil {
		t.Fatal("NewRoutedProvider returned nil provider")
	}

	chatReq := &provider.ChatRequest{Model: "chat-fast"}
	if _, err := routedProvider.Chat(context.Background(), chatReq); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if chatReq.Model != "gpt-4.1-mini" {
		t.Fatalf("Chat request model = %q, want gpt-4.1-mini", chatReq.Model)
	}
}

func TestResolveRejectsDisabledRoute(t *testing.T) {
	gw := NewAgentGateway()
	if err := gw.Bootstrap(context.Background(), BootstrapOptions{
		StaticLLMRoutes: mustLLMRouteConfigs(t, llmroutepkg.LLMRoute{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{
				ID:          "chat-prod",
				Protocol:    llmroutepkg.RouteProtocolOpenAI,
				Disabled:    true,
				MatchPolicy: llmroutepkg.RouteMatchPolicy{PathPrefix: "/v1"},
			},
			TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
				ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai"},
			},
		}),
		StaticProviders: map[string]provider.Provider{
			"openai": testProvider{cfg: provider.ProviderConfig{Id: "openai", ProviderType: "openai"}},
		},
	}); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, err := gw.ResolveRoute(context.Background(), req)
	if err == nil {
		t.Fatal("ResolveRoute returned nil error, want disabled route rejection")
	}
}

func mustLLMRouteConfigs(t *testing.T, routes ...llmroutepkg.LLMRoute) []routecore.AgentRouteConfig {
	t.Helper()

	out := make([]routecore.AgentRouteConfig, 0, len(routes))
	for _, route := range routes {
		cfg, err := route.ToConfig()
		if err != nil {
			t.Fatalf("ToConfig returned error: %v", err)
		}
		out = append(out, cfg)
	}
	return out
}

func TestBootstrapInitializesStaticMCPRoutes(t *testing.T) {
	staticRoute, err := (mcproutepkg.MCPRouteConfig{
		AgentRouteConfig: mcproutepkg.AgentRouteConfig{
			ID:          "mcp-route",
			MatchPolicy: mcproutepkg.RouteMatch{PathPrefix: "/mcp"},
			AuthPolicy:  mcproutepkg.RouteAuthPolicy{RequireVirtualKey: true},
		},
		ServiceID: "svc-1",
	}).ToConfig()
	if err != nil {
		t.Fatalf("ToConfig returned error: %v", err)
	}

	gw := NewAgentGateway()
	if err := gw.Bootstrap(context.Background(), BootstrapOptions{
		StaticMCPRoutes: []routecore.AgentRouteConfig{staticRoute},
	}); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	manager := gw.MCPRouteConfigManager()
	if manager == nil {
		t.Fatal("MCPRouteConfigManager() = nil")
	}
	if !manager.IsStatic("mcp-route") {
		t.Fatal("expected mcp-route to be initialized as static")
	}

	resolver := gw.MCPRouteResolver()
	if resolver == nil {
		t.Fatal("MCPRouteResolver() = nil")
	}
	route, err := resolver.ResolveByID(context.Background(), "mcp-route")
	if err != nil {
		t.Fatalf("ResolveByID returned error: %v", err)
	}
	if route.ServiceID != "svc-1" {
		t.Fatalf("ServiceID = %q, want svc-1", route.ServiceID)
	}

	err = resolver.DeleteConfig(context.Background(), "mcp-route")
	if err == nil {
		t.Fatal("DeleteConfig returned nil error, want static route rejection")
	}
	if !errors.Is(err, routecore.ErrStaticRouteReadOnly) {
		t.Fatalf("DeleteConfig returned %v, want static route read-only error", err)
	}
}

type testGatewayProviderStore struct {
	items map[string]*provider.ProviderConfig
}

type testGatewayModelCatalogResolver struct {
	models map[string]modelcatalog.ResolvedManagedModel
}

func (s *testGatewayProviderStore) List(ctx context.Context) ([]any, error) {
	return s.ListByTag(ctx, "")
}

func (s *testGatewayProviderStore) ListByTag(_ context.Context, name string) ([]any, error) {
	out := make([]any, 0, len(s.items))
	for _, item := range s.items {
		if name != "" && item.ProviderType != name {
			continue
		}
		cloned := *item
		out = append(out, &cloned)
	}
	return out, nil
}

func (s *testGatewayProviderStore) ListByTagPrefix(ctx context.Context, tagPrefix string) ([]any, error) {
	return s.ListByTag(ctx, tagPrefix)
}

func (s *testGatewayProviderStore) Create(_ context.Context, obj any) error {
	cfg, ok := obj.(*provider.ProviderConfig)
	if !ok {
		return nil
	}
	if s.items == nil {
		s.items = map[string]*provider.ProviderConfig{}
	}
	cloned := *cfg
	s.items[cloned.Id] = &cloned
	return nil
}

func (s *testGatewayProviderStore) Update(ctx context.Context, obj any) error {
	return s.Create(ctx, obj)
}

func (s *testGatewayProviderStore) Delete(_ context.Context, keyParts ...any) error {
	id, _ := keyParts[0].(string)
	delete(s.items, id)
	return nil
}

func (s *testGatewayProviderStore) Get(_ context.Context, keyParts ...any) (any, error) {
	id, _ := keyParts[0].(string)
	item := s.items[id]
	if item == nil {
		return nil, configstore.ErrNotFound
	}
	cloned := *item
	return &cloned, nil
}

func (s *testGatewayProviderStore) GetByIndex(context.Context, string, any) (any, error) {
	return nil, configstore.ErrNotFound
}

func (testGatewayModelCatalogResolver) GetManagedModel(context.Context, string, string) (*modelcatalog.ManagedModel, bool, error) {
	return nil, false, nil
}

func (r testGatewayModelCatalogResolver) GetResolvedManagedModel(_ context.Context, providerID string, upstreamModel string) (*modelcatalog.ResolvedManagedModel, bool, error) {
	item, ok := r.models[providerID+"/"+upstreamModel]
	if !ok {
		return nil, false, nil
	}
	cloned := item
	return &cloned, true, nil
}

type testGatewayConfigStore struct {
	providerStore configstore.ConfigStore
	modelStore    configstore.ConfigStore
}

func (s *testGatewayConfigStore) Register(string, configstore.StoreSchema) error {
	return nil
}

func (s *testGatewayConfigStore) Get(name string) (configstore.ConfigStore, error) {
	if name == configstoreschema.StoreProviders {
		return s.providerStore, nil
	}
	if name == configstoreschema.StoreManagedModels {
		return s.modelStore, nil
	}
	return nil, nil
}

type testGatewayManagedModelStore struct {
	items map[string]*modelcatalog.ManagedModel
}

func (s *testGatewayManagedModelStore) List(context.Context) ([]any, error) {
	out := make([]any, 0, len(s.items))
	for _, item := range s.items {
		cloned := *item
		out = append(out, &cloned)
	}
	return out, nil
}

func (s *testGatewayManagedModelStore) ListByTag(context.Context, string) ([]any, error) {
	return nil, nil
}

func (s *testGatewayManagedModelStore) ListByTagPrefix(context.Context, string) ([]any, error) {
	return nil, nil
}

func (s *testGatewayManagedModelStore) Create(_ context.Context, obj any) error {
	model, ok := obj.(*modelcatalog.ManagedModel)
	if !ok {
		return nil
	}
	if s.items == nil {
		s.items = map[string]*modelcatalog.ManagedModel{}
	}
	cloned := *model
	s.items[cloned.ProviderID+"/"+cloned.UpstreamModel] = &cloned
	return nil
}

func (s *testGatewayManagedModelStore) Update(ctx context.Context, obj any) error {
	return s.Create(ctx, obj)
}

func (s *testGatewayManagedModelStore) Delete(_ context.Context, keyParts ...any) error {
	providerID, _ := keyParts[0].(string)
	upstreamModel, _ := keyParts[1].(string)
	delete(s.items, providerID+"/"+upstreamModel)
	return nil
}

func (s *testGatewayManagedModelStore) Get(_ context.Context, keyParts ...any) (any, error) {
	providerID, _ := keyParts[0].(string)
	upstreamModel, _ := keyParts[1].(string)
	item := s.items[providerID+"/"+upstreamModel]
	if item == nil {
		return nil, configstore.ErrNotFound
	}
	cloned := *item
	return &cloned, nil
}

func (s *testGatewayManagedModelStore) GetByIndex(context.Context, string, any) (any, error) {
	return nil, configstore.ErrNotFound
}

func TestBootstrapDoesNotSyncDynamicProviderConfigCredentials(t *testing.T) {
	credMgr := credential.NewManager(nil)
	scheduler := credentialscheduler.NewScheduler(nil)
	if listener, ok := scheduler.(credential.CredentialLifecycleListener); ok {
		credMgr.AddListener(listener)
	}

	gw := NewAgentGateway()
	if err := gw.Bootstrap(context.Background(), BootstrapOptions{
		ConfigStoreBackend: &testGatewayConfigStore{
			providerStore: &testGatewayProviderStore{
				items: map[string]*provider.ProviderConfig{
					"deepseek-test": {
						Id:           "deepseek-test",
						ProviderType: "deepseek",
						APIKey:       "deepseek-key",
						BaseURL:      "https://deepseek.example",
					},
				},
			},
		},
		CredentialManager:   credMgr,
		CredentialScheduler: scheduler,
	}); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	if cred := credMgr.GetCredential("provider-config-api-key:deepseek-test"); cred != nil {
		t.Fatalf("provider config credential should not be registered, got %#v", cred)
	}
}

func TestDirectProviderFallsBackToProviderConfigAPIKey(t *testing.T) {
	capture := &testContextCaptureProvider{
		cfg: provider.ProviderConfig{
			Id:           "openai-main",
			ProviderType: "openai",
			APIKey:       "provider-config-key",
		},
	}
	routedProvider := &RoutedProvider{
		route: &llmroutepkg.LLMRoute{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "chat-prod", Protocol: llmroutepkg.RouteProtocolOpenAI},
			TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
				ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai-main"},
			},
		},
		providerResolver: ProviderResolverFunc(func(context.Context, string) (provider.Provider, error) {
			return capture, nil
		}),
		modelCatalog: testGatewayModelCatalogResolver{},
		providerConfigs: testGatewayProviderConfigResolver{
			configs: map[string]provider.ProviderConfig{
				"openai-main": {Id: "openai-main", ProviderType: "openai", APIKey: "provider-config-key"},
			},
		},
		credentialMgr: credential.NewManager(nil),
		scheduler:     credentialscheduler.NewScheduler(nil),
	}

	if _, err := routedProvider.Chat(context.Background(), &provider.ChatRequest{Model: "gpt-4.1"}); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if capture.credential != nil {
		t.Fatalf("credential = %#v, want provider config fallback without managed credential", capture.credential)
	}
}

func TestRoutedProviderInjectsExplicitCredentialValuesIntoContext(t *testing.T) {
	credMgr := credential.NewManager(nil)
	scheduler := credentialscheduler.NewScheduler(nil)
	if listener, ok := scheduler.(credential.CredentialLifecycleListener); ok {
		credMgr.AddListener(listener)
	}

	if err := credMgr.RegisterCredential(context.Background(), &credential.Credential{
		ID:           "cred-openai-managed",
		ProviderType: "openai",
		ProviderID:   "openai-main",
		Scope:        "id:openai-main",
		Type:         credential.TypeAPIKey,
		Attributes: map[string]string{
			"api_key": "managed-key",
		},
	}); err != nil {
		t.Fatalf("register credential: %v", err)
	}

	capture := &testContextCaptureProvider{
		cfg: provider.ProviderConfig{
			Id:           "openai-main",
			ProviderType: "openai",
		},
	}
	routedProvider := &RoutedProvider{
		route: &llmroutepkg.LLMRoute{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "chat-prod", Protocol: llmroutepkg.RouteProtocolOpenAI},
			TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
				ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai-main"},
			},
		},
		providerResolver: ProviderResolverFunc(func(context.Context, string) (provider.Provider, error) {
			return capture, nil
		}),
		modelCatalog: testGatewayModelCatalogResolver{},
		providerConfigs: testGatewayProviderConfigResolver{
			configs: map[string]provider.ProviderConfig{
				"openai-main": {Id: "openai-main", ProviderType: "openai"},
			},
		},
		credentialMgr: credMgr,
		scheduler:     scheduler,
	}

	if _, err := routedProvider.Chat(context.Background(), &provider.ChatRequest{Model: "gpt-4.1"}); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if capture.credential == nil {
		t.Fatal("credential = nil, want managed credential")
	}
	if capture.credential.APIKey() != "managed-key" {
		t.Fatalf("api key = %q, want managed-key", capture.credential.APIKey())
	}
}

func TestRoutedProviderReportsCredentialRefreshFailure(t *testing.T) {
	const sensitiveDetail = "upstream body contains access_token=secret"
	credMgr := credential.NewManager(nil)
	scheduler := credentialscheduler.NewScheduler(nil)
	if listener, ok := scheduler.(credential.CredentialLifecycleListener); ok {
		credMgr.AddListener(listener)
	}
	credMgr.SetRefresher(testGatewayCredentialRefresher{err: errors.New(sensitiveDetail)})
	logCore, logs := observer.New(zap.ErrorLevel)

	if err := credMgr.RegisterCredential(context.Background(), &credential.Credential{
		ID:           "oauth-openai-managed",
		ProviderType: "openai",
		ProviderID:   "openai-main",
		Scope:        "id:openai-main",
		Type:         credential.TypeOAuthToken,
		Attributes:   map[string]string{"api_key": "stale-key"},
		Metadata: map[string]any{
			credential.MetadataRefreshExpiryDeltaKey: "5m",
			"expired":                                time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		},
	}); err != nil {
		t.Fatalf("register credential: %v", err)
	}

	routedProvider := &RoutedProvider{
		route: &llmroutepkg.LLMRoute{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "chat-prod", Protocol: llmroutepkg.RouteProtocolOpenAI},
			TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
				ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai-main"},
			},
		},
		providerResolver: ProviderResolverFunc(func(context.Context, string) (provider.Provider, error) {
			return &testContextCaptureProvider{cfg: provider.ProviderConfig{Id: "openai-main", ProviderType: "openai"}}, nil
		}),
		modelCatalog: testGatewayModelCatalogResolver{},
		providerConfigs: testGatewayProviderConfigResolver{configs: map[string]provider.ProviderConfig{
			"openai-main": {Id: "openai-main", ProviderType: "openai"},
		}},
		credentialMgr: credMgr,
		scheduler:     scheduler,
		logger:        zap.New(logCore),
	}

	_, err := routedProvider.Chat(context.Background(), &provider.ChatRequest{Model: "gpt-4.1"})
	if err == nil || !strings.Contains(err.Error(), `credential refresh failed for credential "oauth-openai-managed"`) {
		t.Fatalf("Chat error = %v, want safe credential refresh failure", err)
	}
	if strings.Contains(err.Error(), sensitiveDetail) || strings.Contains(err.Error(), "access_token") {
		t.Fatalf("Chat error leaked refresher detail: %v", err)
	}
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("refresh error log entries = %d, want 1", len(entries))
	}
	if loggedErr, _ := entries[0].ContextMap()["error"].(string); !strings.Contains(loggedErr, sensitiveDetail) {
		t.Fatalf("logged error = %q, want refresher detail", loggedErr)
	}
}

type testCountingRefresher struct {
	calls int
	err   error
}

func (r *testCountingRefresher) Refresh(context.Context, *credential.Credential) (*credential.Credential, error) {
	r.calls++
	return nil, r.err
}

func TestRoutedProviderCoolsCredentialAfterRefreshFailure(t *testing.T) {
	credMgr := credential.NewManager(nil)
	scheduler := credentialscheduler.NewScheduler(nil)
	if listener, ok := scheduler.(credential.CredentialLifecycleListener); ok {
		credMgr.AddListener(listener)
	}
	refresher := &testCountingRefresher{err: errors.New("refresh backend down")}
	credMgr.SetRefresher(refresher)

	if err := credMgr.RegisterCredential(context.Background(), &credential.Credential{
		ID:           "oauth-openai-managed",
		ProviderType: "openai",
		ProviderID:   "openai-main",
		Scope:        "id:openai-main",
		Type:         credential.TypeOAuthToken,
		Attributes:   map[string]string{"api_key": "stale-key"},
		Metadata: map[string]any{
			credential.MetadataRefreshExpiryDeltaKey: "5m",
			"expired":                                time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		},
	}); err != nil {
		t.Fatalf("register credential: %v", err)
	}

	routedProvider := &RoutedProvider{
		route: &llmroutepkg.LLMRoute{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "chat-prod", Protocol: llmroutepkg.RouteProtocolOpenAI},
			TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
				ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai-main"},
			},
		},
		providerResolver: ProviderResolverFunc(func(context.Context, string) (provider.Provider, error) {
			return &testContextCaptureProvider{cfg: provider.ProviderConfig{Id: "openai-main", ProviderType: "openai"}}, nil
		}),
		modelCatalog: testGatewayModelCatalogResolver{},
		providerConfigs: testGatewayProviderConfigResolver{configs: map[string]provider.ProviderConfig{
			"openai-main": {Id: "openai-main", ProviderType: "openai"},
		}},
		credentialMgr: credMgr,
		scheduler:     scheduler,
	}

	if _, err := routedProvider.Chat(context.Background(), &provider.ChatRequest{Model: "gpt-4.1"}); err == nil {
		t.Fatal("first Chat returned nil error, want refresh failure")
	}
	if refresher.calls != 1 {
		t.Fatalf("refresher calls after first Chat = %d, want 1", refresher.calls)
	}

	if _, err := routedProvider.Chat(context.Background(), &provider.ChatRequest{Model: "gpt-4.1"}); err == nil {
		t.Fatal("second Chat returned nil error, want credential unavailable")
	}
	if refresher.calls != 1 {
		t.Fatalf("refresher calls after second Chat = %d, want 1 (cooled credential must not refresh again)", refresher.calls)
	}
}

func TestRoutedProviderRetriesAnotherCredentialBeforeModelFallback(t *testing.T) {
	credMgr := credential.NewManager(nil)
	scheduler := credentialscheduler.NewScheduler(nil)
	if listener, ok := scheduler.(credential.CredentialLifecycleListener); ok {
		credMgr.AddListener(listener)
	}

	for _, cred := range []*credential.Credential{
		{
			ID:           "cred-openai-bad",
			ProviderType: "openai",
			ProviderID:   "openai-main",
			Scope:        "id:openai-main",
			Type:         credential.TypeAPIKey,
			Attributes: map[string]string{
				"api_key": "bad-key",
			},
		},
		{
			ID:           "cred-openai-good",
			ProviderType: "openai",
			ProviderID:   "openai-main",
			Scope:        "id:openai-main",
			Type:         credential.TypeAPIKey,
			Attributes: map[string]string{
				"api_key": "good-key",
			},
		},
	} {
		if err := credMgr.RegisterCredential(context.Background(), cred); err != nil {
			t.Fatalf("register credential %q: %v", cred.ID, err)
		}
	}

	var attempts []string
	prov := &testCredentialAwareProvider{
		cfg:      provider.ProviderConfig{Id: "openai-main", ProviderType: "openai"},
		attempts: &attempts,
		failures: map[string]error{
			"gpt-4.1|bad-key": statuserr.New(http.StatusServiceUnavailable, "upstream unavailable"),
		},
	}
	routedProvider := &RoutedProvider{
		route: &llmroutepkg.LLMRoute{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "chat-prod", Protocol: llmroutepkg.RouteProtocolOpenAI},
			TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
				ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai-main"},
			},
		},
		providerResolver: ProviderResolverFunc(func(context.Context, string) (provider.Provider, error) {
			return prov, nil
		}),
		modelCatalog: testGatewayModelCatalogResolver{},
		providerConfigs: testGatewayProviderConfigResolver{
			configs: map[string]provider.ProviderConfig{
				"openai-main": {Id: "openai-main", ProviderType: "openai"},
			},
		},
		credentialMgr: credMgr,
		scheduler:     scheduler,
	}

	if _, err := routedProvider.Chat(context.Background(), &provider.ChatRequest{Model: "gpt-4.1"}); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if !slices.Equal(attempts, []string{"gpt-4.1|bad-key", "gpt-4.1|good-key"}) {
		t.Fatalf("attempts = %v, want bad credential retry before success", attempts)
	}
}

func TestRoutedProviderFallsBackToAnotherModelAfterCandidateCredentialsExhausted(t *testing.T) {
	credMgr := credential.NewManager(nil)
	scheduler := credentialscheduler.NewScheduler(nil)
	if listener, ok := scheduler.(credential.CredentialLifecycleListener); ok {
		credMgr.AddListener(listener)
	}

	for _, cred := range []*credential.Credential{
		{
			ID:           "cred-openai-main",
			ProviderType: "openai",
			ProviderID:   "openai-main",
			Scope:        "id:openai-main",
			Type:         credential.TypeAPIKey,
			Attributes: map[string]string{
				"api_key": "main-key",
			},
		},
		{
			ID:           "cred-openai-backup",
			ProviderType: "openai",
			ProviderID:   "openai-backup",
			Scope:        "id:openai-backup",
			Type:         credential.TypeAPIKey,
			Attributes: map[string]string{
				"api_key": "backup-key",
			},
		},
	} {
		if err := credMgr.RegisterCredential(context.Background(), cred); err != nil {
			t.Fatalf("register credential %q: %v", cred.ID, err)
		}
	}

	var attempts []string
	providers := map[string]provider.Provider{
		"openai-main": &testCredentialAwareProvider{
			cfg:      provider.ProviderConfig{Id: "openai-main", ProviderType: "openai"},
			attempts: &attempts,
			failures: map[string]error{
				"gpt-4.1|main-key": statuserr.New(http.StatusServiceUnavailable, "main unavailable"),
			},
		},
		"openai-backup": &testCredentialAwareProvider{
			cfg:      provider.ProviderConfig{Id: "openai-backup", ProviderType: "openai"},
			attempts: &attempts,
			failures: map[string]error{},
		},
	}
	routedProvider := &RoutedProvider{
		route: &llmroutepkg.LLMRoute{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "chat-prod", Protocol: llmroutepkg.RouteProtocolOpenAI},
			TargetPolicy: &llmroutepkg.RouteLogicalModelTargetPolicy{
				DefaultModel:          "chat-fast",
				ModelSelectorStrategy: llmroutepkg.RouteSelectionStrategyPriority,
				Fallback:              llmroutepkg.RouteFallbackPolicy{Enabled: true, MaxNum: 1},
				ModelTargets: []llmroutepkg.RouteModelTarget{{
					Name: "chat-fast",
					Candidates: []llmroutepkg.RouteModelCandidate{
						{ProviderID: "openai-main", UpstreamModel: "gpt-4.1", Priority: 2},
						{ProviderID: "openai-backup", UpstreamModel: "gpt-4.1-mini", Priority: 1},
					},
				}},
			},
		},
		providerResolver: ProviderResolverFunc(func(_ context.Context, providerID string) (provider.Provider, error) {
			return providers[providerID], nil
		}),
		modelCatalog: testGatewayModelCatalogResolver{
			models: map[string]modelcatalog.ResolvedManagedModel{
				"openai-main/gpt-4.1": {
					ManagedModel: modelcatalog.ManagedModel{
						ProviderID:      "openai-main",
						UpstreamModel:   "gpt-4.1",
						Enabled:         true,
						CredentialScope: credential.ProviderIDCredentialScope("openai-main"),
					},
				},
				"openai-backup/gpt-4.1-mini": {
					ManagedModel: modelcatalog.ManagedModel{
						ProviderID:      "openai-backup",
						UpstreamModel:   "gpt-4.1-mini",
						Enabled:         true,
						CredentialScope: credential.ProviderIDCredentialScope("openai-backup"),
					},
				},
			},
		},
		providerConfigs: testGatewayProviderConfigResolver{
			configs: map[string]provider.ProviderConfig{
				"openai-main":   {Id: "openai-main", ProviderType: "openai"},
				"openai-backup": {Id: "openai-backup", ProviderType: "openai"},
			},
		},
		credentialMgr: credMgr,
		scheduler:     scheduler,
	}

	req := &provider.ChatRequest{Model: "chat-fast"}
	if _, err := routedProvider.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if req.Model != "gpt-4.1-mini" {
		t.Fatalf("request model = %q, want fallback model gpt-4.1-mini", req.Model)
	}
	if !slices.Equal(attempts, []string{"gpt-4.1|main-key", "gpt-4.1-mini|backup-key"}) {
		t.Fatalf("attempts = %v, want same candidate exhaustion before model fallback", attempts)
	}
}
