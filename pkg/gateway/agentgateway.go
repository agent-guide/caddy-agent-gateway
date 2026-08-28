package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/internal/statuserr"
	acphost "github.com/agent-guide/agent-gateway/pkg/acp/host"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	builtinpkg "github.com/agent-guide/agent-gateway/pkg/agent/builtin"
	agentruntime "github.com/agent-guide/agent-gateway/pkg/agent/runtime"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	"github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	"github.com/agent-guide/agent-gateway/pkg/credential"
	credentialscheduler "github.com/agent-guide/agent-gateway/pkg/credential/scheduler"
	agentroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/agentroute"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	mcproutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/mcproute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
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
	StaticAgentRoutes   []routecore.AgentRouteConfig
	StaticProviders     map[string]provider.Provider
	ConfigStoreBackend  configstore.ConfigStoreBackend
	CredentialManager   *credential.Manager
	CredentialScheduler credentialscheduler.CredentialScheduler
	UsageObserver       usage.InteractionObserver
	UsageQuery          usage.QueryService
	UsageStats          usage.RuntimeStats
	UsagePrometheus     usage.PrometheusProvider
	UsageConfig         usage.Config
	Logger              *zap.Logger
	// ACPRuntime overrides the native ACP turn server the registered ACP
	// backend drives. Nil uses the process-pool runtime manager. Tests inject
	// a fake to exercise Agent-dispatched ACP execution without spawning
	// processes.
	ACPRuntime ACPTurnServer
}

type AgentGateway struct {
	mu sync.RWMutex

	configured          bool
	configStoreBackend  configstore.ConfigStoreBackend
	routeConfigManager  *routecore.AgentRouteConfigManager
	llmRouteResolver    *llmroutepkg.LLMRouteResolver
	mcpRouteResolver    *mcproutepkg.MCPRouteResolver
	agentRouteResolver  *agentroutepkg.AgentRouteResolver
	builtinHost         *builtinpkg.Host
	virtualKeyManager   *virtualkeypkg.VirtualKeyManager
	providerManager     *ProviderManager
	credentialManager   *credential.Manager
	credentialScheduler credentialscheduler.CredentialScheduler
	modelCatalog        modelcatalog.Service
	mcpServiceManager   *mcpservice.Manager
	mcpRuntimeRegistry  *mcpruntime.Registry
	acpRuntimeManager   *acphost.Manager
	agentManager        *agentpkg.Manager
	runtimeRegistry     *agentruntime.Registry
	runRegistry         *agentruntime.RunRegistry
	permissionBroker    *agentruntime.PermissionBroker
	usageObserver       usage.InteractionObserver
	usageQuery          usage.QueryService
	usageStats          usage.RuntimeStats
	usagePrometheus     usage.PrometheusProvider
	usageConfig         usage.Config
	logger              *zap.Logger
}

func NewAgentGateway() *AgentGateway {
	return &AgentGateway{
		configured:         false,
		mcpRuntimeRegistry: mcpruntime.NewRegistry(),
		runtimeRegistry:    agentruntime.NewRegistry(),
		runRegistry:        agentruntime.NewRunRegistry(),
		permissionBroker:   agentruntime.NewPermissionBroker(),
		usageObserver:      usage.NoopObserver{},
	}
}

func (g *AgentGateway) Bootstrap(ctx context.Context, opts BootstrapOptions) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.configureConfigStoreBackend(opts.ConfigStoreBackend)
	staticRoutes := make([]routecore.AgentRouteConfig, 0, len(opts.StaticLLMRoutes)+len(opts.StaticMCPRoutes)+len(opts.StaticAgentRoutes))
	staticRoutes = append(staticRoutes, opts.StaticLLMRoutes...)
	staticRoutes = append(staticRoutes, opts.StaticMCPRoutes...)
	staticRoutes = append(staticRoutes, opts.StaticAgentRoutes...)
	if err := g.configureRouteResolver(ctx, opts.ConfigStoreBackend, staticRoutes); err != nil {
		return err
	}
	if err := g.configureMCPServiceManager(opts.ConfigStoreBackend); err != nil {
		return err
	}
	g.acpRuntimeManager = acphost.NewManager()
	if err := g.configureAgentManager(ctx, opts.ConfigStoreBackend); err != nil {
		return err
	}
	if err := g.configureVirtualKeyManager(ctx, opts.ConfigStoreBackend); err != nil {
		return err
	}
	if err := g.configureProviderResolver(ctx, opts.ConfigStoreBackend, opts.StaticProviders); err != nil {
		return err
	}
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
	g.logger = opts.Logger
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
	var backends []agentruntime.Backend
	controls := RuntimeControls{Runs: g.runRegistry, Permissions: g.permissionBroker, Logger: opts.Logger}
	var acpBackend *ACPBackend
	if g.acpRuntimeManager != nil {
		turnServer := ACPTurnServer(g.acpRuntimeManager)
		if opts.ACPRuntime != nil {
			turnServer = opts.ACPRuntime
		}
		acpBackend = NewACPBackend(turnServer, controls)
		backends = append(backends, acpBackend)
	}
	if g.builtinHost != nil {
		backends = append(backends, NewBuiltinBackend(g.builtinHost, controls))
	}
	if err := g.runtimeRegistry.RegisterAll(backends...); err != nil {
		return fmt.Errorf("register agent runtime backends: %w", err)
	}
	// The canonical ACP runtime-config snapshot follows the Agent definition
	// generation: it preloads at bootstrap and rebuilds (with fingerprint
	// retirement) on every definition commit, so turn dispatch never reads the
	// service store (docs/plans/unified-agent-runtime.md M4).
	if acpBackend != nil && g.agentManager != nil {
		g.agentManager.AddDefinitionListener(acpBackend.PrepareRuntimeConfigs)
		acpBackend.RefreshRuntimeConfigs(ctx, g.agentManager.Snapshot())
	}
	g.configured = true
	return nil
}

