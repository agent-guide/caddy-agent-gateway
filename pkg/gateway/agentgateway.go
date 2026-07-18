package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/internal/statuserr"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	builtinpkg "github.com/agent-guide/agent-gateway/pkg/agent/builtin"
	"github.com/agent-guide/agent-gateway/pkg/cliauth"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	"github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	acproutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/acproute"
	builtinroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/builtinroute"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	mcproutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/mcproute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	"github.com/agent-guide/agent-gateway/pkg/llm/credentialmgr"
	credentialmgrscheduler "github.com/agent-guide/agent-gateway/pkg/llm/credentialmgr/scheduler"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	einomodelbridge "github.com/agent-guide/agent-gateway/pkg/llm/provider/einomodel"
	mcpruntime "github.com/agent-guide/agent-gateway/pkg/mcp/runtime"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
	einocomponentmodel "github.com/cloudwego/eino/components/model"
	"go.uber.org/zap"
)

type BootstrapOptions struct {
	StaticLLMRoutes     []routecore.AgentRouteConfig
	StaticMCPRoutes     []routecore.AgentRouteConfig
	StaticACPRoutes     []routecore.AgentRouteConfig
	StaticProviders     map[string]provider.Provider
	ConfigStoreBackend  configstore.ConfigStoreBackend
	CLIAuthManager      *cliauth.Manager
	CLIAuthRefresher    *cliauth.AutoRefresher
	CredentialManager   *credentialmgr.Manager
	CredentialScheduler credentialmgrscheduler.CredentialScheduler
	UsageObserver       usage.InteractionObserver
	UsageQuery          usage.QueryService
	UsageStats          usage.RuntimeStats
	UsagePrometheus     usage.PrometheusProvider
	UsageConfig         usage.Config
	Logger              *zap.Logger
}

type AgentGateway struct {
	mu sync.RWMutex

	configured           bool
	configStoreBackend   configstore.ConfigStoreBackend
	routeConfigManager   *routecore.AgentRouteConfigManager
	llmRouteResolver     *llmroutepkg.LLMRouteResolver
	mcpRouteResolver     *mcproutepkg.MCPRouteResolver
	acpRouteResolver     *acproutepkg.ACPRouteResolver
	builtinRouteResolver *builtinroutepkg.BuiltinRouteResolver
	builtinHost          *builtinpkg.Host
	virtualKeyManager    *virtualkeypkg.VirtualKeyManager
	providerManager      *ProviderManager
	cliauthManager       *cliauth.Manager
	cliauthRefresher     *cliauth.AutoRefresher
	credentialManager    *credentialmgr.Manager
	credentialScheduler  credentialmgrscheduler.CredentialScheduler
	modelCatalog         modelcatalog.Service
	mcpServiceManager    *mcpservice.Manager
	mcpRuntimeRegistry   *mcpruntime.Registry
	acpServiceManager    *acpservice.Manager
	acpRuntimeManager    *acpruntime.Manager
	agentManager         *agentpkg.Manager
	usageObserver        usage.InteractionObserver
	usageQuery           usage.QueryService
	usageStats           usage.RuntimeStats
	usagePrometheus      usage.PrometheusProvider
	usageConfig          usage.Config
}

func NewAgentGateway() *AgentGateway {
	return &AgentGateway{
		configured:         false,
		mcpRuntimeRegistry: mcpruntime.NewRegistry(),
		usageObserver:      usage.NoopObserver{},
	}
}

