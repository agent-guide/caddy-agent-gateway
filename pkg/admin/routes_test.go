package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/cliauth"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	configstoreschema "github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	configstoresqlite "github.com/agent-guide/agent-gateway/pkg/configstore/sqlite"
	dispatcherpkg "github.com/agent-guide/agent-gateway/pkg/dispatcher"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	agentroute "github.com/agent-guide/agent-gateway/pkg/gateway/agentroute"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	mcproute "github.com/agent-guide/agent-gateway/pkg/gateway/mcproute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	"github.com/agent-guide/agent-gateway/pkg/llm/credentialmgr"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/deepseek"
	_ "github.com/agent-guide/agent-gateway/pkg/llm/provider/openai"
	mcpruntime "github.com/agent-guide/agent-gateway/pkg/mcp/runtime"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
	"github.com/cloudwego/eino/schema"
)

type testConfigStore struct {
	providerStore   configstore.ConfigStore
	routeStore      configstore.ConfigStore
	mcpServiceStore configstore.ConfigStore
	virtualKeyStore configstore.ConfigStore
	modelStore      configstore.ConfigStore
	agentStore      configstore.ConfigStore
}

type adminCaptureSink struct {
	events []any
}

func (s *adminCaptureSink) Enqueue(v any) bool {
	s.events = append(s.events, v)
	return true
}

func newTestAgentGateway(configStoreBackend configstore.ConfigStoreBackend, cliauthMgr *cliauth.Manager, cliauthRefresher *cliauth.AutoRefresher, staticRoutes []llmroutepkg.LLMRoute, _ []virtualkeypkg.VirtualKey, staticProviders ...map[string]provider.Provider) *gateway.AgentGateway {
	var providers map[string]provider.Provider
	if len(staticProviders) > 0 {
		providers = staticProviders[0]
	}
	staticLLMRoutes := make([]routecore.AgentRouteConfig, 0, len(staticRoutes))
	for _, route := range staticRoutes {
		cfg, err := route.ToConfig()
		if err != nil {
			panic(err)
		}
		staticLLMRoutes = append(staticLLMRoutes, cfg)
	}
	agentGateway := gateway.NewAgentGateway()
	if err := agentGateway.Bootstrap(context.Background(), gateway.BootstrapOptions{
		ConfigStoreBackend: configStoreBackend,
		StaticLLMRoutes:    staticLLMRoutes,
		StaticProviders:    providers,
		CLIAuthManager:     cliauthMgr,
		CLIAuthRefresher:   cliauthRefresher,
	}); err != nil {
		panic(err)
	}
	return agentGateway
}

func (s *testConfigStore) Register(string, configstore.StoreSchema) error {
	return nil
}

func (s *testConfigStore) Get(name string) (configstore.ConfigStore, error) {
	switch name {
	case configstoreschema.StoreProviders:
		return s.providerStore, nil
	case configstoreschema.StoreRoutes:
		return s.routeStore, nil
	case configstoreschema.StoreMCPServices:
		return s.mcpServiceStore, nil
	case configstoreschema.StoreVirtualKeys:
		return s.virtualKeyStore, nil
	case configstoreschema.StoreManagedModels:
		return s.modelStore, nil
	case configstoreschema.StoreAgents:
		return s.agentStore, nil
	case configstoreschema.StoreCredentials:
		return nil, nil
	default:
		return nil, configstore.ErrNotFound
	}
}

type testModelStore struct {
	items map[string]*modelcatalog.ManagedModel
}

func (s *testModelStore) List(_ context.Context) ([]any, error) {
	out := make([]any, 0, len(s.items))
	for _, item := range s.items {
		cloned := *item
		out = append(out, &cloned)
	}
	return out, nil
}

func (s *testModelStore) ListByTag(context.Context, string) ([]any, error) {
	return s.List(context.Background())
}

func (s *testModelStore) ListByTagPrefix(context.Context, string) ([]any, error) {
	return s.List(context.Background())
}

func (s *testModelStore) Get(_ context.Context, keyParts ...any) (any, error) {
	providerID, _ := keyParts[0].(string)
	upstreamModel, _ := keyParts[1].(string)
	item, ok := s.items[providerID+"\x00"+upstreamModel]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	cloned := *item
	return &cloned, nil
}

func (s *testModelStore) Create(_ context.Context, obj any) error {
	item, ok := obj.(*modelcatalog.ManagedModel)
	if !ok {
		return errors.New("unexpected type")
	}
	if s.items == nil {
		s.items = map[string]*modelcatalog.ManagedModel{}
	}
	key := item.ProviderID + "\x00" + item.UpstreamModel
	if _, exists := s.items[key]; exists {
		return errors.New("managed model already exists")
	}
	cloned := *item
	s.items[key] = &cloned
	return nil
}

func (s *testModelStore) Update(_ context.Context, obj any) error {
	item, ok := obj.(*modelcatalog.ManagedModel)
	if !ok {
		return errors.New("unexpected type")
	}
	if s.items == nil {
		s.items = map[string]*modelcatalog.ManagedModel{}
	}
	key := item.ProviderID + "\x00" + item.UpstreamModel
	if _, exists := s.items[key]; !exists {
		return configstore.ErrNotFound
	}
	cloned := *item
	s.items[key] = &cloned
	return nil
}

func (s *testModelStore) Delete(_ context.Context, keyParts ...any) error {
	providerID, _ := keyParts[0].(string)
	upstreamModel, _ := keyParts[1].(string)
	delete(s.items, providerID+"\x00"+upstreamModel)
	return nil
}

func (s *testModelStore) GetByIndex(context.Context, string, any) (any, error) {
	return nil, configstore.ErrNotFound
}

type testRouteStore struct {
	items map[string]*routecore.AgentRouteConfig
	tags  map[string]string
}

func testAdminRoute(cfg llmroutepkg.AgentRouteConfig, protocol llmroutepkg.RouteProtocol, targetPolicy llmroutepkg.RouteTargetPolicy) llmroutepkg.LLMRoute {
	cfg.Protocol = protocol
	return llmroutepkg.LLMRoute{
		AgentRouteConfig: cfg,
		TargetPolicy:     targetPolicy,
	}
}

type testMCPRouteStore struct {
	items map[string]*routecore.AgentRouteConfig
	tags  map[string]string
}

type testMCPServiceStore struct {
	items map[string]*mcpservice.MCPServiceConfig
	tags  map[string]string
}

