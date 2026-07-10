package admin

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/agent-guide/agent-gateway/internal/httpcapture"
	"github.com/agent-guide/agent-gateway/internal/httplog"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/cliauth"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	acproute "github.com/agent-guide/agent-gateway/pkg/gateway/acproute"
	llmroute "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	mcproute "github.com/agent-guide/agent-gateway/pkg/gateway/mcproute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	"github.com/agent-guide/agent-gateway/pkg/llm/credentialmgr"
	mcpruntime "github.com/agent-guide/agent-gateway/pkg/mcp/runtime"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
	"go.uber.org/zap"
)

// Handler handles Admin API requests under /admin/.
type Handler struct {
	cliauthManager          *cliauth.Manager
	cliauthRefresher        *cliauth.AutoRefresher
	credentialManager       *credentialmgr.Manager
	configStoreBackend      configstore.ConfigStoreBackend
	routeConfigManager      *routecore.AgentRouteConfigManager
	sharedLLMRouteResolver  *llmroute.LLMRouteResolver
	sharedMCPRouteResolver  *mcproute.MCPRouteResolver
	sharedACPRouteResolver  *acproute.ACPRouteResolver
	sharedMCPServiceManager *mcpservice.Manager
	sharedACPServiceManager *acpservice.Manager
	agentManager            *agentpkg.Manager
	virtualKeyManager       *virtualkeypkg.VirtualKeyManager
	providerManager         *gateway.ProviderManager
	modelCatalog            modelcatalog.Service
	mcpRuntimeRegistry      *mcpruntime.Registry
	acpRuntimeManager       *acpruntime.Manager
	usageObserver           usage.InteractionObserver
	usageQuery              usage.QueryService
	usageStats              usage.RuntimeStats
	usagePrometheus         usage.PrometheusProvider
	mux                     *http.ServeMux
	logger                  *zap.Logger
	cliAuthMu               sync.RWMutex
	cliAuthSessions         map[string]cliAuthStatus // login_id -> cliAuthStatus
	cliAuthActive           map[string]string        // cliname -> login_id
}

// NewHandler constructs an admin Handler.
// logger may be nil (a no-op logger is used in that case).
func NewHandler(agentGateway *gateway.AgentGateway, logger *zap.Logger) *Handler {
	if logger == nil {
		logger = zap.NewNop()
	}

	var cliauthMgr *cliauth.Manager
	var cliauthRefresher *cliauth.AutoRefresher
	var credentialMgr *credentialmgr.Manager
	var configStoreBackend configstore.ConfigStoreBackend
	var routeConfigManager *routecore.AgentRouteConfigManager
	var sharedLLMRouteResolver *llmroute.LLMRouteResolver
	var sharedMCPRouteResolver *mcproute.MCPRouteResolver
	var sharedACPRouteResolver *acproute.ACPRouteResolver
	var sharedMCPServiceManager *mcpservice.Manager
	var sharedACPServiceManager *acpservice.Manager
	var agentManager *agentpkg.Manager
	var virtualKeyManager *virtualkeypkg.VirtualKeyManager
	var providerManager *gateway.ProviderManager
	var modelCatalogSvc modelcatalog.Service
	var mcpRuntimeRegistry *mcpruntime.Registry
	var acpRuntimeManager *acpruntime.Manager
	var usageObserver usage.InteractionObserver
	var usageQuery usage.QueryService
	var usageStats usage.RuntimeStats
	var usagePrometheus usage.PrometheusProvider
	if agentGateway != nil {
		cliauthMgr = agentGateway.CLIAuthManager()
		cliauthRefresher = agentGateway.CLIAuthRefresher()
		credentialMgr = agentGateway.CredentialManager()
		configStoreBackend = agentGateway.ConfigStoreBackend()
		routeConfigManager = agentGateway.AgentRouteConfigManager()
		sharedLLMRouteResolver = agentGateway.LLMRouteResolver()
		sharedMCPRouteResolver = agentGateway.MCPRouteResolver()
		sharedACPRouteResolver = agentGateway.ACPRouteResolver()
		sharedMCPServiceManager = agentGateway.MCPServiceManager()
		sharedACPServiceManager = agentGateway.ACPServiceManager()
		agentManager = agentGateway.AgentManager()
		virtualKeyManager = agentGateway.VirtualKeyManager()
		providerManager = agentGateway.ProviderManager()
		modelCatalogSvc = agentGateway.ModelCatalog()
		mcpRuntimeRegistry = agentGateway.MCPRuntimeRegistry()
		acpRuntimeManager = agentGateway.ACPRuntimeManager()
		usageObserver = agentGateway.UsageObserver()
		usageQuery = agentGateway.UsageQuery()
		usageStats = agentGateway.UsageStats()
		usagePrometheus = agentGateway.UsagePrometheus()
	}

	h := &Handler{
		cliauthManager:          cliauthMgr,
		cliauthRefresher:        cliauthRefresher,
		credentialManager:       credentialMgr,
		configStoreBackend:      configStoreBackend,
		routeConfigManager:      routeConfigManager,
		sharedLLMRouteResolver:  sharedLLMRouteResolver,
		sharedMCPRouteResolver:  sharedMCPRouteResolver,
		sharedACPRouteResolver:  sharedACPRouteResolver,
		sharedMCPServiceManager: sharedMCPServiceManager,
		sharedACPServiceManager: sharedACPServiceManager,
		agentManager:            agentManager,
		virtualKeyManager:       virtualKeyManager,
		providerManager:         providerManager,
		modelCatalog:            modelCatalogSvc,
		mcpRuntimeRegistry:      mcpRuntimeRegistry,
		acpRuntimeManager:       acpRuntimeManager,
		usageObserver:           usageObserver,
		usageQuery:              usageQuery,
		usageStats:              usageStats,
		usagePrometheus:         usagePrometheus,
		logger:                  logger,
		cliAuthSessions:         map[string]cliAuthStatus{},
		cliAuthActive:           map[string]string{},
	}
	h.mux = http.NewServeMux()
	// The Admin API does not authenticate requests itself; protect the mount
	// with the HTTP deployment boundary (Caddy basic_auth, mTLS, a reverse
	// proxy authenticator, or the standalone daemon's Basic Auth wrapper).
	for _, route := range h.Routes() {
		h.mux.HandleFunc(route.Method+" "+route.Path, route.Handler)
	}
	return h
}