func (g *AgentGateway) Bootstrap(ctx context.Context, opts BootstrapOptions) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.configureConfigStoreBackend(opts.ConfigStoreBackend)
	staticRoutes := make([]routecore.AgentRouteConfig, 0, len(opts.StaticLLMRoutes)+len(opts.StaticMCPRoutes)+len(opts.StaticACPRoutes))
	staticRoutes = append(staticRoutes, opts.StaticLLMRoutes...)
	staticRoutes = append(staticRoutes, opts.StaticMCPRoutes...)
	staticRoutes = append(staticRoutes, opts.StaticACPRoutes...)
	if err := g.configureRouteResolver(ctx, opts.ConfigStoreBackend, staticRoutes); err != nil {
		return err
	}
	if err := g.configureMCPServiceManager(opts.ConfigStoreBackend); err != nil {
		return err
	}
	if err := g.configureACPServiceManager(opts.ConfigStoreBackend); err != nil {
		return err
	}
	if err := g.configureAgentManager(ctx, opts.ConfigStoreBackend); err != nil {
		return err
	}
	if err := g.configureVirtualKeyManager(ctx, opts.ConfigStoreBackend); err != nil {
		return err
	}
	if err := g.configureProviderResolver(ctx, opts.ConfigStoreBackend, opts.StaticProviders); err != nil {
		return err
	}
	g.cliauthManager = opts.CLIAuthManager
	g.cliauthRefresher = opts.CLIAuthRefresher
	g.credentialManager = opts.CredentialManager
	g.credentialScheduler = opts.CredentialScheduler
	if opts.UsageObserver != nil {
		g.usageObserver = opts.UsageObserver
	} else {
		g.usageObserver = usage.NoopObserver{}
	}
	g.usageQuery = opts.UsageQuery
	g.usageStats = opts.UsageStats
	g.usagePrometheus = opts.UsagePrometheus
	g.usageConfig = opts.UsageConfig.Normalized()
	if err := g.configureModelCatalog(ctx, opts.ConfigStoreBackend, opts.Logger); err != nil {
		return err
	}
	// The builtin host is constructed last: it consumes the agent manager, the
	// LLM route layer (through the RoutedProvider -> eino chat model adapter),
	// the MCP service manager, and the usage observer configured above.
	if g.agentManager != nil {
		g.builtinHost = builtinpkg.NewHost(builtinpkg.Config{
			Agents:   g.agentManager,
			Models:   routedChatModelResolver{gateway: g},
			Tools:    g.mcpServiceManager,
			Observer: g.usageObserver,
		})
	}
	g.configured = true
	return nil
}

func (g *AgentGateway) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.configured = false
	g.configStoreBackend = nil
	g.routeConfigManager = nil
	g.llmRouteResolver = nil
	g.mcpRouteResolver = nil
	g.acpRouteResolver = nil
	g.builtinRouteResolver = nil
	g.builtinHost = nil
	g.virtualKeyManager = nil
	g.providerManager = nil
	g.cliauthManager = nil
	g.cliauthRefresher = nil
	g.credentialManager = nil
	g.credentialScheduler = nil
	g.modelCatalog = nil
	g.mcpServiceManager = nil
	g.mcpRuntimeRegistry = mcpruntime.NewRegistry()
	if g.acpRuntimeManager != nil {
		g.acpRuntimeManager.Close()
	}
	g.acpServiceManager = nil
	g.acpRuntimeManager = nil
	g.agentManager = nil
	g.usageObserver = usage.NoopObserver{}
	g.usageQuery = nil
	g.usageStats = nil
	g.usagePrometheus = nil
	g.usageConfig = usage.Config{}
}

func (g *AgentGateway) Close() {
	g.Reset()
}

func (g *AgentGateway) ConfigStoreBackend() configstore.ConfigStoreBackend {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.configStoreBackend
}

func (g *AgentGateway) CLIAuthManager() *cliauth.Manager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.cliauthManager
}

func (g *AgentGateway) CLIAuthRefresher() *cliauth.AutoRefresher {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.cliauthRefresher
}

func (g *AgentGateway) CredentialManager() *credentialmgr.Manager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.credentialManager
}

func (g *AgentGateway) CredentialScheduler() credentialmgrscheduler.CredentialScheduler {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.credentialScheduler
}

func (g *AgentGateway) AgentRouteConfigManager() *routecore.AgentRouteConfigManager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.routeConfigManager
}

func (g *AgentGateway) LLMRouteResolver() *llmroutepkg.LLMRouteResolver {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.llmRouteResolver
}

func (g *AgentGateway) MCPRouteConfigManager() *routecore.AgentRouteConfigManager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.routeConfigManager
}

func (g *AgentGateway) MCPRouteResolver() *mcproutepkg.MCPRouteResolver {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.mcpRouteResolver
}

func (g *AgentGateway) ACPRouteConfigManager() *routecore.AgentRouteConfigManager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.routeConfigManager
}

func (g *AgentGateway) ACPRouteResolver() *acproutepkg.ACPRouteResolver {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.acpRouteResolver
}

func (g *AgentGateway) BuiltinRouteResolver() *builtinroutepkg.BuiltinRouteResolver {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.builtinRouteResolver
}

// BuiltinHost returns the in-process ADK host serving builtin-runtime agents.
func (g *AgentGateway) BuiltinHost() *builtinpkg.Host {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.builtinHost
}