func (s *testRouteStore) ListByTag(_ context.Context, tag string) ([]any, error) {
	out := make([]any, 0, len(s.items))
	for id, item := range s.items {
		if tag != "" && s.tags[id] != tag {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *testRouteStore) List(ctx context.Context) ([]any, error) {
	return s.ListByTag(ctx, "")
}

func (s *testRouteStore) ListByTagPrefix(_ context.Context, tagPrefix string) ([]any, error) {
	out := make([]any, 0, len(s.items))
	for id, item := range s.items {
		if tagPrefix != "" && !strings.HasPrefix(s.tags[id], tagPrefix) {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *testRouteStore) Create(_ context.Context, obj any) error {
	tag := ""
	if carrier, ok := obj.(interface{ ConfigStoreTag() string }); ok {
		tag = carrier.ConfigStoreTag()
	}
	if unwrapper, ok := obj.(interface{ ConfigStoreObject() any }); ok {
		obj = unwrapper.ConfigStoreObject()
	}
	cfg, ok := obj.(*routecore.AgentRouteConfig)
	if !ok || cfg == nil {
		return errors.New("unexpected type")
	}
	if s.items == nil {
		s.items = map[string]*routecore.AgentRouteConfig{}
	}
	if s.tags == nil {
		s.tags = map[string]string{}
	}
	cloned := *cfg
	s.items[cloned.ID] = &cloned
	s.tags[cloned.ID] = tag
	return nil
}

func (s *testRouteStore) Update(ctx context.Context, obj any) error {
	if unwrapper, ok := obj.(interface{ ConfigStoreObject() any }); ok {
		obj = unwrapper.ConfigStoreObject()
	}
	cfg, ok := obj.(*routecore.AgentRouteConfig)
	if !ok || cfg == nil {
		return errors.New("unexpected type")
	}
	if _, ok := s.items[cfg.ID]; !ok {
		return configstore.ErrNotFound
	}
	return s.Create(ctx, obj)
}

func (s *testRouteStore) Delete(_ context.Context, keyParts ...any) error {
	id, _ := keyParts[0].(string)
	delete(s.items, id)
	delete(s.tags, id)
	return nil
}

func (s *testRouteStore) Get(_ context.Context, keyParts ...any) (any, error) {
	id, _ := keyParts[0].(string)
	item, ok := s.items[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	cloned := *item
	return &cloned, nil
}

func mustRouteConfig(t *testing.T, route llmroutepkg.LLMRoute) *routecore.AgentRouteConfig {
	t.Helper()
	cfg, err := route.ToConfig()
	if err != nil {
		t.Fatalf("ToConfig returned error: %v", err)
	}
	return &cfg
}

func mustMCPRouteConfig(t *testing.T, route mcproute.MCPRouteConfig) *routecore.AgentRouteConfig {
	t.Helper()
	cfg, err := route.ToConfig()
	if err != nil {
		t.Fatalf("ToConfig returned error: %v", err)
	}
	return &cfg
}

func (s *testRouteStore) GetByIndex(context.Context, string, any) (any, error) {
	return nil, configstore.ErrNotFound
}

func (s *testMCPRouteStore) ListByTag(_ context.Context, tag string) ([]any, error) {
	out := make([]any, 0, len(s.items))
	for id, item := range s.items {
		if tag != "" && s.tags[id] != tag {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *testMCPRouteStore) List(ctx context.Context) ([]any, error) {
	return s.ListByTag(ctx, "")
}

func (s *testMCPRouteStore) ListByTagPrefix(_ context.Context, tagPrefix string) ([]any, error) {
	out := make([]any, 0, len(s.items))
	for id, item := range s.items {
		if tagPrefix != "" && !strings.HasPrefix(s.tags[id], tagPrefix) {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *testMCPRouteStore) Create(_ context.Context, obj any) error {
	tag := ""
	if carrier, ok := obj.(interface{ ConfigStoreTag() string }); ok {
		tag = carrier.ConfigStoreTag()
	}
	if unwrapper, ok := obj.(interface{ ConfigStoreObject() any }); ok {
		obj = unwrapper.ConfigStoreObject()
	}
	cfg, ok := obj.(*routecore.AgentRouteConfig)
	if !ok || cfg == nil {
		return errors.New("unexpected type")
	}
	if s.items == nil {
		s.items = map[string]*routecore.AgentRouteConfig{}
	}
	if s.tags == nil {
		s.tags = map[string]string{}
	}
	cloned := *cfg
	s.items[cloned.ID] = &cloned
	s.tags[cloned.ID] = tag
	return nil
}

func (s *testMCPRouteStore) Update(ctx context.Context, obj any) error {
	if unwrapper, ok := obj.(interface{ ConfigStoreObject() any }); ok {
		obj = unwrapper.ConfigStoreObject()
	}
	cfg, ok := obj.(*routecore.AgentRouteConfig)
	if !ok || cfg == nil {
		return errors.New("unexpected type")
	}
	if _, ok := s.items[cfg.ID]; !ok {
		return configstore.ErrNotFound
	}
	return s.Create(ctx, obj)
}

func (s *testMCPRouteStore) Delete(_ context.Context, keyParts ...any) error {
	id, _ := keyParts[0].(string)
	delete(s.items, id)
	delete(s.tags, id)
	return nil
}

func (s *testMCPRouteStore) Get(_ context.Context, keyParts ...any) (any, error) {
	id, _ := keyParts[0].(string)
	item, ok := s.items[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	cloned := *item
	return &cloned, nil
}

func (s *testMCPRouteStore) GetByIndex(context.Context, string, any) (any, error) {
	return nil, configstore.ErrNotFound
}

func (s *testMCPServiceStore) ListByTag(_ context.Context, tag string) ([]any, error) {
	out := make([]any, 0, len(s.items))
	for id, item := range s.items {
		if tag != "" && s.tags[id] != tag {
			continue
		}
		cloned := *item
		out = append(out, &cloned)
	}
	return out, nil
}

func (s *testMCPServiceStore) List(ctx context.Context) ([]any, error) {
	return s.ListByTag(ctx, "")
}

func (s *testMCPServiceStore) ListByTagPrefix(_ context.Context, tagPrefix string) ([]any, error) {
	out := make([]any, 0, len(s.items))
	for id, item := range s.items {
		if tagPrefix != "" && !strings.HasPrefix(s.tags[id], tagPrefix) {
			continue
		}
		cloned := *item
		out = append(out, &cloned)
	}
	return out, nil
}

func (s *testMCPServiceStore) Create(_ context.Context, obj any) error {
	tag := ""
	if carrier, ok := obj.(interface{ ConfigStoreTag() string }); ok {
		tag = carrier.ConfigStoreTag()
	}
	if unwrapper, ok := obj.(interface{ ConfigStoreObject() any }); ok {
		obj = unwrapper.ConfigStoreObject()
	}
	cfg, ok := obj.(*mcpservice.MCPServiceConfig)
	if !ok || cfg == nil {
		return errors.New("unexpected type")
	}
	if s.items == nil {
		s.items = map[string]*mcpservice.MCPServiceConfig{}
	}
	if s.tags == nil {
		s.tags = map[string]string{}
	}
	cloned := *cfg
	s.items[cloned.ID] = &cloned
	s.tags[cloned.ID] = tag
	return nil
}

func (s *testMCPServiceStore) Update(ctx context.Context, obj any) error {
	if unwrapper, ok := obj.(interface{ ConfigStoreObject() any }); ok {
		obj = unwrapper.ConfigStoreObject()
	}
	cfg, ok := obj.(*mcpservice.MCPServiceConfig)
	if !ok || cfg == nil {
		return errors.New("unexpected type")
	}
	if _, ok := s.items[cfg.ID]; !ok {
		return configstore.ErrNotFound
	}
	return s.Create(ctx, obj)
}

func (s *testMCPServiceStore) Delete(_ context.Context, keyParts ...any) error {
	id, _ := keyParts[0].(string)
	if _, ok := s.items[id]; !ok {
		return configstore.ErrNotFound
	}
	delete(s.items, id)
	delete(s.tags, id)
	return nil
}

func (s *testMCPServiceStore) Get(_ context.Context, keyParts ...any) (any, error) {
	id, _ := keyParts[0].(string)
	item, ok := s.items[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	cloned := *item
	return &cloned, nil
}

func (s *testMCPServiceStore) GetByIndex(context.Context, string, any) (any, error) {
	return nil, configstore.ErrNotFound
}

type testVirtualKeyStore struct {
	items map[string]*virtualkeypkg.VirtualKey
}

type testProviderConfigStore struct {
	items map[string]*provider.ProviderConfig
}

func (s *testProviderConfigStore) List(ctx context.Context) ([]any, error) {
	return s.ListByTag(ctx, "")
}

func (s *testProviderConfigStore) ListByTag(_ context.Context, name string) ([]any, error) {
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

func (s *testProviderConfigStore) ListByTagPrefix(ctx context.Context, tagPrefix string) ([]any, error) {
	return s.ListByTag(ctx, tagPrefix)
}

func (s *testProviderConfigStore) Create(_ context.Context, obj any) error {
	cfg, ok := obj.(*provider.ProviderConfig)
	if !ok {
		return errors.New("unexpected type")
	}
	if s.items == nil {
		s.items = map[string]*provider.ProviderConfig{}
	}
	cloned := *cfg
	s.items[cloned.Id] = &cloned
	return nil
}

func (s *testProviderConfigStore) Update(ctx context.Context, obj any) error {
	cfg, ok := obj.(*provider.ProviderConfig)
	if !ok {
		return errors.New("unexpected type")
	}
	if _, ok := s.items[cfg.Id]; !ok {
		return configstore.ErrNotFound
	}
	return s.Create(ctx, obj)
}

func (s *testProviderConfigStore) Delete(_ context.Context, keyParts ...any) error {
	id, _ := keyParts[0].(string)
	if _, ok := s.items[id]; !ok {
		return configstore.ErrNotFound
	}
	delete(s.items, id)
	return nil
}

func (s *testProviderConfigStore) Get(_ context.Context, keyParts ...any) (any, error) {
	id, _ := keyParts[0].(string)
	item, ok := s.items[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	cloned := *item
	return &cloned, nil
}

func (s *testProviderConfigStore) GetByIndex(context.Context, string, any) (any, error) {
	return nil, configstore.ErrNotFound
}

type stubAdminProvider struct {
	cfg    provider.ProviderConfig
	models []provider.ModelInfo
}

func (p *stubAdminProvider) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, nil
}

func (p *stubAdminProvider) StreamChat(context.Context, *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}

func (p *stubAdminProvider) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return append([]provider.ModelInfo(nil), p.models...), nil
}

func (p *stubAdminProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{}
}

func (p *stubAdminProvider) Config() provider.ProviderConfig {
	return p.cfg
}

func (s *testVirtualKeyStore) ListByTag(_ context.Context, tag string) ([]any, error) {
	out := make([]any, 0, len(s.items))
	for _, item := range s.items {
		if tag != "" && item.Tag != tag {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *testVirtualKeyStore) List(ctx context.Context) ([]any, error) {
	return s.ListByTag(ctx, "")
}

func (s *testVirtualKeyStore) ListByTagPrefix(ctx context.Context, tagPrefix string) ([]any, error) {
	return s.ListByTag(ctx, tagPrefix)
}

func (s *testVirtualKeyStore) Create(_ context.Context, obj any) error {
	if unwrapper, ok := obj.(interface{ ConfigStoreObject() any }); ok {
		obj = unwrapper.ConfigStoreObject()
	}
	item, ok := obj.(*virtualkeypkg.VirtualKey)
	if !ok {
		return errors.New("unexpected type")
	}
	if s.items == nil {
		s.items = map[string]*virtualkeypkg.VirtualKey{}
	}
	cloned := *item
	s.items[cloned.ID] = &cloned
	return nil
}

func (s *testVirtualKeyStore) Update(ctx context.Context, obj any) error {
	item, ok := obj.(*virtualkeypkg.VirtualKey)
	if !ok {
		return errors.New("unexpected type")
	}
	if _, ok := s.items[item.ID]; !ok {
		return configstore.ErrNotFound
	}
	return s.Create(ctx, obj)
}

func (s *testVirtualKeyStore) Delete(_ context.Context, keyParts ...any) error {
	id, _ := keyParts[0].(string)
	delete(s.items, id)
	return nil
}

func (s *testVirtualKeyStore) Get(_ context.Context, keyParts ...any) (any, error) {
	id, _ := keyParts[0].(string)
	item, ok := s.items[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return item, nil
}

func (s *testVirtualKeyStore) GetByIndex(_ context.Context, indexName string, value any) (any, error) {
	key, _ := value.(string)
	for _, item := range s.items {
		if item.Key == key {
			return item, nil
		}
	}
	return nil, configstore.ErrNotFound
}

func TestLLMRouteCRUD(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		routeStore: &testRouteStore{items: map[string]*routecore.AgentRouteConfig{}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	createBody, err := json.Marshal(llmroutepkg.LLMRoute{
		AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "chat-prod", Protocol: llmroutepkg.RouteProtocolOpenAI},
		TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
			ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai"},
		},
	})
	if err != nil {
		t.Fatalf("marshal route: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/admin/llm/routes", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: got %d want %d", createRec.Code, http.StatusCreated)
	}

	var created LLMRouteView
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode created route: %v", err)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("created route CreatedAt is zero")
	}
	if created.UpdatedAt.IsZero() {
		t.Fatal("created route UpdatedAt is zero")
	}
	if created.Source != "store" || created.ReadOnly {
		t.Fatalf("unexpected created route metadata: %#v", created)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/admin/llm/routes/chat-prod", nil)
	getReq.Header.Set("Authorization", "Bearer "+token)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("unexpected get status: got %d want %d", getRec.Code, http.StatusOK)
	}

	var got LLMRouteView
	if err := json.NewDecoder(getRec.Body).Decode(&got); err != nil {
		t.Fatalf("decode route: %v", err)
	}
	if got.ID != "chat-prod" {
		t.Fatalf("unexpected route id: got %q want %q", got.ID, "chat-prod")
	}
	gotCfg, err := got.LLMRouteConfig()
	if err != nil {
		t.Fatalf("decode route config: %v", err)
	}
	directPolicy, ok := llmroutepkg.DirectProviderPolicyOf(gotCfg.TargetPolicy)
	if !ok || directPolicy.ProviderTarget.ProviderID != "openai" {
		t.Fatalf("unexpected target_policy: %#v", gotCfg.TargetPolicy)
	}
	if got.Source != "store" || got.ReadOnly {
		t.Fatalf("unexpected route metadata: %#v", got)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("got CreatedAt = %v, want %v", got.CreatedAt, created.CreatedAt)
	}
}

func TestAgentRouteCRUD(t *testing.T) {
	backend, err := configstore.OpenBackend(t.Context(), "sqlite", configstoresqlite.Config{SQLitePath: t.TempDir() + "/config.db"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := configstoreschema.RegisterDefaultStores(backend); err != nil {
		t.Fatal(err)
	}
	gw := newTestAgentGateway(backend, nil, nil, nil, nil)
	if err := gw.AgentManager().Create(t.Context(), agentpkg.Agent{
		ID: "assistant", Name: "Assistant",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeHTTP, HTTP: &agentpkg.HTTPRuntime{Endpoint: "https://example.com/agent"}},
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(gw, nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	createBody, err := json.Marshal(agentroute.AgentRouteConfig{
		AgentRouteBaseConfig: agentroute.AgentRouteBaseConfig{
			ID:          "assistant",
			MatchPolicy: agentroute.RouteMatch{PathPrefix: "/agents/assistant"},
			AuthPolicy:  agentroute.RouteAuthPolicy{RequireVirtualKey: true},
		},
		AgentID: "assistant",
	})
	if err != nil {
		t.Fatal(err)
	}
	createReq := httptest.NewRequest(http.MethodPost, "/admin/agents/routes", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create AgentRoute: status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created AgentRouteView
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Kind != agentroute.RouteKindAgent || created.Protocol != agentroute.RouteProtocolAgent || created.AgentID != "assistant" {
		t.Fatalf("created AgentRoute = %#v", created)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/admin/agents/routes/assistant", nil)
	getReq.Header.Set("Authorization", "Bearer "+token)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get AgentRoute: status=%d body=%s", getRec.Code, getRec.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/admin/agents/routes/assistant", nil)
	deleteReq.Header.Set("Authorization", "Bearer "+token)
	deleteRec := httptest.NewRecorder()
	handler.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete AgentRoute: status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
}

func TestAgentIDWithRoutesPrefixIsNotDispatchedAsAgentRoute(t *testing.T) {
	backend, err := configstore.OpenBackend(t.Context(), "sqlite", configstoresqlite.Config{SQLitePath: t.TempDir() + "/config.db"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := configstoreschema.RegisterDefaultStores(backend); err != nil {
		t.Fatal(err)
	}
	gw := newTestAgentGateway(backend, nil, nil, nil, nil)
	if err := gw.AgentManager().Create(t.Context(), agentpkg.Agent{
		ID: "routesXYZ", Name: "Routes Prefix",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeHTTP, HTTP: &agentpkg.HTTPRuntime{Endpoint: "https://example.com/agent"}},
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(gw, nil)
	token := loginForTest(t, handler, "admin", "secret-pass")
	for _, path := range []string{"/admin/agents/routesXYZ", "/admin/agents/routesXYZ/workspace"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestLegacyRuntimeAdminSurfacesAreNotRegistered(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		routeStore: &testRouteStore{items: map[string]*routecore.AgentRouteConfig{}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")
	for _, path := range []string{"/admin/acp/services", "/admin/acp/routes", "/admin/builtin/routes"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s status=%d body=%s, want 404", path, rec.Code, rec.Body.String())
		}
	}
}

func TestLLMRouteUpdatePreservesCreatedAt(t *testing.T) {
	createdAt := time.Now().UTC().Add(-time.Hour).Round(0)
	updatedAt := createdAt
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		routeStore: &testRouteStore{items: map[string]*routecore.AgentRouteConfig{
			"chat-prod": mustRouteConfig(t, llmroutepkg.LLMRoute{
				AgentRouteConfig: llmroutepkg.AgentRouteConfig{
					ID:        "chat-prod",
					Protocol:  llmroutepkg.RouteProtocolOpenAI,
					CreatedAt: createdAt,
					UpdatedAt: updatedAt,
				},
				TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
					ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai"},
				},
			}),
		}, tags: map[string]string{"chat-prod": defaultLLMRouteTag}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	updateBody, err := json.Marshal(llmroutepkg.LLMRoute{
		AgentRouteConfig: llmroutepkg.AgentRouteConfig{Protocol: llmroutepkg.RouteProtocolOpenAI},
		TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
			ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "anthropic"},
		},
	})
	if err != nil {
		t.Fatalf("marshal route update: %v", err)
	}

	updateReq := httptest.NewRequest(http.MethodPut, "/admin/llm/routes/chat-prod", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", "Bearer "+token)
	updateRec := httptest.NewRecorder()
	handler.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("unexpected update status: got %d want %d", updateRec.Code, http.StatusOK)
	}

	var updated LLMRouteView
	if err := json.NewDecoder(updateRec.Body).Decode(&updated); err != nil {
		t.Fatalf("decode updated route: %v", err)
	}
	if !updated.CreatedAt.Equal(createdAt) {
		t.Fatalf("updated CreatedAt = %v, want %v", updated.CreatedAt, createdAt)
	}
	if !updated.UpdatedAt.After(updatedAt) {
		t.Fatalf("updated UpdatedAt = %v, want after %v", updated.UpdatedAt, updatedAt)
	}
}

func TestLLMRouteCreateRejectsClientManagedTimestamps(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		routeStore: &testRouteStore{items: map[string]*routecore.AgentRouteConfig{}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	body, err := json.Marshal(llmroutepkg.LLMRoute{
		AgentRouteConfig: llmroutepkg.AgentRouteConfig{
			ID:        "chat-prod",
			Protocol:  llmroutepkg.RouteProtocolOpenAI,
			CreatedAt: time.Now().UTC(),
		},
		TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
			ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai"},
		},
	})
	if err != nil {
		t.Fatalf("marshal route: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/llm/routes", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected create status: got %d want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestMCPRouteAutoIDIsSlashFreeAndAddressable guards the route-id contract: an
// auto-generated id (derived from the path prefix) must be slash-free so it is
// addressable as a single Admin API path segment. A slash-bearing id would 404
// on GET/DELETE because the Go ServeMux "{id}" wildcard only matches one
// segment.
func TestMCPRouteAutoIDIsSlashFreeAndAddressable(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		routeStore: &testMCPRouteStore{items: map[string]*routecore.AgentRouteConfig{}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	createBody, err := json.Marshal(mcproute.MCPRouteConfig{
		AgentRouteConfig: mcproute.AgentRouteConfig{
			MatchPolicy: mcproute.RouteMatch{PathPrefix: "/mcp/foo"},
		},
		ServiceID: "svc-main",
	})
	if err != nil {
		t.Fatalf("marshal mcp route: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/admin/mcp/routes", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: got %d want %d", createRec.Code, http.StatusCreated)
	}

	var created MCPRouteView
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode created mcp route: %v", err)
	}
	if strings.ContainsAny(created.ID, "/\\") {
		t.Fatalf("auto-generated route id must be slash-free: %q", created.ID)
	}
	if created.ID != "mcp:svc-main:mcp-foo" {
		t.Fatalf("unexpected auto-generated route id: %q", created.ID)
	}

	// GET and DELETE with the generated id must resolve through the "{id}"
	// pattern, not 404.
	getReq := httptest.NewRequest(http.MethodGet, "/admin/mcp/routes/"+created.ID, nil)
	getReq.Header.Set("Authorization", "Bearer "+token)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("unexpected get status: got %d want %d", getRec.Code, http.StatusOK)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/admin/mcp/routes/"+created.ID, nil)
	delReq.Header.Set("Authorization", "Bearer "+token)
	delRec := httptest.NewRecorder()
	handler.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("unexpected delete status: got %d want %d", delRec.Code, http.StatusOK)
	}
}

// TestMCPRouteCreateRejectsSlashID ensures an explicitly supplied slash-bearing
// route id is rejected rather than silently creating an unaddressable route.
func TestMCPRouteCreateRejectsSlashID(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		routeStore: &testMCPRouteStore{items: map[string]*routecore.AgentRouteConfig{}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	createBody, err := json.Marshal(mcproute.MCPRouteConfig{
		AgentRouteConfig: mcproute.AgentRouteConfig{
			ID:          "mcp/with/slash",
			MatchPolicy: mcproute.RouteMatch{PathPrefix: "/mcp"},
		},
		ServiceID: "svc-main",
	})
	if err != nil {
		t.Fatalf("marshal mcp route: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/admin/mcp/routes", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusBadRequest {
		t.Fatalf("expected slash id to be rejected with 400, got status %d", createRec.Code)
	}
}

func TestMCPRouteCRUD(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		routeStore: &testMCPRouteStore{items: map[string]*routecore.AgentRouteConfig{}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	createBody, err := json.Marshal(mcproute.MCPRouteConfig{
		AgentRouteConfig: mcproute.AgentRouteConfig{
			ID:          "mcp-route-1",
			MatchPolicy: mcproute.RouteMatch{PathPrefix: "/mcp"},
		},
		ServiceID: "svc-main",
	})
	if err != nil {
		t.Fatalf("marshal mcp route: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/admin/mcp/routes", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: got %d want %d", createRec.Code, http.StatusCreated)
	}

	var created MCPRouteView
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode created mcp route: %v", err)
	}
	if created.ID != "mcp-route-1" || created.ServiceID != "svc-main" {
		t.Fatalf("unexpected created route: %#v", created)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/admin/mcp/routes/mcp-route-1", nil)
	getReq.Header.Set("Authorization", "Bearer "+token)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("unexpected get status: got %d want %d", getRec.Code, http.StatusOK)
	}
}

func TestListMCPRoutes(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		routeStore: &testMCPRouteStore{items: map[string]*routecore.AgentRouteConfig{
			"mcp-route-1": mustMCPRouteConfig(t, mcproute.MCPRouteConfig{
				AgentRouteConfig: mcproute.AgentRouteConfig{
					ID:          "mcp-route-1",
					MatchPolicy: mcproute.RouteMatch{PathPrefix: "/mcp"},
				},
				ServiceID: "svc-main",
			}),
		}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodGet, "/admin/mcp/routes", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []MCPRouteView `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != "mcp-route-1" {
		t.Fatalf("unexpected items: %#v", resp.Items)
	}
}

func TestListRoutesExcludesMCPRoutesFromSharedStore(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		routeStore: &testRouteStore{items: map[string]*routecore.AgentRouteConfig{
			"chat-prod": mustRouteConfig(t, llmroutepkg.LLMRoute{
				AgentRouteConfig: llmroutepkg.AgentRouteConfig{
					ID:          "chat-prod",
					Protocol:    llmroutepkg.RouteProtocolOpenAI,
					MatchPolicy: llmroutepkg.RouteMatchPolicy{PathPrefix: "/v1"},
				},
				TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
					ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai-main"},
				},
			}),
			"mcp-route-1": mustMCPRouteConfig(t, mcproute.MCPRouteConfig{
				AgentRouteConfig: mcproute.AgentRouteConfig{
					ID:          "mcp-route-1",
					MatchPolicy: mcproute.RouteMatch{PathPrefix: "/mcp"},
				},
				ServiceID: "svc-main",
			}),
		}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/routes", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []LLMRouteView `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != "chat-prod" {
		t.Fatalf("unexpected items: %#v", resp.Items)
	}
}

func TestListMCPRoutesExcludesLLMRoutesFromSharedStore(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		routeStore: &testRouteStore{items: map[string]*routecore.AgentRouteConfig{
			"chat-prod": mustRouteConfig(t, llmroutepkg.LLMRoute{
				AgentRouteConfig: llmroutepkg.AgentRouteConfig{
					ID:          "chat-prod",
					Protocol:    llmroutepkg.RouteProtocolOpenAI,
					MatchPolicy: llmroutepkg.RouteMatchPolicy{PathPrefix: "/v1"},
				},
				TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
					ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai-main"},
				},
			}),
			"mcp-route-1": mustMCPRouteConfig(t, mcproute.MCPRouteConfig{
				AgentRouteConfig: mcproute.AgentRouteConfig{
					ID:          "mcp-route-1",
					MatchPolicy: mcproute.RouteMatch{PathPrefix: "/mcp"},
				},
				ServiceID: "svc-main",
			}),
		}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodGet, "/admin/mcp/routes", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Items []MCPRouteView `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != "mcp-route-1" {
		t.Fatalf("unexpected items: %#v", resp.Items)
	}
}

func TestGetMCPDispatcherRuntime(t *testing.T) {
	agentGateway := newTestAgentGateway(&testConfigStore{}, nil, nil, nil, nil)
	registry := agentGateway.MCPRuntimeRegistry()
	routeID := "mcp:test"
	_, finish := registry.BeginRequest(context.Background(), routeID, "req-1", "tools/call", "progress-1")
	defer finish(nil)
	total := float64(10)
	registry.StoreProgress(routeID, "progress-1", 3, &total, "working")

	handler := NewHandler(agentGateway, nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodGet, "/admin/mcp/runtime", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusOK)
	}

	var resp MCPDispatcherRuntimeView
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode runtime response: %v", err)
	}
	if len(resp.InFlight) != 1 || resp.InFlight[0].RequestKey != mcpruntime.RouteRequestKey(routeID, "req-1") {
		t.Fatalf("unexpected in-flight payload: %#v", resp.InFlight)
	}
	if len(resp.Progress) != 1 || resp.Progress[0].Progress != 3 {
		t.Fatalf("unexpected progress payload: %#v", resp.Progress)
	}
}

func TestListMCPDispatcherHistory(t *testing.T) {
	agentGateway := newTestAgentGateway(&testConfigStore{}, nil, nil, nil, nil)
	registry := agentGateway.MCPRuntimeRegistry()

	// add two completed requests on different routes
	_, finishA := registry.BeginRequest(context.Background(), "route-a", "req-1", "tools/call", nil)
	finishA(nil)
	_, finishB := registry.BeginRequest(context.Background(), "route-b", "req-2", "ping", nil)
	finishB(nil)

	handler := NewHandler(agentGateway, nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	// no filter: both entries
	req := httptest.NewRequest(http.MethodGet, "/admin/mcp/runtime/history", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusOK)
	}
	var resp struct {
		Items []mcpruntime.CompletedRequest `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode history response: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 history items, got %d", len(resp.Items))
	}

	// route_id filter: only route-a
	req2 := httptest.NewRequest(http.MethodGet, "/admin/mcp/runtime/history?route_id=route-a", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", rec2.Code, http.StatusOK)
	}
	var resp2 struct {
		Items []mcpruntime.CompletedRequest `json:"items"`
	}
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decode filtered history response: %v", err)
	}
	if len(resp2.Items) != 1 || resp2.Items[0].RouteID != "route-a" {
		t.Fatalf("expected 1 item for route-a, got %#v", resp2.Items)
	}
}

func TestMCPServiceEndpointsRequireSharedManager(t *testing.T) {
	handler := NewHandler(nil, nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodGet, "/admin/mcp/services", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), "mcp service manager is not configured") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/mcp/services/svc-1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status: got %d want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), "mcp service manager is not configured") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestVirtualKeyCRUD(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		virtualKeyStore: &testVirtualKeyStore{items: map[string]*virtualkeypkg.VirtualKey{}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	body, err := json.Marshal(virtualkeypkg.VirtualKey{
		ID:              "vk-test",
		Tag:             "admin",
		AllowedRouteIDs: []string{"chat-prod"},
		RateLimits: &virtualkeypkg.VirtualKeyRateLimits{
			LLM: &virtualkeypkg.RateLimit{RequestsPerMinute: 60, Burst: 10},
		},
	})
	if err != nil {
		t.Fatalf("marshal virtual key: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/admin/virtual_keys", bytes.NewReader(body))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: got %d want %d", createRec.Code, http.StatusCreated)
	}

	var created virtualkeypkg.VirtualKey
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode created virtual key: %v", err)
	}
	if created.Key == "" {
		t.Fatal("created virtual key is empty")
	}
	if !strings.HasPrefix(created.Key, virtualkeypkg.GeneratedKeyPrefix) {
		t.Fatalf("created virtual key = %q, want prefix %q", created.Key, virtualkeypkg.GeneratedKeyPrefix)
	}
	if created.ID != "vk-test" {
		t.Fatalf("created virtual key id = %q, want vk-test", created.ID)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("created virtual key CreatedAt is zero")
	}
	if created.UpdatedAt.IsZero() {
		t.Fatal("created virtual key UpdatedAt is zero")
	}

	getReq := httptest.NewRequest(http.MethodGet, "/admin/virtual_keys/"+created.ID, nil)
	getReq.Header.Set("Authorization", "Bearer "+token)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("unexpected get status: got %d want %d", getRec.Code, http.StatusOK)
	}

	var got VirtualKeyView
	if err := json.NewDecoder(getRec.Body).Decode(&got); err != nil {
		t.Fatalf("decode virtual key: %v", err)
	}
	if got.Key != created.Key {
		t.Fatalf("unexpected virtual key: got %q want %q", got.Key, created.Key)
	}
	if got.ID != created.ID {
		t.Fatalf("unexpected virtual key id: got %q want %q", got.ID, created.ID)
	}
	if len(got.AllowedRouteIDs) != 1 || got.AllowedRouteIDs[0] != "chat-prod" {
		t.Fatalf("unexpected allowed routes: %#v", got.AllowedRouteIDs)
	}
	if got.RateLimits == nil || got.RateLimits.LLM == nil ||
		got.RateLimits.LLM.RequestsPerMinute != 60 || got.RateLimits.LLM.Burst != 10 {
		t.Fatalf("unexpected rate limits: %#v", got.RateLimits)
	}
	if got.Source != "store" || got.ReadOnly {
		t.Fatalf("unexpected virtual key metadata: %#v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("got virtual key CreatedAt is zero")
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("got virtual key UpdatedAt is zero")
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("got CreatedAt = %v, want %v", got.CreatedAt, created.CreatedAt)
	}
}

func TestVirtualKeyCreateRejectsClientManagedTimestamps(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		virtualKeyStore: &testVirtualKeyStore{items: map[string]*virtualkeypkg.VirtualKey{}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	body, err := json.Marshal(virtualkeypkg.VirtualKey{
		ID:        "vk-test",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal virtual key: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/virtual_keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected create status: got %d want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestVirtualKeyCreateRejectsInvalidOrUnknownRateLimits(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		virtualKeyStore: &testVirtualKeyStore{items: map[string]*virtualkeypkg.VirtualKey{}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	for _, body := range []string{
		`{"id":"vk-invalid","rate_limits":{"llm":{"requests_per_minute":0,"burst":1}}}`,
		`{"id":"vk-unknown","rate_limits":{"lmm":{"requests_per_minute":1,"burst":1}}}`,
		`{"id":"vk-setting","rate_limits":{"llm":{"requests_per_minute":1,"brust":1}}}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/admin/virtual_keys", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want %d; response=%s", body, rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	}

	store := &testVirtualKeyStore{items: map[string]*virtualkeypkg.VirtualKey{
		"vk-existing": {ID: "vk-existing", Key: "secret"},
	}}
	updateHandler := NewHandler(newTestAgentGateway(&testConfigStore{
		virtualKeyStore: store,
	}, nil, nil, nil, nil), nil)
	updateToken := loginForTest(t, updateHandler, "admin", "secret-pass")
	req := httptest.NewRequest(http.MethodPut, "/admin/virtual_keys/vk-existing",
		strings.NewReader(`{"id":"vk-existing","rate_limits":{"agent":{"requests_per_minute":1,"burst":0}}}`))
	req.Header.Set("Authorization", "Bearer "+updateToken)
	rec := httptest.NewRecorder()
	updateHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update status = %d, want %d; response=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestProviderCRUD(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		providerStore: &testProviderConfigStore{items: map[string]*provider.ProviderConfig{}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	body, err := json.Marshal(provider.ProviderConfig{
		Id:           "openai-main",
		ProviderType: "openai",
		BaseURL:      "https://api.openai.com/v1",
		DefaultModel: "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("marshal provider config: %v", err)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/admin/llm/providers", bytes.NewReader(body))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: got %d want %d", createRec.Code, http.StatusCreated)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/admin/llm/providers/openai-main", nil)
	getReq.Header.Set("Authorization", "Bearer "+token)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("unexpected get status: got %d want %d", getRec.Code, http.StatusOK)
	}

	var got ProviderView
	if err := json.NewDecoder(getRec.Body).Decode(&got); err != nil {
		t.Fatalf("decode provider: %v", err)
	}
	if got.Id != "openai-main" {
		t.Fatalf("unexpected provider id: got %q want %q", got.Id, "openai-main")
	}
	if got.ProviderType != "openai" {
		t.Fatalf("unexpected provider_type: got %q want %q", got.ProviderType, "openai")
	}
	if got.Source != "store" || got.ReadOnly {
		t.Fatalf("unexpected provider metadata: %#v", got)
	}
}

func TestProviderEnableDisable(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		providerStore: &testProviderConfigStore{items: map[string]*provider.ProviderConfig{
			"openai-main": {Id: "openai-main", ProviderType: "openai"},
		}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	disableReq := httptest.NewRequest(http.MethodPost, "/admin/llm/providers/openai-main/disable", nil)
	disableReq.Header.Set("Authorization", "Bearer "+token)
	disableRec := httptest.NewRecorder()
	handler.ServeHTTP(disableRec, disableReq)
	if disableRec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want %d", disableRec.Code, http.StatusOK)
	}

	var disabled ProviderView
	if err := json.NewDecoder(disableRec.Body).Decode(&disabled); err != nil {
		t.Fatalf("decode disabled provider: %v", err)
	}
	if !disabled.Disabled {
		t.Fatal("provider disabled = false, want true")
	}

	enableReq := httptest.NewRequest(http.MethodPost, "/admin/llm/providers/openai-main/enable", nil)
	enableReq.Header.Set("Authorization", "Bearer "+token)
	enableRec := httptest.NewRecorder()
	handler.ServeHTTP(enableRec, enableReq)
	if enableRec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want %d", enableRec.Code, http.StatusOK)
	}

	var enabled ProviderView
	if err := json.NewDecoder(enableRec.Body).Decode(&enabled); err != nil {
		t.Fatalf("decode enabled provider: %v", err)
	}
	if enabled.Disabled {
		t.Fatal("provider disabled = true, want false")
	}
}

func TestProviderTypeList(t *testing.T) {
	const providerType = "test-admin-provider-name"
	provider.RegisterProviderFactory(providerType, func(cfg provider.ProviderConfig) (provider.Provider, error) {
		return &stubAdminProvider{cfg: cfg}, nil
	})
	handler := NewHandler(nil, nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	listReq := httptest.NewRequest(http.MethodGet, "/admin/llm/provider_types", nil)
	listReq.Header.Set("Authorization", "Bearer "+token)
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d", listRec.Code, http.StatusOK)
	}

	var listed struct {
		Items []ProviderTypeView `json:"items"`
	}
	if err := json.NewDecoder(listRec.Body).Decode(&listed); err != nil {
		t.Fatalf("decode provider types: %v", err)
	}
	found := false
	for _, item := range listed.Items {
		if item.ProviderType != providerType {
			continue
		}
		found = true
		if !item.Enabled {
			t.Fatal("provider type enabled = false, want true")
		}
	}
	if !found {
		t.Fatalf("provider type %q not listed", providerType)
	}
}

func TestLLMApiHandlerTypeList(t *testing.T) {
	const handlerType = "test-admin-llm-api-handler"
	dispatcherpkg.RegisterLLMApiHandlerType(handlerType)
	handler := NewHandler(nil, nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	listReq := httptest.NewRequest(http.MethodGet, "/admin/llm/api_handler_types", nil)
	listReq.Header.Set("Authorization", "Bearer "+token)
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d", listRec.Code, http.StatusOK)
	}

	var listed struct {
		Items []LLMApiHandlerTypeView `json:"items"`
	}
	if err := json.NewDecoder(listRec.Body).Decode(&listed); err != nil {
		t.Fatalf("decode llm api handler types: %v", err)
	}
	found := false
	for _, item := range listed.Items {
		if item.LLMApiHandlerType != handlerType {
			continue
		}
		found = true
	}
	if !found {
		t.Fatalf("llm api handler type %q not listed", handlerType)
	}
}

func TestLLMRouteEnableDisable(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		routeStore: &testRouteStore{items: map[string]*routecore.AgentRouteConfig{
			"chat-prod": mustRouteConfig(t, llmroutepkg.LLMRoute{
				AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "chat-prod"},
				TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
					ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai"},
				},
			}),
		}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	disableReq := httptest.NewRequest(http.MethodPost, "/admin/llm/routes/chat-prod/disable", nil)
	disableReq.Header.Set("Authorization", "Bearer "+token)
	disableRec := httptest.NewRecorder()
	handler.ServeHTTP(disableRec, disableReq)
	if disableRec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want %d", disableRec.Code, http.StatusOK)
	}

	var disabled LLMRouteView
	if err := json.NewDecoder(disableRec.Body).Decode(&disabled); err != nil {
		t.Fatalf("decode disabled route: %v", err)
	}
	if !disabled.Disabled {
		t.Fatal("route disabled = false, want true")
	}

	enableReq := httptest.NewRequest(http.MethodPost, "/admin/llm/routes/chat-prod/enable", nil)
	enableReq.Header.Set("Authorization", "Bearer "+token)
	enableRec := httptest.NewRecorder()
	handler.ServeHTTP(enableRec, enableReq)
	if enableRec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want %d", enableRec.Code, http.StatusOK)
	}

	var enabled LLMRouteView
	if err := json.NewDecoder(enableRec.Body).Decode(&enabled); err != nil {
		t.Fatalf("decode enabled route: %v", err)
	}
	if enabled.Disabled {
		t.Fatal("route disabled = true, want false")
	}
}

func TestVirtualKeyEnableDisable(t *testing.T) {
	createdAt := time.Now().UTC().Add(-time.Hour).Round(0)
	updatedAt := createdAt
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		virtualKeyStore: &testVirtualKeyStore{items: map[string]*virtualkeypkg.VirtualKey{
			"vk-test": {ID: "vk-test", Key: "lk-test", Tag: "admin", CreatedAt: createdAt, UpdatedAt: updatedAt},
		}},
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	disableReq := httptest.NewRequest(http.MethodPost, "/admin/virtual_keys/vk-test/disable", nil)
	disableReq.Header.Set("Authorization", "Bearer "+token)
	disableRec := httptest.NewRecorder()
	handler.ServeHTTP(disableRec, disableReq)
	if disableRec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want %d", disableRec.Code, http.StatusOK)
	}

	var disabled VirtualKeyView
	if err := json.NewDecoder(disableRec.Body).Decode(&disabled); err != nil {
		t.Fatalf("decode disabled virtual key: %v", err)
	}
	if !disabled.Disabled {
		t.Fatal("virtual key disabled = false, want true")
	}
	if !disabled.CreatedAt.Equal(createdAt) {
		t.Fatalf("disabled CreatedAt = %v, want %v", disabled.CreatedAt, createdAt)
	}
	if !disabled.UpdatedAt.After(updatedAt) {
		t.Fatalf("disabled UpdatedAt = %v, want after %v", disabled.UpdatedAt, updatedAt)
	}
	firstUpdatedAt := disabled.UpdatedAt

	enableReq := httptest.NewRequest(http.MethodPost, "/admin/virtual_keys/vk-test/enable", nil)
	enableReq.Header.Set("Authorization", "Bearer "+token)
	enableRec := httptest.NewRecorder()
	handler.ServeHTTP(enableRec, enableReq)
	if enableRec.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want %d", enableRec.Code, http.StatusOK)
	}

	var enabled VirtualKeyView
	if err := json.NewDecoder(enableRec.Body).Decode(&enabled); err != nil {
		t.Fatalf("decode enabled virtual key: %v", err)
	}
	if enabled.Disabled {
		t.Fatal("virtual key disabled = true, want false")
	}
	if !enabled.CreatedAt.Equal(createdAt) {
		t.Fatalf("enabled CreatedAt = %v, want %v", enabled.CreatedAt, createdAt)
	}
	if !enabled.UpdatedAt.After(firstUpdatedAt) {
		t.Fatalf("enabled UpdatedAt = %v, want after %v", enabled.UpdatedAt, firstUpdatedAt)
	}
}

func TestProviderGetMarksStaticProviderAsReadOnly(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		providerStore: &testProviderConfigStore{items: map[string]*provider.ProviderConfig{
			"openai-main": {Id: "openai-main", ProviderType: "openai", BaseURL: "https://dynamic.example"},
		}},
	}, nil, nil, nil, nil, map[string]provider.Provider{
		"openai-main": &stubAdminProvider{cfg: provider.ProviderConfig{Id: "openai-main", ProviderType: "openai", BaseURL: "https://static.example"}},
	}), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/providers/openai-main", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected get status: got %d want %d", rec.Code, http.StatusOK)
	}

	var got ProviderView
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode provider: %v", err)
	}
	if got.BaseURL != "https://static.example" {
		t.Fatalf("base_url = %q, want https://static.example", got.BaseURL)
	}
	if got.Source != "caddyfile" || !got.ReadOnly {
		t.Fatalf("unexpected provider metadata: %#v", got)
	}
}

func TestProviderListMarksStaticProvidersAsReadOnly(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		providerStore: &testProviderConfigStore{items: map[string]*provider.ProviderConfig{
			"openai-dynamic": {Id: "openai-dynamic", ProviderType: "openai"},
		}},
	}, nil, nil, nil, nil, map[string]provider.Provider{
		"anthropic-static": &stubAdminProvider{cfg: provider.ProviderConfig{Id: "anthropic-static", ProviderType: "anthropic"}},
	}), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/providers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected list status: got %d want %d", rec.Code, http.StatusOK)
	}

	var got struct {
		Items []ProviderView `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode providers: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("item count = %d, want 2", len(got.Items))
	}

	byID := map[string]ProviderView{}
	for _, item := range got.Items {
		byID[item.Id] = item
	}
	if byID["anthropic-static"].Source != "caddyfile" || !byID["anthropic-static"].ReadOnly {
		t.Fatalf("unexpected static provider metadata: %#v", byID["anthropic-static"])
	}
	if byID["openai-dynamic"].Source != "store" || byID["openai-dynamic"].ReadOnly {
		t.Fatalf("unexpected dynamic provider metadata: %#v", byID["openai-dynamic"])
	}
}

func TestCredentialListShowsOnlyManagedCredentials(t *testing.T) {
	credMgr := credentialmgr.NewManager(nil)
	agentGateway := gateway.NewAgentGateway()
	if err := agentGateway.Bootstrap(context.Background(), gateway.BootstrapOptions{
		ConfigStoreBackend: &testConfigStore{
			providerStore: &testProviderConfigStore{items: map[string]*provider.ProviderConfig{}},
		},
		CredentialManager: credMgr,
		StaticProviders: map[string]provider.Provider{
			"openai-static": &stubAdminProvider{cfg: provider.ProviderConfig{
				Id:           "openai-static",
				ProviderType: "openai",
				APIKey:       "static-key",
				BaseURL:      "https://static.example",
			}},
		},
	}); err != nil {
		t.Fatalf("bootstrap gateway: %v", err)
	}
	if err := credMgr.RegisterCredential(context.Background(), &credentialmgr.Credential{
		ID:           "cred-1",
		ProviderType: "openai",
		ProviderID:   "openai-static",
		Type:         credentialmgr.TypeAPIKey,
		Attributes:   map[string]string{"api_key": "managed-key"},
	}); err != nil {
		t.Fatalf("register managed credential: %v", err)
	}

	handler := NewHandler(agentGateway, nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodGet, "/admin/credentials", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected list status: got %d want %d", rec.Code, http.StatusOK)
	}

	var got struct {
		Items []CredentialView `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode credentials: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("item count = %d, want 1", len(got.Items))
	}
	if got.Items[0].ID != "cred-1" || got.Items[0].Attributes["api_key"] != "managed-key" || got.Items[0].ReadOnly {
		t.Fatalf("unexpected managed credential view: %#v", got.Items[0])
	}
}

func TestCredentialCreateUsesProviderID(t *testing.T) {
	credMgr := credentialmgr.NewManager(nil)
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		providerStore: &testProviderConfigStore{items: map[string]*provider.ProviderConfig{
			"openai-main": {Id: "openai-main", ProviderType: "openai"},
		}},
	}, nil, nil, nil, nil), nil)
	handler.credentialManager = credMgr
	token := loginForTest(t, handler, "admin", "secret-pass")

	body, err := json.Marshal(map[string]any{
		"type":        credentialmgr.TypeAPIKey,
		"provider_id": "openai-main",
		"label":       "primary",
		"attributes": map[string]string{
			"api_key": "sk-test",
		},
	})
	if err != nil {
		t.Fatalf("marshal create request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/credentials", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: got %d want %d", rec.Code, http.StatusCreated)
	}

	var got credentialmgr.Credential
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode created credential: %v", err)
	}
	if strings.TrimSpace(got.ID) == "" {
		t.Fatal("expected created credential response to include id")
	}
	if got.ProviderID != "openai-main" {
		t.Fatalf("unexpected provider_id: got %q want %q", got.ProviderID, "openai-main")
	}
	if got.ProviderType != "openai" {
		t.Fatalf("unexpected provider_type: got %q want %q", got.ProviderType, "openai")
	}
	if got.Label != "primary" {
		t.Fatalf("unexpected label: got %q want %q", got.Label, "primary")
	}
}

func TestCredentialCreateRejectsUnknownProviderID(t *testing.T) {
	credMgr := credentialmgr.NewManager(nil)
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		providerStore: &testProviderConfigStore{items: map[string]*provider.ProviderConfig{}},
	}, nil, nil, nil, nil), nil)
	handler.credentialManager = credMgr
	token := loginForTest(t, handler, "admin", "secret-pass")

	body, err := json.Marshal(map[string]any{
		"type":        credentialmgr.TypeAPIKey,
		"provider_id": "missing-provider",
		"attributes": map[string]string{
			"api_key": "sk-test",
		},
	})
	if err != nil {
		t.Fatalf("marshal create request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/credentials", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unexpected create status: got %d want %d", rec.Code, http.StatusNotFound)
	}
}

func TestProviderCreateDoesNotSyncProviderConfigCredential(t *testing.T) {
	credMgr := credentialmgr.NewManager(nil)
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		providerStore: &testProviderConfigStore{items: map[string]*provider.ProviderConfig{}},
	}, nil, nil, nil, nil), nil)
	handler.credentialManager = credMgr
	token := loginForTest(t, handler, "admin", "secret-pass")

	body, err := json.Marshal(map[string]any{
		"id":            "deepseek-test",
		"provider_type": "deepseek",
		"api_key":       "deepseek-key",
		"base_url":      "https://deepseek.example",
	})
	if err != nil {
		t.Fatalf("marshal create request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/llm/providers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: got %d want %d", rec.Code, http.StatusCreated)
	}

	if cred := credMgr.GetCredential("provider-config-api-key:deepseek-test"); cred != nil {
		t.Fatalf("provider config credential should not be synced, got %#v", cred)
	}
}

func TestProviderDeleteRejectsStaticProvider(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		providerStore: &testProviderConfigStore{items: map[string]*provider.ProviderConfig{}},
	}, nil, nil, nil, nil, map[string]provider.Provider{
		"openai-main": &stubAdminProvider{cfg: provider.ProviderConfig{Id: "openai-main", ProviderType: "openai"}},
	}), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodDelete, "/admin/llm/providers/openai-main", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("unexpected delete status: got %d want %d", rec.Code, http.StatusConflict)
	}
}

func TestProviderDeleteRemovesManagedModelsForProvider(t *testing.T) {
	providerStore := &testProviderConfigStore{items: map[string]*provider.ProviderConfig{
		"openai-main":   {Id: "openai-main", ProviderType: "openai"},
		"deepseek-main": {Id: "deepseek-main", ProviderType: "deepseek"},
	}}
	modelStore := &testModelStore{items: map[string]*modelcatalog.ManagedModel{
		"openai-main\x00gpt-4.1": {
			ProviderID:    "openai-main",
			UpstreamModel: "gpt-4.1",
			Enabled:       true,
		},
		"openai-main\x00gpt-4.1-mini": {
			ProviderID:    "openai-main",
			UpstreamModel: "gpt-4.1-mini",
			Enabled:       true,
		},
		"deepseek-main\x00deepseek-v4-pro": {
			ProviderID:    "deepseek-main",
			UpstreamModel: "deepseek-v4-pro",
			Enabled:       true,
		},
	}}
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		providerStore: providerStore,
		modelStore:    modelStore,
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodDelete, "/admin/llm/providers/openai-main", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected delete status: got %d want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if _, ok := providerStore.items["openai-main"]; ok {
		t.Fatal("provider openai-main was not deleted")
	}
	if _, ok := modelStore.items["openai-main\x00gpt-4.1"]; ok {
		t.Fatal("managed model openai-main/gpt-4.1 was not deleted")
	}
	if _, ok := modelStore.items["openai-main\x00gpt-4.1-mini"]; ok {
		t.Fatal("managed model openai-main/gpt-4.1-mini was not deleted")
	}
	if _, ok := modelStore.items["deepseek-main\x00deepseek-v4-pro"]; !ok {
		t.Fatal("managed model for another provider was deleted")
	}
}

func TestProviderDeleteRejectsRouteReferences(t *testing.T) {
	providerStore := &testProviderConfigStore{items: map[string]*provider.ProviderConfig{
		"openai-main": {Id: "openai-main", ProviderType: "openai"},
	}}
	modelStore := &testModelStore{items: map[string]*modelcatalog.ManagedModel{
		"openai-main\x00gpt-4.1": {
			ProviderID:    "openai-main",
			UpstreamModel: "gpt-4.1",
			Enabled:       true,
		},
	}}
	routeStore := &testRouteStore{items: map[string]*routecore.AgentRouteConfig{
		"chat": mustRouteConfig(t, llmroutepkg.LLMRoute{
			AgentRouteConfig: llmroutepkg.AgentRouteConfig{
				ID:       "chat",
				Kind:     routecore.RouteKindLLM,
				Protocol: routecore.RouteProtocolOpenAI,
			},
			TargetPolicy: &llmroutepkg.RouteLogicalModelTargetPolicy{
				DefaultModel: "chat-default",
				ModelTargets: []llmroutepkg.RouteModelTarget{{
					Name: "chat-default",
					Candidates: []llmroutepkg.RouteModelCandidate{{
						ProviderID:    "openai-main",
						UpstreamModel: "gpt-4.1",
						Weight:        100,
					}},
				}},
			},
		}),
	}}
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		providerStore: providerStore,
		routeStore:    routeStore,
		modelStore:    modelStore,
	}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodDelete, "/admin/llm/providers/openai-main", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("unexpected delete status: got %d want %d body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if _, ok := providerStore.items["openai-main"]; !ok {
		t.Fatal("referenced provider was deleted")
	}
	if _, ok := modelStore.items["openai-main\x00gpt-4.1"]; !ok {
		t.Fatal("managed model for referenced provider was deleted")
	}
	if !strings.Contains(rec.Body.String(), "chat") {
		t.Fatalf("response body %q does not mention referencing route", rec.Body.String())
	}
}

func TestProtectedRouteAllowsRequestsWithoutEmbeddedAdminAuth(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		virtualKeyStore: &testVirtualKeyStore{items: map[string]*virtualkeypkg.VirtualKey{}},
	}, nil, nil, nil, nil), nil)

	req := httptest.NewRequest(http.MethodGet, "/admin/virtual_keys", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected list status: got %d want %d", rec.Code, http.StatusOK)
	}
}

func TestCreateVirtualKeyDoesNotBindTagToSessionUser(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		virtualKeyStore: &testVirtualKeyStore{items: map[string]*virtualkeypkg.VirtualKey{}},
	}, nil, nil, nil, nil), nil)

	token := loginForTest(t, handler, "admin", "secret-pass")

	body, err := json.Marshal(virtualkeypkg.VirtualKey{
		ID:  "vk-tag-test",
		Tag: "someone-else",
	})
	if err != nil {
		t.Fatalf("marshal virtual key: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/virtual_keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: got %d want %d", rec.Code, http.StatusCreated)
	}

	var created virtualkeypkg.VirtualKey
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode created virtual key: %v", err)
	}
	if created.Tag != "someone-else" {
		t.Fatalf("created tag = %q, want someone-else", created.Tag)
	}
}

func TestListVirtualKeysFiltersByTag(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		virtualKeyStore: &testVirtualKeyStore{items: map[string]*virtualkeypkg.VirtualKey{
			"vk-admin": {ID: "vk-admin", Key: "lk-admin", Tag: "admin"},
			"vk-other": {ID: "vk-other", Key: "lk-other", Tag: "someone-else"},
		}},
	}, nil, nil, nil, nil), nil)

	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodGet, "/admin/virtual_keys?tag=someone-else", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected list status: got %d want %d", rec.Code, http.StatusOK)
	}

	var got struct {
		Items []VirtualKeyView `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode virtual keys: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].ID != "vk-other" || got.Items[0].Key != "lk-other" {
		t.Fatalf("unexpected filtered virtual keys: %#v", got.Items)
	}
}

func TestLLMRouteGetPrefersStaticRouteConfigManager(t *testing.T) {
	store := &testRouteStore{
		items: map[string]*routecore.AgentRouteConfig{
			"chat-prod": mustRouteConfig(t, llmroutepkg.LLMRoute{
				AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "chat-prod"},
				TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
					ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai"},
				},
			}),
		},
	}
	handler := NewHandler(newTestAgentGateway(&testConfigStore{routeStore: store}, nil, nil, []llmroutepkg.LLMRoute{{
		AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "chat-prod"},
		TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
			ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "anthropic"},
		},
	}}, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/routes/chat-prod", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected get status: got %d want %d", rec.Code, http.StatusOK)
	}

	var got LLMRouteView
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode route: %v", err)
	}
	if got.ID != "chat-prod" {
		t.Fatalf("route id = %q, want chat-prod", got.ID)
	}
	if got.Source != "caddyfile" || !got.ReadOnly {
		t.Fatalf("unexpected route metadata: %#v", got)
	}
}

func TestLLMRouteListMarksStaticRoutesAsReadOnly(t *testing.T) {
	store := &testRouteStore{
		items: map[string]*routecore.AgentRouteConfig{
			"chat-dynamic": mustRouteConfig(t, llmroutepkg.LLMRoute{
				AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "chat-dynamic"},
				TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
					ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "openai"},
				},
			}),
		},
	}
	handler := NewHandler(newTestAgentGateway(&testConfigStore{routeStore: store}, nil, nil, []llmroutepkg.LLMRoute{{
		AgentRouteConfig: llmroutepkg.AgentRouteConfig{ID: "chat-static"},
		TargetPolicy: &llmroutepkg.RouteDirectProviderPolicy{
			ProviderTarget: llmroutepkg.DirectProviderTarget{ProviderID: "anthropic"},
		},
	}}, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/routes", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected list status: got %d want %d", rec.Code, http.StatusOK)
	}

	var got struct {
		Items []LLMRouteView `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode routes: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("item count = %d, want 2", len(got.Items))
	}

	byID := map[string]LLMRouteView{}
	for _, item := range got.Items {
		byID[item.ID] = item
	}
	if byID["chat-static"].Source != "caddyfile" || !byID["chat-static"].ReadOnly {
		t.Fatalf("unexpected static route metadata: %#v", byID["chat-static"])
	}
	if byID["chat-dynamic"].Source != "store" || byID["chat-dynamic"].ReadOnly {
		t.Fatalf("unexpected dynamic route metadata: %#v", byID["chat-dynamic"])
	}
}

func TestManagedModelViewIncludesResolvedAndSnapshotFields(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{
		modelStore: &testModelStore{
			items: map[string]*modelcatalog.ManagedModel{
				"openai-main\x00gpt-4.1-mini": {
					ProviderID:    "openai-main",
					UpstreamModel: "gpt-4.1-mini",
					Enabled:       true,
				},
			},
		},
	}, nil, nil, nil, nil, map[string]provider.Provider{
		"openai-main": &stubAdminProvider{
			cfg: provider.ProviderConfig{Id: "openai-main", ProviderType: "openai"},
			models: []provider.ModelInfo{{
				ID:          "gpt-4.1-mini",
				DisplayName: "GPT 4.1 Mini",
				Description: "fast chat",
				Capabilities: provider.ModelCapabilities{
					Streaming: true,
					Tools:     true,
				},
			}},
		},
	}), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	req := httptest.NewRequest(http.MethodGet, "/admin/llm/models/managed/openai-main/gpt-4.1-mini", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected get status: got %d want %d", rec.Code, http.StatusOK)
	}

	var got ManagedConcreteModelView
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode managed model view: %v", err)
	}
	if got.ProviderType != "openai" {
		t.Fatalf("ProviderType = %q, want openai", got.ProviderType)
	}
	if got.DisplayName != "GPT 4.1 Mini" {
		t.Fatalf("DisplayName = %q, want %q", got.DisplayName, "GPT 4.1 Mini")
	}
	if !got.Capabilities.Streaming || !got.Capabilities.Tools {
		t.Fatalf("Capabilities = %#v, want streaming+tools", got.Capabilities)
	}
	if got.SnapshotState != modelcatalog.SnapshotStatusOK {
		t.Fatalf("SnapshotState = %q, want %q", got.SnapshotState, modelcatalog.SnapshotStatusOK)
	}
}

func TestCreateManagedModelUsesStoreCreate(t *testing.T) {
	store := &testModelStore{}
	handler := NewHandler(newTestAgentGateway(&testConfigStore{modelStore: store}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	body := bytes.NewBufferString(`{"provider_id":"openai-main","upstream_model":"gpt-4.1","enabled":true}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/llm/models/managed", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("unexpected create status: got %d want %d body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if _, ok := store.items["openai-main\x00gpt-4.1"]; !ok {
		t.Fatalf("managed model was not created")
	}
}

func TestUpdateManagedModelUsesStoreUpdate(t *testing.T) {
	store := &testModelStore{
		items: map[string]*modelcatalog.ManagedModel{
			"openai-main\x00gpt-4.1": {
				ProviderID:    "openai-main",
				UpstreamModel: "gpt-4.1",
				Enabled:       false,
			},
		},
	}
	handler := NewHandler(newTestAgentGateway(&testConfigStore{modelStore: store}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	body := bytes.NewBufferString(`{"enabled":true}`)
	req := httptest.NewRequest(http.MethodPut, "/admin/llm/models/managed/openai-main/gpt-4.1", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected update status: got %d want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !store.items["openai-main\x00gpt-4.1"].Enabled {
		t.Fatalf("managed model was not updated")
	}
}

func TestUpdateManagedModelReturnsNotFoundWhenMissing(t *testing.T) {
	handler := NewHandler(newTestAgentGateway(&testConfigStore{modelStore: &testModelStore{}}, nil, nil, nil, nil), nil)
	token := loginForTest(t, handler, "admin", "secret-pass")

	body := bytes.NewBufferString(`{"enabled":true}`)
	req := httptest.NewRequest(http.MethodPut, "/admin/llm/models/managed/openai-main/gpt-4.1", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unexpected update status: got %d want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestACPAdminAuditRecordsTraceID(t *testing.T) {
	sink := &adminCaptureSink{}
	observer := usage.NewObserver(sink)
	handler := &Handler{usageObserver: observer}

	req := httptest.NewRequest(http.MethodGet, "/admin/acp/runtime/inflight", nil)
	rec := httptest.NewRecorder()
	handler.handleListACPInFlight(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.events))
	}
	ev, ok := sink.events[0].(usage.ACPUsageEvent)
	if !ok {
		t.Fatalf("event type = %T, want ACPUsageEvent", sink.events[0])
	}
	if !usage.ValidTraceID(ev.TraceID) {
		t.Fatalf("trace_id = %q, want generated W3C trace id", ev.TraceID)
	}
	if !usage.ValidSpanID(ev.SpanID) {
		t.Fatalf("span_id = %q, want generated W3C span id", ev.SpanID)
	}
}

func TestACPAdminAuditUsesDirectAgentIdentityWithoutServiceID(t *testing.T) {
	sink := &adminCaptureSink{}
	handler := &Handler{usageObserver: usage.NewObserver(sink)}
	req := httptest.NewRequest(http.MethodDelete, "/admin/acp/runtime/agents/acp-agent/threads/thread-1", nil)
	req.SetPathValue("agent_id", "acp-agent")
	req.SetPathValue("thread_id", "thread-1")
	rec := httptest.NewRecorder()
	handler.handleCloseACPThread(rec, req)

	if rec.Code != http.StatusServiceUnavailable || len(sink.events) != 1 {
		t.Fatalf("status/events = %d/%#v", rec.Code, sink.events)
	}
	ev, ok := sink.events[0].(usage.ACPUsageEvent)
	if !ok || ev.RouteID != "/admin/acp" || ev.RouteKind != "acp" || ev.RouteProtocol != "admin" || ev.AgentID != "acp-agent" || ev.RuntimeType != agentpkg.RuntimeTypeACP || ev.ServiceID != "" || ev.Operation != "thread_close" || ev.ThreadID != "thread-1" || ev.ResultStatus != "error" {
		t.Fatalf("ACP Admin audit = %#v", sink.events[0])
	}
}

func loginForTest(t *testing.T, handler *Handler, username string, password string) string {
	t.Helper()
	_ = handler
	_ = username
	_ = password
	return "external-auth"
}