// ServeHTTP dispatches admin API requests, including CORS preflight handling.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rr := httpcapture.NewResponseRecorder(w)
	defer func() {
		if recovered := recover(); recovered != nil {
			httplog.Error(h.logger, "admin request panicked", r, http.StatusInternalServerError, nil, zap.Any("panic", recovered))
			http.Error(rr, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		httplog.ResponseError(h.logger, "admin request failed", r, rr)
	}()

	if origin := r.Header.Get("Origin"); origin != "" {
		rr.Header().Set("Access-Control-Allow-Origin", origin)
		rr.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		rr.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		rr.Header().Set("Access-Control-Max-Age", "86400")
	}
	if r.Method == "OPTIONS" {
		rr.WriteHeader(http.StatusNoContent)
		return
	}
	h.mux.ServeHTTP(rr, r)
}

func (h *Handler) mcpServiceManager() (*mcpservice.Manager, error) {
	if h.sharedMCPServiceManager == nil {
		return nil, fmt.Errorf("mcp service manager is not configured")
	}
	return h.sharedMCPServiceManager, nil
}

func (h *Handler) mcpRouteResolver() (*mcproute.MCPRouteResolver, error) {
	if h.sharedMCPRouteResolver != nil {
		return h.sharedMCPRouteResolver, nil
	}
	manager := h.routeConfigManagerForRoutes()
	if manager == nil {
		return nil, configstore.ErrUnknownStoreName
	}
	return mcproute.NewMCPRouteResolver(manager), nil
}

func (h *Handler) acpServiceManager() (*acpservice.Manager, error) {
	if h.sharedACPServiceManager == nil {
		return nil, fmt.Errorf("acp service manager is not configured")
	}
	return h.sharedACPServiceManager, nil
}

func (h *Handler) acpRouteResolver() (*acproute.ACPRouteResolver, error) {
	if h.sharedACPRouteResolver != nil {
		return h.sharedACPRouteResolver, nil
	}
	manager := h.routeConfigManagerForRoutes()
	if manager == nil {
		return nil, configstore.ErrUnknownStoreName
	}
	return acproute.NewACPRouteResolver(manager), nil
}