func (g *AgentGateway) VirtualKeyManager() *virtualkeypkg.VirtualKeyManager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.virtualKeyManager
}

func (g *AgentGateway) ProviderManager() *ProviderManager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.providerManager
}

func (g *AgentGateway) ModelCatalog() modelcatalog.Service {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.modelCatalog
}

func (g *AgentGateway) MCPServiceManager() *mcpservice.Manager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.mcpServiceManager
}

func (g *AgentGateway) MCPRuntimeRegistry() *mcpruntime.Registry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.mcpRuntimeRegistry
}

func (g *AgentGateway) ACPServiceManager() *acpservice.Manager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.acpServiceManager
}

func (g *AgentGateway) ACPRuntimeManager() *acpruntime.Manager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.acpRuntimeManager
}

func (g *AgentGateway) AgentManager() *agentpkg.Manager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.agentManager
}

func (g *AgentGateway) UsageObserver() usage.InteractionObserver {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.usageObserver == nil {
		return usage.NoopObserver{}
	}
	return g.usageObserver
}

func (g *AgentGateway) UsageQuery() usage.QueryService {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.usageQuery
}

func (g *AgentGateway) UsageStats() usage.RuntimeStats {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.usageStats
}

func (g *AgentGateway) UsagePrometheus() usage.PrometheusProvider {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.usagePrometheus
}

func (g *AgentGateway) UsageConfig() usage.Config {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.usageConfig.Normalized()
}

func (g *AgentGateway) Match(ctx context.Context, r *http.Request) (routecore.AgentRouteConfig, error) {
	g.mu.RLock()
	manager := g.routeConfigManager
	g.mu.RUnlock()

	if manager == nil {
		return routecore.AgentRouteConfig{}, statuserr.New(http.StatusServiceUnavailable, "llm route config manager is not configured")
	}

	route, ok, err := manager.Match(ctx, r)
	if err != nil {
		return routecore.AgentRouteConfig{}, statuserr.New(http.StatusInternalServerError, fmt.Sprintf("match route: %v", err))
	}
	if !ok {
		return routecore.AgentRouteConfig{}, nil
	}
	return route, nil
}

func (g *AgentGateway) ResolveRoute(ctx context.Context, r *http.Request) (*llmroutepkg.LLMRoute, error) {
	cfg, err := g.Match(ctx, r)
	if err != nil {
		return nil, err
	}
	if cfg.ID == "" || cfg.Kind != routecore.RouteKindLLM {
		return nil, nil
	}

	routeResolver := g.LLMRouteResolver()
	if routeResolver == nil {
		return nil, statuserr.New(http.StatusServiceUnavailable, "llm route resolver is not configured")
	}
	route, err := routeResolver.Resolve(ctx, cfg)
	if err != nil {
		return nil, statuserr.New(http.StatusInternalServerError, fmt.Sprintf("get llm route %q: %v", cfg.ID, err))
	}
	return route, nil
}

func (g *AgentGateway) NewRoutedProvider(route *llmroutepkg.LLMRoute, requestRequirements llmroutepkg.RequestRequirements) (*RoutedProvider, error) {
	resolver := g.providerResolver()
	if resolver == nil {
		return nil, statuserr.New(http.StatusServiceUnavailable, "provider resolver is not configured")
	}
	if route == nil {
		return nil, statuserr.New(http.StatusServiceUnavailable, "llm route is not configured")
	}
	return &RoutedProvider{
		route:               route,
		requestRequirements: requestRequirements,
		providerResolver:    resolver,
		providerConfigs:     g.ProviderManager(),
		modelCatalog:        g.ModelCatalog(),
		credentialMgr:       g.CredentialManager(),
		scheduler:           g.CredentialScheduler(),
	}, nil
}