func (g *AgentGateway) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Stop new permission publications and drain every pending continuation
	// before tearing down either native runtime. Claimed builtin decisions live
	// outside the pending broker and are discarded explicitly as well.
	if g.permissionBroker != nil {
		g.permissionBroker.Close(agentruntime.WithPermissionSource(context.Background(), "process_shutdown"))
	}
	if g.runtimeRegistry != nil {
		if backend, err := g.runtimeRegistry.Resolve(agentpkg.RuntimeTypeBuiltin); err == nil {
			if discarder, ok := backend.(interface{ DiscardAllContinuations() }); ok {
				discarder.DiscardAllContinuations()
			}
		}
	}

	g.configured = false
	g.configStoreBackend = nil
	g.routeConfigManager = nil
	g.llmRouteResolver = nil
	g.mcpRouteResolver = nil
	g.agentRouteResolver = nil
	g.builtinHost = nil
	g.virtualKeyManager = nil
	g.providerManager = nil
	g.credentialManager = nil
	g.credentialScheduler = nil
	g.modelCatalog = nil
	g.mcpServiceManager = nil
	g.mcpRuntimeRegistry = mcpruntime.NewRegistry()
	if g.acpRuntimeManager != nil {
		g.acpRuntimeManager.Close()
	}
	g.acpRuntimeManager = nil
	g.agentManager = nil
	g.runtimeRegistry = agentruntime.NewRegistry()
	g.runRegistry = agentruntime.NewRunRegistry()
	g.permissionBroker = agentruntime.NewPermissionBroker()
	g.usageObserver = usage.NoopObserver{}
	g.usageQuery = nil
	g.usageStats = nil
	g.usagePrometheus = nil
	g.usageConfig = usage.Config{}
	g.logger = nil
}

func (g *AgentGateway) Close() {
	g.Reset()
}

func (g *AgentGateway) ConfigStoreBackend() configstore.ConfigStoreBackend {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.configStoreBackend
}

func (g *AgentGateway) CredentialManager() *credential.Manager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.credentialManager
}

func (g *AgentGateway) CredentialScheduler() credentialscheduler.CredentialScheduler {
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

// AgentRouteResolver resolves the unified kind=agent ingress routes. The
// route surface is the only public Agent ingress.
func (g *AgentGateway) AgentRouteResolver() *agentroutepkg.AgentRouteResolver {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.agentRouteResolver
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

func (g *AgentGateway) ACPRuntimeManager() *acphost.Manager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.acpRuntimeManager
}

func (g *AgentGateway) AgentManager() *agentpkg.Manager {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.agentManager
}

// RuntimeRegistry returns the runtime-neutral Agent backend registry. Runtime
// adapters are registered during Bootstrap and consumed by Agent-bound ACP
// and builtin route turns.
func (g *AgentGateway) RuntimeRegistry() *agentruntime.Registry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.runtimeRegistry
}

func (g *AgentGateway) RunRegistry() *agentruntime.RunRegistry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.runRegistry
}

func (g *AgentGateway) PermissionBroker() *agentruntime.PermissionBroker {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.permissionBroker
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
		logger:              g.logger,
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
	if g.routeConfigManager != nil || g.llmRouteResolver != nil || g.mcpRouteResolver != nil || g.agentRouteResolver != nil {
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
	g.agentRouteResolver = agentroutepkg.NewAgentRouteResolver(g.routeConfigManager)

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
	manager.SetRouteLookup(agentRouteLookup{agent: g.agentRouteResolver})
	if err := manager.Refresh(ctx); err != nil {
		return fmt.Errorf("load agents: %w", err)
	}
	g.agentManager = manager
	if g.agentRouteResolver != nil {
		g.agentRouteResolver.SetAgentLookup(manager)
	}
	return nil
}

// agentRouteLookup adapts the unified AgentRoute resolver to the Agent
// manager's deletion-guard lookup without reversing package dependencies.
type agentRouteLookup struct {
	agent *agentroutepkg.AgentRouteResolver
}

func (l agentRouteLookup) AgentRouteIDsForAgent(ctx context.Context, agentID string) ([]string, error) {
	if l.agent == nil {
		return nil, nil
	}
	configs, err := l.agent.ListConfigs(ctx, agentroutepkg.RouteListOptions{})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, cfg := range configs {
		if cfg.Kind != routecore.RouteKindAgent {
			continue
		}
		target, err := agentroutepkg.DecodeTargetAgentID(cfg.TargetPolicy)
		if err != nil {
			return nil, err
		}
		if target == agentID {
			ids = append(ids, cfg.ID)
		}
	}
	return ids, nil
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