func (g *AgentGateway) ResolveVirtualKey(ctx context.Context, httpReq *http.Request, r routecore.AgentRouteConfig) (*virtualkeypkg.VirtualKey, error) {
	if !r.AuthPolicy.RequireVirtualKey {
		return nil, nil
	}
	candidates := virtualkeypkg.ExtractAPIKeys(httpReq)
	if len(candidates) == 0 {
		return nil, statuserr.New(http.StatusUnauthorized, "virtual key is required")
	}

	g.mu.RLock()
	virtualKeyManager := g.virtualKeyManager
	g.mu.RUnlock()
	if virtualKeyManager == nil {
		return nil, statuserr.New(http.StatusServiceUnavailable, "virtual key manager is not configured")
	}

	// A request may carry candidate keys in both `Authorization` and
	// `x-api-key`; accept the first one that resolves to a usable virtual key
	// for this route, so an unrelated key in one header cannot shadow the valid
	// key in the other.
	var lastErr error = statuserr.New(http.StatusUnauthorized, "invalid virtual key")
	for _, rawKey := range candidates {
		virtualKey, err := virtualKeyManager.GetByKey(ctx, rawKey)
		if err != nil {
			lastErr = statuserr.New(http.StatusUnauthorized, "invalid virtual key")
			continue
		}
		if err := virtualKey.ValidateForRoute(r.ID); err != nil {
			lastErr = err
			continue
		}
		return &virtualKey, nil
	}
	return nil, lastErr
}

func (g *AgentGateway) providerResolver() ProviderResolver {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.providerManager
}

func (g *AgentGateway) configureConfigStoreBackend(configStoreBackend configstore.ConfigStoreBackend) {
	g.configStoreBackend = configStoreBackend
}

func (g *AgentGateway) configureRouteResolver(ctx context.Context, configStoreBackend configstore.ConfigStoreBackend, staticRoutes []routecore.AgentRouteConfig) error {
	_ = ctx
	if g.routeConfigManager != nil || g.llmRouteResolver != nil || g.mcpRouteResolver != nil || g.acpRouteResolver != nil {
		return fmt.Errorf("route config manager and resolvers are already configured")
	}

	var routeStore configstore.ConfigStore
	if configStoreBackend != nil {
		var err error
		routeStore, err = configStoreBackend.Get(schema.StoreRoutes)
		if err != nil {
			return fmt.Errorf("get llm route store: %w", err)
		}
	}
	g.routeConfigManager = routecore.NewAgentRouteConfigManager(routeStore)
	g.routeConfigManager.InitStaticRoutes(staticRoutes)
	if err := g.routeConfigManager.Refresh(ctx); err != nil {
		return fmt.Errorf("load route configs: %w", err)
	}
	g.llmRouteResolver = llmroutepkg.NewLLMRouteResolver(g.routeConfigManager)
	g.mcpRouteResolver = mcproutepkg.NewMCPRouteResolver(g.routeConfigManager)
	g.acpRouteResolver = acproutepkg.NewACPRouteResolver(g.routeConfigManager)
	g.builtinRouteResolver = builtinroutepkg.NewBuiltinRouteResolver(g.routeConfigManager)

	return nil
}

func (g *AgentGateway) configureMCPServiceManager(configStoreBackend configstore.ConfigStoreBackend) error {
	if g.mcpServiceManager != nil {
		return nil
	}
	if configStoreBackend == nil {
		return nil
	}
	store, err := configStoreBackend.Get(schema.StoreMCPServices)
	if err != nil {
		return err
	}
	g.mcpServiceManager = mcpservice.NewManager(store)
	return nil
}

func (g *AgentGateway) configureACPServiceManager(configStoreBackend configstore.ConfigStoreBackend) error {
	if g.acpServiceManager != nil {
		return nil
	}
	if configStoreBackend == nil {
		return nil
	}
	store, err := configStoreBackend.Get(schema.StoreACPServices)
	if err != nil {
		return err
	}
	g.acpServiceManager = acpservice.NewManager(store)
	g.acpRuntimeManager = acpruntime.NewManager(g.acpServiceManager)
	return nil
}

func (g *AgentGateway) configureAgentManager(ctx context.Context, configStoreBackend configstore.ConfigStoreBackend) error {
	if g.agentManager != nil {
		return nil
	}
	if configStoreBackend == nil {
		return nil
	}
	store, err := configStoreBackend.Get(schema.StoreAgents)
	if err != nil {
		return err
	}
	manager := agentpkg.NewManager(store)
	manager.SetRouteLookup(agentRouteLookup{
		acp:     g.acpRouteResolver,
		builtin: g.builtinRouteResolver,
	})
	if err := manager.Refresh(ctx); err != nil {
		return fmt.Errorf("load agents: %w", err)
	}
	g.agentManager = manager
	return nil
}

// agentRouteLookup adapts the ACP and builtin route resolvers to the agent
// manager's lookup seams so the manager can validate acp_route_ids and
// builtin_route_ids without depending on the resolver types directly.
type agentRouteLookup struct {
	acp     *acproutepkg.ACPRouteResolver
	builtin *builtinroutepkg.BuiltinRouteResolver
}

func (l agentRouteLookup) ACPRouteServiceID(ctx context.Context, routeID string) (string, error) {
	if l.acp == nil {
		return "", fmt.Errorf("acp route resolver is not configured")
	}
	route, err := l.acp.ResolveByID(ctx, routeID)
	if err != nil {
		return "", err
	}
	if route == nil {
		return "", fmt.Errorf("acp route %q not found", routeID)
	}
	return route.ServiceID, nil
}

func (l agentRouteLookup) BuiltinRouteAgentID(ctx context.Context, routeID string) (string, error) {
	if l.builtin == nil {
		return "", fmt.Errorf("builtin route resolver is not configured")
	}
	route, err := l.builtin.ResolveByID(ctx, routeID)
	if err != nil {
		return "", err
	}
	if route == nil {
		return "", fmt.Errorf("builtin route %q not found", routeID)
	}
	return route.AgentID, nil
}

// routedChatModelResolver implements the builtin host's ChatModelResolver:
// every builtin model reference resolves through an LLM route into a
// RoutedProvider wrapped by the einomodel bridge, so credential scheduling,
// candidate fallback, and usage attribution apply unchanged to in-process
// agents.
type routedChatModelResolver struct {
	gateway *AgentGateway
}

func (r routedChatModelResolver) ResolveChatModel(ctx context.Context, llmRouteID, model string, requireTools bool) (einocomponentmodel.ToolCallingChatModel, error) {
	resolver := r.gateway.LLMRouteResolver()
	if resolver == nil {
		return nil, fmt.Errorf("llm route resolver is not configured")
	}
	route, err := resolver.ResolveByID(ctx, llmRouteID)
	if err != nil {
		return nil, err
	}
	if route == nil {
		return nil, fmt.Errorf("llm route %q not found", llmRouteID)
	}
	routed, err := r.gateway.NewRoutedProvider(route, llmroutepkg.RequestRequirements{Model: model, RequireTools: requireTools})
	if err != nil {
		return nil, err
	}
	return einomodelbridge.New(routed, model)
}

func (g *AgentGateway) configureVirtualKeyManager(ctx context.Context, configStoreBackend configstore.ConfigStoreBackend) error {
	_ = ctx
	if g.virtualKeyManager != nil {
		return fmt.Errorf("virtual key manager is not nil")
	}

	var virtualKeyStore configstore.ConfigStore
	if configStoreBackend != nil {
		var err error
		virtualKeyStore, err = configStoreBackend.Get(schema.StoreVirtualKeys)
		if err != nil {
			return fmt.Errorf("get virtual key store: %w", err)
		}
	}

	g.virtualKeyManager = virtualkeypkg.NewVirtualKeyManager(virtualKeyStore)
	return nil
}

func (g *AgentGateway) configureProviderResolver(ctx context.Context, configStoreBackend configstore.ConfigStoreBackend, staticProviders map[string]provider.Provider) error {
	_ = ctx
	if g.providerManager != nil {
		return fmt.Errorf("provider resolver is not nil")
	}

	var providerStore configstore.ConfigStore
	if configStoreBackend != nil {
		var err error
		providerStore, err = configStoreBackend.Get(schema.StoreProviders)
		if err != nil {
			return fmt.Errorf("get provider config store: %w", err)
		}
	}

	providerManager := NewProviderManager(providerStore)
	if err := providerManager.InitStaticProviders(staticProviders); err != nil {
		return fmt.Errorf("init static providers: %w", err)
	}
	g.providerManager = providerManager
	return nil
}

func (g *AgentGateway) configureModelCatalog(ctx context.Context, configStoreBackend configstore.ConfigStoreBackend, logger *zap.Logger) error {
	_ = ctx
	if g.modelCatalog != nil {
		return fmt.Errorf("model catalog is not nil")
	}

	var modelStore configstore.ConfigStore
	if configStoreBackend != nil {
		var err error
		modelStore, err = configStoreBackend.Get(schema.StoreManagedModels)
		if err != nil {
			return fmt.Errorf("get model store: %w", err)
		}
	}
	g.modelCatalog = modelcatalog.NewService(modelStore, g.providerManager, logger)
	return nil
}
