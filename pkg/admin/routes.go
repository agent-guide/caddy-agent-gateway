package admin

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	"github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	dispatcherpkg "github.com/agent-guide/agent-gateway/pkg/dispatcher"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/modelcatalog"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"gorm.io/gorm"
)

type VirtualKeyView struct {
	virtualkeypkg.VirtualKey
	Source   string `json:"source"`
	ReadOnly bool   `json:"read_only"`
}

type ProviderView struct {
	provider.ProviderConfig
	Source   string `json:"source"`
	ReadOnly bool   `json:"read_only"`
}

type ProviderTypeView struct {
	ProviderType string `json:"provider_type"`
	Enabled      bool   `json:"enabled"`
}

type LLMApiHandlerTypeView struct {
	LLMApiHandlerType string `json:"llm_api_handler_type"`
}

// Route defines an admin API route.
type Route struct {
	Method  string
	Path    string
	Handler http.HandlerFunc
}

// Routes returns all admin API routes.
func (h *Handler) Routes() []Route {
	return []Route{
		// Health — public
		{Method: http.MethodGet, Path: "/admin/health", Handler: h.handleHealth},

		// LLM provider types
		{Method: http.MethodGet, Path: "/admin/llm/provider_types", Handler: h.handleListProviderTypes},

		// LLM API handler types
		{Method: http.MethodGet, Path: "/admin/llm/api_handler_types", Handler: h.handleListLLMApiHandlerTypes},

		// LLM providers and models
		{Method: http.MethodGet, Path: "/admin/llm/providers", Handler: h.handleListProviders},
		{Method: http.MethodPost, Path: "/admin/llm/providers", Handler: h.handleCreateProvider},
		{Method: http.MethodGet, Path: "/admin/llm/providers/{id}", Handler: h.handleGetProvider},
		{Method: http.MethodPut, Path: "/admin/llm/providers/{id}", Handler: h.handleUpdateProvider},
		{Method: http.MethodPost, Path: "/admin/llm/providers/{id}/enable", Handler: h.handleEnableProvider},
		{Method: http.MethodPost, Path: "/admin/llm/providers/{id}/disable", Handler: h.handleDisableProvider},
		{Method: http.MethodDelete, Path: "/admin/llm/providers/{id}", Handler: h.handleDeleteProvider},
		{Method: http.MethodGet, Path: "/admin/llm/models/providers/{provider_id}/discovered", Handler: h.handleListDiscoveredModels},
		{Method: http.MethodPost, Path: "/admin/llm/models/providers/{provider_id}/refresh", Handler: h.handleRefreshProviderModels},
		{Method: http.MethodGet, Path: "/admin/llm/models/managed", Handler: h.handleListManagedModels},
		{Method: http.MethodPost, Path: "/admin/llm/models/managed", Handler: h.handleCreateManagedModel},
		{Method: http.MethodGet, Path: "/admin/llm/models/managed/{provider_id}/{upstream_model}", Handler: h.handleGetManagedModel},
		{Method: http.MethodPut, Path: "/admin/llm/models/managed/{provider_id}/{upstream_model}", Handler: h.handleUpdateManagedModel},
		{Method: http.MethodDelete, Path: "/admin/llm/models/managed/{provider_id}/{upstream_model}", Handler: h.handleDeleteManagedModel},
		{Method: http.MethodGet, Path: "/admin/llm/routes", Handler: h.handleListLLMRoutes},
		{Method: http.MethodPost, Path: "/admin/llm/routes", Handler: h.handleCreateLLMRoute},
		{Method: http.MethodGet, Path: "/admin/llm/routes/{id}", Handler: h.handleGetLLMRoute},
		{Method: http.MethodPut, Path: "/admin/llm/routes/{id}", Handler: h.handleUpdateLLMRoute},
		{Method: http.MethodPost, Path: "/admin/llm/routes/{id}/enable", Handler: h.handleEnableLLMRoute},
		{Method: http.MethodPost, Path: "/admin/llm/routes/{id}/disable", Handler: h.handleDisableLLMRoute},
		{Method: http.MethodDelete, Path: "/admin/llm/routes/{id}", Handler: h.handleDeleteLLMRoute},
		{Method: http.MethodGet, Path: "/admin/virtual_keys", Handler: h.handleListVirtualKeys},
		{Method: http.MethodPost, Path: "/admin/virtual_keys", Handler: h.handleCreateVirtualKey},
		{Method: http.MethodGet, Path: "/admin/virtual_keys/{id}", Handler: h.handleGetVirtualKey},
		{Method: http.MethodPut, Path: "/admin/virtual_keys/{id}", Handler: h.handleUpdateVirtualKey},
		{Method: http.MethodPost, Path: "/admin/virtual_keys/{id}/enable", Handler: h.handleEnableVirtualKey},
		{Method: http.MethodPost, Path: "/admin/virtual_keys/{id}/disable", Handler: h.handleDisableVirtualKey},
		{Method: http.MethodDelete, Path: "/admin/virtual_keys/{id}", Handler: h.handleDeleteVirtualKey},
		// Credentials (api_key and cliauth)
		{Method: http.MethodGet, Path: "/admin/credentials", Handler: h.handleListCredentials},
		{Method: http.MethodPost, Path: "/admin/credentials", Handler: h.handleCreateCredential},
		{Method: http.MethodGet, Path: "/admin/credentials/{credential_id}", Handler: h.handleGetCredential},
		{Method: http.MethodPut, Path: "/admin/credentials/{credential_id}", Handler: h.handleUpdateCredential},
		{Method: http.MethodDelete, Path: "/admin/credentials/{credential_id}", Handler: h.handleDeleteCredential},

		// CLI Auth Authenticators
		{Method: http.MethodGet, Path: "/admin/cliauth/authenticators", Handler: h.handleListCLIAuthAuthenticators},
		{Method: http.MethodGet, Path: "/admin/cliauth/authenticators/{authenticator_name}", Handler: h.handleGetCLIAuthAuthenticator},
		{Method: http.MethodPut, Path: "/admin/cliauth/authenticators/{authenticator_name}", Handler: h.handleUpdateCLIAuthAuthenticator},
		{Method: http.MethodPost, Path: "/admin/cliauth/authenticators/{authenticator_name}/login", Handler: h.handleStartCLIAuthAuthenticatorLogin},
		{Method: http.MethodGet, Path: "/admin/cliauth/refresher", Handler: h.handleGetCLIAuthRefresherStatus},
		{Method: http.MethodPost, Path: "/admin/cliauth/refresher/enable", Handler: h.handleEnableCLIAuthRefresher},
		{Method: http.MethodPost, Path: "/admin/cliauth/refresher/disable", Handler: h.handleDisableCLIAuthRefresher},
		{Method: http.MethodGet, Path: "/admin/cliauth/logins/{login_id}", Handler: h.handleGetCLIAuthLoginStatus},

		// MCP
		{Method: http.MethodGet, Path: "/admin/mcp/services", Handler: h.handlerListMCPServices},
		{Method: http.MethodPost, Path: "/admin/mcp/services", Handler: h.handlerAddMCPService},
		{Method: http.MethodGet, Path: "/admin/mcp/services/{id}", Handler: h.handlerGetMCPService},
		{Method: http.MethodPut, Path: "/admin/mcp/services/{id}", Handler: h.handlerUpdateMCPService},
		{Method: http.MethodDelete, Path: "/admin/mcp/services/{id}", Handler: h.handlerRemoveMCPService},
		{Method: http.MethodGet, Path: "/admin/mcp/services/{id}/capabilities", Handler: h.handleGetMCPServiceCapabilities},
		{Method: http.MethodGet, Path: "/admin/mcp/routes", Handler: h.handleListMCPRoutes},
		{Method: http.MethodPost, Path: "/admin/mcp/routes", Handler: h.handleCreateMCPRoute},
		{Method: http.MethodGet, Path: "/admin/mcp/routes/{id}", Handler: h.handleGetMCPRoute},
		{Method: http.MethodPut, Path: "/admin/mcp/routes/{id}", Handler: h.handleUpdateMCPRoute},
		{Method: http.MethodDelete, Path: "/admin/mcp/routes/{id}", Handler: h.handleDeleteMCPRoute},
		{Method: http.MethodGet, Path: "/admin/mcp/runtime", Handler: h.handleGetMCPDispatcherRuntime},
		{Method: http.MethodGet, Path: "/admin/mcp/runtime/inflight", Handler: h.handleListMCPDispatcherInFlight},
		{Method: http.MethodGet, Path: "/admin/mcp/runtime/progress", Handler: h.handleListMCPDispatcherProgress},
		{Method: http.MethodGet, Path: "/admin/mcp/runtime/history", Handler: h.handleListMCPDispatcherHistory},
		{Method: http.MethodGet, Path: "/admin/mcp/services/{id}/sessions", Handler: h.handleListMCPServiceSessions},
		{Method: http.MethodGet, Path: "/admin/mcp/services/{id}/tools", Handler: h.handleListMCPTools},
		{Method: http.MethodPost, Path: "/admin/mcp/services/{id}/tools/call", Handler: h.handleCallMCPTool},
		{Method: http.MethodGet, Path: "/admin/mcp/services/{id}/resources", Handler: h.handleListMCPResources},
		{Method: http.MethodGet, Path: "/admin/mcp/services/{id}/resource-templates", Handler: h.handleListMCPResourceTemplates},
		{Method: http.MethodPost, Path: "/admin/mcp/services/{id}/resources/read", Handler: h.handleReadMCPResource},
		{Method: http.MethodGet, Path: "/admin/mcp/services/{id}/prompts", Handler: h.handleListMCPPrompts},
		{Method: http.MethodPost, Path: "/admin/mcp/services/{id}/prompts/get", Handler: h.handleGetMCPPrompt},

		// ACP
		{Method: http.MethodGet, Path: "/admin/acp/services", Handler: h.handleListACPServices},
		{Method: http.MethodPost, Path: "/admin/acp/services", Handler: h.handleCreateACPService},
		{Method: http.MethodGet, Path: "/admin/acp/services/{id}", Handler: h.handleGetACPService},
		{Method: http.MethodPut, Path: "/admin/acp/services/{id}", Handler: h.handleUpdateACPService},
		{Method: http.MethodDelete, Path: "/admin/acp/services/{id}", Handler: h.handleDeleteACPService},
		{Method: http.MethodGet, Path: "/admin/acp/services/{id}/sessions", Handler: h.handleListACPSessions},
		{Method: http.MethodGet, Path: "/admin/acp/services/{id}/sessions/{session_id}/transcript", Handler: h.handleGetACPSessionTranscript},
		{Method: http.MethodGet, Path: "/admin/acp/routes", Handler: h.handleListACPRoutes},
		{Method: http.MethodPost, Path: "/admin/acp/routes", Handler: h.handleCreateACPRoute},
		{Method: http.MethodGet, Path: "/admin/acp/routes/{id}", Handler: h.handleGetACPRoute},
		{Method: http.MethodPut, Path: "/admin/acp/routes/{id}", Handler: h.handleUpdateACPRoute},
		{Method: http.MethodDelete, Path: "/admin/acp/routes/{id}", Handler: h.handleDeleteACPRoute},
		{Method: http.MethodGet, Path: "/admin/acp/runtime", Handler: h.handleGetACPRuntime},
		{Method: http.MethodGet, Path: "/admin/acp/runtime/inflight", Handler: h.handleListACPInFlight},
		{Method: http.MethodDelete, Path: "/admin/acp/runtime/threads/{service_id}/{thread_id}", Handler: h.handleCloseACPThread},
		{Method: http.MethodPost, Path: "/admin/acp/runtime/permissions/{request_id}", Handler: h.handleResolveACPPermission},

		// Builtin agent routes
		{Method: http.MethodGet, Path: "/admin/builtin/runtime", Handler: h.handleGetBuiltinRuntime},
		{Method: http.MethodGet, Path: "/admin/builtin/runtime/inflight", Handler: h.handleListBuiltinInFlight},
		{Method: http.MethodDelete, Path: "/admin/builtin/runtime/turns/{agent_id}/{session_id}", Handler: h.handleCancelBuiltinTurn},
		{Method: http.MethodGet, Path: "/admin/builtin/routes", Handler: h.handleListBuiltinRoutes},
		{Method: http.MethodPost, Path: "/admin/builtin/routes", Handler: h.handleCreateBuiltinRoute},
		{Method: http.MethodGet, Path: "/admin/builtin/routes/{id}", Handler: h.handleGetBuiltinRoute},
		{Method: http.MethodPut, Path: "/admin/builtin/routes/{id}", Handler: h.handleUpdateBuiltinRoute},
		{Method: http.MethodDelete, Path: "/admin/builtin/routes/{id}", Handler: h.handleDeleteBuiltinRoute},

		// Memory
		{Method: http.MethodGet, Path: "/admin/memory/config", Handler: h.handleGetMemoryConfig},
		{Method: http.MethodPut, Path: "/admin/memory/config", Handler: h.handleSetMemoryConfig},
		{Method: http.MethodGet, Path: "/admin/memory/search", Handler: h.handleSearchMemory},

		// Agents
		{Method: http.MethodGet, Path: "/admin/agents", Handler: h.handleListAgents},
		{Method: http.MethodPost, Path: "/admin/agents", Handler: h.handleCreateAgent},
		{Method: http.MethodGet, Path: "/admin/agents/{id}", Handler: h.handleGetAgent},
		{Method: http.MethodPut, Path: "/admin/agents/{id}", Handler: h.handleUpdateAgent},
		{Method: http.MethodDelete, Path: "/admin/agents/{id}", Handler: h.handleDeleteAgent},
		{Method: http.MethodGet, Path: "/admin/agents/{id}/workspace", Handler: h.handleGetAgentWorkspace},
		{Method: http.MethodGet, Path: "/admin/agents/{id}/activity", Handler: h.handleGetAgentActivity},
		{Method: http.MethodGet, Path: "/admin/agents/{id}/usage", Handler: h.handleGetAgentUsage},
		{Method: http.MethodGet, Path: "/admin/agents/{id}/interactions", Handler: h.handleGetAgentInteractions},
		{Method: http.MethodGet, Path: "/admin/agents/{id}/resources", Handler: h.handleGetAgentResources},
		{Method: http.MethodPut, Path: "/admin/agents/{id}/resources", Handler: h.handleUpdateAgentResources},
		{Method: http.MethodGet, Path: "/admin/agents/{id}/health", Handler: h.handleGetAgentHealth},

		// Metrics
		{Method: http.MethodGet, Path: "/admin/metrics", Handler: h.handleMetrics},
		{Method: http.MethodGet, Path: "/admin/metrics/prometheus", Handler: h.handlePrometheusMetrics},
		{Method: http.MethodGet, Path: "/admin/metrics/llm/events", Handler: h.handleListLLMMetricsEvents},
		{Method: http.MethodGet, Path: "/admin/metrics/llm/timeseries", Handler: h.handleLLMMetricsTimeseries},
		{Method: http.MethodGet, Path: "/admin/metrics/llm/breakdown", Handler: h.handleLLMMetricsBreakdown},
		{Method: http.MethodGet, Path: "/admin/metrics/mcp/events", Handler: h.handleListMCPMetricsEvents},
		{Method: http.MethodGet, Path: "/admin/metrics/mcp/timeseries", Handler: h.handleMCPMetricsTimeseries},
		{Method: http.MethodGet, Path: "/admin/metrics/mcp/breakdown", Handler: h.handleMCPMetricsBreakdown},
		{Method: http.MethodGet, Path: "/admin/metrics/mcp/tools/summary", Handler: h.handleMCPMetricsToolsSummary},
		{Method: http.MethodGet, Path: "/admin/metrics/acp/events", Handler: h.handleListACPMetricsEvents},
		{Method: http.MethodGet, Path: "/admin/metrics/acp/timeseries", Handler: h.handleACPMetricsTimeseries},
		{Method: http.MethodGet, Path: "/admin/metrics/acp/breakdown", Handler: h.handleACPMetricsBreakdown},
		{Method: http.MethodGet, Path: "/admin/metrics/acp/summary", Handler: h.handleACPMetricsSummary},
		{Method: http.MethodGet, Path: "/admin/metrics/interactions", Handler: h.handleListMetricInteractions},
		{Method: http.MethodGet, Path: "/admin/metrics/interactions/summary", Handler: h.handleMetricInteractionsSummary},
	}
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	_ = httpjson.Write(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleListProviders(w http.ResponseWriter, r *http.Request) {
	manager := h.providerManagerForRoutes()
	if manager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "provider manager is not configured")
		return
	}

	items, err := manager.ListConfigs(r.Context(), gateway.ProviderListOptions{
		ProviderType: r.URL.Query().Get("provider_type"),
	})
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	providers := make([]ProviderView, 0, len(items))
	for _, cfg := range items {
		providers = append(providers, providerViewFromConfig(manager, cfg))
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": providers})
}

func (h *Handler) handleListProviderTypes(w http.ResponseWriter, r *http.Request) {
	names := provider.ListProviderTypes()
	items := make([]ProviderTypeView, 0, len(names))
	for _, name := range names {
		enabled, ok := provider.IsProviderTypeEnabled(name)
		if !ok {
			continue
		}
		items = append(items, ProviderTypeView{
			ProviderType: name,
			Enabled:      enabled,
		})
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) handleListLLMApiHandlerTypes(w http.ResponseWriter, r *http.Request) {
	types := dispatcherpkg.ListLLMApiHandlerTypes()
	items := make([]LLMApiHandlerTypeView, 0, len(types))
	for _, handlerType := range types {
		items = append(items, LLMApiHandlerTypeView{
			LLMApiHandlerType: handlerType,
		})
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	manager := h.providerManagerForRoutes()
	if manager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "provider manager is not configured")
		return
	}

	var cfg provider.ProviderConfig
	if err := httpjson.Decode(r, &cfg); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	if cfg.Id == "" || cfg.ProviderType == "" {
		_ = httpjson.Error(w, http.StatusBadRequest, "id and provider_type are required")
		return
	}
	if err := manager.CreateConfig(r.Context(), cfg); err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	created, err := manager.GetConfig(r.Context(), cfg.Id)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusCreated, providerViewFromConfig(manager, created))
}

func (h *Handler) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	manager := h.providerManagerForRoutes()
	if manager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "provider manager is not configured")
		return
	}

	id := r.PathValue("id")
	cfg, err := manager.GetConfig(r.Context(), id)
	if err != nil {
		if errors.Is(err, gateway.ErrProviderNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "provider not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, providerViewFromConfig(manager, cfg))
}

func (h *Handler) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	manager := h.providerManagerForRoutes()
	if manager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "provider manager is not configured")
		return
	}

	var cfg provider.ProviderConfig
	if err := httpjson.Decode(r, &cfg); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	if cfg.ProviderType == "" {
		_ = httpjson.Error(w, http.StatusBadRequest, "provider_type is required")
		return
	}

	id := r.PathValue("id")
	if cfg.Id != "" && cfg.Id != id {
		_ = httpjson.Error(w, http.StatusBadRequest, "body id must match path id")
		return
	}
	cfg.Id = id
	if err := manager.UpdateConfig(r.Context(), id, cfg); err != nil {
		if errors.Is(err, gateway.ErrProviderNotConfigured) || errors.Is(err, gorm.ErrRecordNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "provider not found")
			return
		}
		if errors.Is(err, gateway.ErrStaticProviderReadOnly) {
			_ = httpjson.Error(w, http.StatusConflict, err.Error())
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	updatedCfg, err := manager.GetConfig(r.Context(), id)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, providerViewFromConfig(manager, updatedCfg))
}

func (h *Handler) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	manager := h.providerManagerForRoutes()
	if manager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "provider manager is not configured")
		return
	}

	id := r.PathValue("id")
	refs, err := h.providerRouteReferences(r, id)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(refs) > 0 {
		_ = httpjson.Error(w, http.StatusConflict, fmt.Sprintf("provider %q is still referenced by llm routes: %s", id, strings.Join(refs, ", ")))
		return
	}
	if err := manager.DeleteConfig(r.Context(), id); err != nil {
		if errors.Is(err, gateway.ErrStaticProviderReadOnly) {
			_ = httpjson.Error(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, gateway.ErrProviderNotConfigured) || errors.Is(err, gorm.ErrRecordNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "provider not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.deleteManagedModelsForProvider(r, id); err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]string{"status": "deleted", "id": id})
}

func (h *Handler) deleteManagedModelsForProvider(r *http.Request, providerID string) error {
	if h.modelCatalog == nil {
		return nil
	}
	items, err := h.modelCatalog.ListManagedModels(r.Context(), modelcatalog.ManagedModelFilter{
		ProviderID: providerID,
	})
	if err != nil {
		return fmt.Errorf("list managed models for provider %q: %w", providerID, err)
	}
	for _, item := range items {
		if err := h.modelCatalog.DeleteManagedModel(r.Context(), item.ProviderID, item.UpstreamModel); err != nil {
			if errors.Is(err, configstore.ErrNotFound) {
				continue
			}
			return fmt.Errorf("delete managed model %q/%q: %w", item.ProviderID, item.UpstreamModel, err)
		}
	}
	return nil
}

func (h *Handler) providerRouteReferences(r *http.Request, providerID string) ([]string, error) {
	resolver := h.llmRouteResolver()
	if resolver == nil {
		return nil, nil
	}
	routes, err := resolver.ListConfigs(r.Context(), routecore.RouteListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list llm routes: %w", err)
	}
	refs := make([]string, 0)
	for _, route := range routes {
		if route.Kind != routecore.RouteKindLLM {
			continue
		}
		cfg, err := llmroutepkg.NewLLMRouteConfigFromConfig(route)
		if err != nil {
			return nil, err
		}
		if cfg.TargetPolicy == nil {
			continue
		}
		for _, id := range cfg.TargetPolicy.ProviderIDs() {
			if id == providerID {
				refs = append(refs, cfg.ID)
				break
			}
		}
	}
	return refs, nil
}

func (h *Handler) handleEnableProvider(w http.ResponseWriter, r *http.Request) {
	h.handleSetProviderDisabled(w, r, false)
}

func (h *Handler) handleDisableProvider(w http.ResponseWriter, r *http.Request) {
	h.handleSetProviderDisabled(w, r, true)
}

func (h *Handler) handleSetProviderDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	manager := h.providerManagerForRoutes()
	if manager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "provider manager is not configured")
		return
	}

	id := r.PathValue("id")
	cfg, err := manager.GetConfig(r.Context(), id)
	if err != nil {
		if errors.Is(err, gateway.ErrProviderNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "provider not found")
			return
		}
		if errors.Is(err, gateway.ErrProviderDisabled) && !disabled {
			_ = httpjson.Error(w, http.StatusNotFound, "provider not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg.Disabled = disabled

	if err := manager.UpdateConfig(r.Context(), id, cfg); err != nil {
		if errors.Is(err, gateway.ErrProviderNotConfigured) || errors.Is(err, gorm.ErrRecordNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "provider not found")
			return
		}
		if errors.Is(err, gateway.ErrStaticProviderReadOnly) {
			_ = httpjson.Error(w, http.StatusConflict, err.Error())
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	updatedCfg, err := manager.GetConfig(r.Context(), id)
	if err != nil {
		if errors.Is(err, gateway.ErrProviderDisabled) && disabled {
			_ = httpjson.Write(w, http.StatusOK, providerViewFromConfig(manager, cfg))
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, providerViewFromConfig(manager, updatedCfg))
}

func (h *Handler) handleListVirtualKeys(w http.ResponseWriter, r *http.Request) {
	manager := h.virtualKeyManagerForRoutes()
	if manager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "virtual key manager not configured")
		return
	}

	tag := r.URL.Query().Get("tag")
	items, err := manager.List(r.Context(), virtualkeypkg.VirtualKeyListOptions{Tag: tag})
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := make([]VirtualKeyView, 0, len(items))
	for _, item := range items {
		views = append(views, virtualKeyViewFromKey(item))
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": views})
}

func (h *Handler) handleCreateVirtualKey(w http.ResponseWriter, r *http.Request) {
	manager := h.virtualKeyManagerForRoutes()
	if manager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "virtual key manager not configured")
		return
	}

	var key virtualkeypkg.VirtualKey
	if err := httpjson.Decode(r, &key); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}

	generatedKey, err := virtualkeypkg.GenerateKey()
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, "failed to generate virtual key")
		return
	}
	key.Key = generatedKey
	if strings.TrimSpace(key.ID) == "" {
		_ = httpjson.Error(w, http.StatusBadRequest, "virtual key id is required")
		return
	}
	if !key.CreatedAt.IsZero() || !key.UpdatedAt.IsZero() {
		_ = httpjson.Error(w, http.StatusBadRequest, "created_at and updated_at are managed by the server and must be omitted")
		return
	}
	if err := key.ValidateConfiguration(); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := manager.Create(r.Context(), key); err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	created, err := manager.GetByID(r.Context(), key.ID)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusCreated, created)
}

func (h *Handler) handleGetVirtualKey(w http.ResponseWriter, r *http.Request) {
	manager := h.virtualKeyManagerForRoutes()
	if manager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "virtual key manager not configured")
		return
	}

	item, err := manager.GetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, virtualkeypkg.ErrVirtualKeyNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "virtual key not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, virtualKeyViewFromKey(item))
}

func (h *Handler) handleUpdateVirtualKey(w http.ResponseWriter, r *http.Request) {
	manager := h.virtualKeyManagerForRoutes()
	if manager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "virtual key manager not configured")
		return
	}

	var key virtualkeypkg.VirtualKey
	if err := httpjson.Decode(r, &key); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	pathID := r.PathValue("id")
	if key.ID == "" {
		key.ID = pathID
	}
	if key.ID != pathID {
		_ = httpjson.Error(w, http.StatusBadRequest, "virtual key id in body must match path")
		return
	}
	if !key.CreatedAt.IsZero() || !key.UpdatedAt.IsZero() {
		_ = httpjson.Error(w, http.StatusBadRequest, "created_at and updated_at are managed by the server and must be omitted")
		return
	}
	if key.Key != "" {
		_ = httpjson.Error(w, http.StatusBadRequest, "virtual key key is generated and must be omitted")
		return
	}
	if err := key.ValidateConfiguration(); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	if _, err := manager.GetByID(r.Context(), pathID); err != nil {
		if errors.Is(err, virtualkeypkg.ErrVirtualKeyNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "virtual key not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := manager.Update(r.Context(), key.ID, key); err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	updated, err := manager.GetByID(r.Context(), key.ID)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, updated)
}

func (h *Handler) handleDeleteVirtualKey(w http.ResponseWriter, r *http.Request) {
	manager := h.virtualKeyManagerForRoutes()
	if manager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "virtual key manager not configured")
		return
	}

	id := r.PathValue("id")
	if err := manager.Delete(r.Context(), id); err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]string{"status": "deleted", "id": id})
}

func (h *Handler) handleEnableVirtualKey(w http.ResponseWriter, r *http.Request) {
	h.handleSetVirtualKeyDisabled(w, r, false)
}

func (h *Handler) handleDisableVirtualKey(w http.ResponseWriter, r *http.Request) {
	h.handleSetVirtualKeyDisabled(w, r, true)
}

func (h *Handler) handleSetVirtualKeyDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	manager := h.virtualKeyManagerForRoutes()
	if manager == nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "virtual key manager not configured")
		return
	}

	keyID := r.PathValue("id")
	key, err := manager.GetByID(r.Context(), keyID)
	if err != nil {
		if errors.Is(err, virtualkeypkg.ErrVirtualKeyNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "virtual key not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	key.Disabled = disabled

	if err := manager.Update(r.Context(), keyID, key); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "virtual key not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}

	updated, err := manager.GetByID(r.Context(), keyID)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, virtualKeyViewFromKey(updated))
}

func (h *Handler) handleGetMemoryConfig(w http.ResponseWriter, r *http.Request) {
	_ = httpjson.Error(w, http.StatusNotImplemented, "not implemented")
}
func (h *Handler) handleSetMemoryConfig(w http.ResponseWriter, r *http.Request) {
	_ = httpjson.Error(w, http.StatusNotImplemented, "not implemented")
}
func (h *Handler) handleSearchMemory(w http.ResponseWriter, r *http.Request) {
	_ = httpjson.Error(w, http.StatusNotImplemented, "not implemented")
}

type metricsResponse struct {
	usage.Summary
	Pipeline pipelineStats `json:"pipeline"`
}

type pipelineStats struct {
	DroppedEvents uint64 `json:"dropped_events"`
	WriteFailures uint64 `json:"write_failures"`
}

func (h *Handler) pipelineStats() pipelineStats {
	if h.usageStats == nil {
		return pipelineStats{}
	}
	return pipelineStats{
		DroppedEvents: h.usageStats.DroppedEvents(),
		WriteFailures: h.usageStats.WriteFailures(),
	}
}

func (h *Handler) handlePrometheusMetrics(w http.ResponseWriter, r *http.Request) {
	var snap usage.PrometheusSnapshot
	if h.usagePrometheus != nil {
		snap = h.usagePrometheus.PrometheusSnapshot()
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(usage.RenderPrometheus(snap, h.usageStats)))
}

func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	resp := metricsResponse{Pipeline: h.pipelineStats()}
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, resp)
		return
	}
	summary, err := h.usageQuery.Summary()
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Summary = summary
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) handleListLLMMetricsEvents(w http.ResponseWriter, r *http.Request) {
	h.handleListMetricsEvents(w, r, "llm", []string{
		"route_id", "provider_id", "virtual_key_id", "logical_model", "upstream_model", "llm_api", "api_operation",
		"request_tool_name", "has_tool_use",
	})
}

func (h *Handler) handleLLMMetricsTimeseries(w http.ResponseWriter, r *http.Request) {
	opts := metricTimeseriesOptions(r, []string{"route_id", "provider_id", "virtual_key_id", "upstream_model", "llm_api"})
	attribution, ok := h.agentAttributionFromRequest(w, r)
	if !ok {
		return
	}
	opts.Attribution = attribution
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, usage.SeriesResponse{Bucket: opts.Bucket, GroupBy: opts.GroupBy})
		return
	}
	resp, err := h.usageQuery.LLMTimeseries(opts)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if opts.GroupBy == "" {
		opts.GroupBy = "route_id"
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) handleLLMMetricsBreakdown(w http.ResponseWriter, r *http.Request) {
	opts, err := metricBreakdownOptions(r, []string{"route_id", "provider_id", "virtual_key_id", "upstream_model", "llm_api"})
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	attribution, ok := h.agentAttributionFromRequest(w, r)
	if !ok {
		return
	}
	opts.Attribution = attribution
	if opts.GroupBy == "" {
		opts.GroupBy = "route_id"
	}
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, usage.BreakdownResponse{GroupBy: opts.GroupBy, Limit: opts.Limit})
		return
	}
	resp, err := h.usageQuery.LLMBreakdown(opts)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if opts.GroupBy == "" {
		opts.GroupBy = "route_kind"
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) handleListMCPMetricsEvents(w http.ResponseWriter, r *http.Request) {
	h.handleListMetricsEvents(w, r, "mcp", []string{
		"route_id", "service_id", "virtual_key_id", "method", "tool_name", "resource_uri", "prompt_name", "result_status",
		"completion_ref_type", "completion_argument",
	})
}

func (h *Handler) handleMCPMetricsTimeseries(w http.ResponseWriter, r *http.Request) {
	opts := metricTimeseriesOptions(r, []string{"route_id", "service_id", "virtual_key_id", "method", "tool_name", "result_status"})
	attribution, ok := h.agentAttributionFromRequest(w, r)
	if !ok {
		return
	}
	opts.Attribution = attribution
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, usage.SeriesResponse{Bucket: opts.Bucket, GroupBy: opts.GroupBy})
		return
	}
	resp, err := h.usageQuery.MCPTimeseries(opts)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) handleMCPMetricsBreakdown(w http.ResponseWriter, r *http.Request) {
	opts, err := metricBreakdownOptions(r, []string{"route_id", "service_id", "virtual_key_id", "method", "tool_name", "result_status"})
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	attribution, ok := h.agentAttributionFromRequest(w, r)
	if !ok {
		return
	}
	opts.Attribution = attribution
	if opts.GroupBy == "" {
		opts.GroupBy = "tool_name"
	}
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, usage.BreakdownResponse{GroupBy: opts.GroupBy, Limit: opts.Limit})
		return
	}
	resp, err := h.usageQuery.MCPBreakdown(opts)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) handleMCPMetricsToolsSummary(w http.ResponseWriter, r *http.Request) {
	opts, err := metricSummaryOptions(r, []string{"route_id", "service_id"})
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, usage.BreakdownResponse{GroupBy: "tool_name", Limit: opts.Limit})
		return
	}
	resp, err := h.usageQuery.MCPToolsSummary(opts)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) handleListACPMetricsEvents(w http.ResponseWriter, r *http.Request) {
	h.handleListMetricsEvents(w, r, "acp", []string{
		"route_id", "route_protocol", "service_id", "virtual_key_id", "agent_type", "operation", "thread_id", "session_id",
	})
}

func (h *Handler) handleACPMetricsTimeseries(w http.ResponseWriter, r *http.Request) {
	opts := metricTimeseriesOptions(r, []string{"route_id", "route_protocol", "service_id", "virtual_key_id", "agent_type", "operation"})
	attribution, ok := h.agentAttributionFromRequest(w, r)
	if !ok {
		return
	}
	opts.Attribution = attribution
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, usage.SeriesResponse{Bucket: opts.Bucket, GroupBy: opts.GroupBy})
		return
	}
	resp, err := h.usageQuery.ACPTimeseries(opts)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) handleACPMetricsBreakdown(w http.ResponseWriter, r *http.Request) {
	opts, err := metricBreakdownOptions(r, []string{"route_id", "route_protocol", "service_id", "virtual_key_id", "agent_type", "operation"})
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	attribution, ok := h.agentAttributionFromRequest(w, r)
	if !ok {
		return
	}
	opts.Attribution = attribution
	if opts.GroupBy == "" {
		opts.GroupBy = "operation"
	}
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, usage.BreakdownResponse{GroupBy: opts.GroupBy, Limit: opts.Limit})
		return
	}
	resp, err := h.usageQuery.ACPBreakdown(opts)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) handleACPMetricsSummary(w http.ResponseWriter, r *http.Request) {
	opts, err := metricBreakdownOptions(r, []string{"route_id", "route_protocol", "service_id", "agent_type", "operation"})
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, usage.BreakdownResponse{GroupBy: opts.GroupBy, Limit: opts.Limit})
		return
	}
	resp, err := h.usageQuery.ACPSummary(opts)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) handleListMetricInteractions(w http.ResponseWriter, r *http.Request) {
	opts, err := metricEventListOptions(r, []string{
		"route_kind", "route_protocol", "route_id", "virtual_key_id", "trace_id", "parent_span_id", "agent_depth",
		"service_id", "session_id",
	})
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	// agent_id resolves to the agent's full attribution (durable tag OR owned
	// routes/ACP service), matching the per-agent interactions/usage reads — not a
	// literal tag filter, so untagged-but-mappable spans still surface.
	attribution, ok := h.agentAttributionFromRequest(w, r)
	if !ok {
		return
	}
	opts.Attribution = attribution
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, usage.EventListResponse{Limit: opts.Limit})
		return
	}
	resp, err := h.usageQuery.ListInteractions(opts)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) handleMetricInteractionsSummary(w http.ResponseWriter, r *http.Request) {
	opts, err := metricBreakdownOptions(r, []string{"route_kind", "route_protocol", "route_id", "virtual_key_id", "service_id", "session_id"})
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	attribution, ok := h.agentAttributionFromRequest(w, r)
	if !ok {
		return
	}
	opts.Attribution = attribution
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, usage.BreakdownResponse{GroupBy: opts.GroupBy, Limit: opts.Limit})
		return
	}
	resp, err := h.usageQuery.InteractionsSummary(opts)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) handleListMetricsEvents(w http.ResponseWriter, r *http.Request, kind string, filterKeys []string) {
	opts, err := metricEventListOptions(r, filterKeys)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	attribution, ok := h.agentAttributionFromRequest(w, r)
	if !ok {
		return
	}
	opts.Attribution = attribution
	if h.usageQuery == nil {
		_ = httpjson.Write(w, http.StatusOK, usage.EventListResponse{Limit: opts.Limit})
		return
	}
	resp, err := h.usageQuery.ListEvents(kind, opts)
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func metricEventListOptions(r *http.Request, filterKeys []string) (usage.EventListOptions, error) {
	q := r.URL.Query()
	limit := 100
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return usage.EventListOptions{}, fmt.Errorf("limit must be an integer")
		}
		limit = parsed
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	var success *bool
	if raw := strings.TrimSpace(q.Get("success")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return usage.EventListOptions{}, fmt.Errorf("success must be a boolean")
		}
		success = &parsed
	}
	filters := map[string]string{}
	for _, key := range filterKeys {
		if value := strings.TrimSpace(q.Get(key)); value != "" {
			filters[key] = value
		}
	}
	return usage.EventListOptions{
		From:    strings.TrimSpace(q.Get("from")),
		To:      strings.TrimSpace(q.Get("to")),
		Limit:   limit,
		Filters: filters,
		Success: success,
	}, nil
}

func metricTimeseriesOptions(r *http.Request, filterKeys []string) usage.TimeseriesOptions {
	q := r.URL.Query()
	filters := map[string]string{}
	for _, key := range filterKeys {
		if value := strings.TrimSpace(q.Get(key)); value != "" {
			filters[key] = value
		}
	}
	return usage.TimeseriesOptions{
		From:    strings.TrimSpace(q.Get("from")),
		To:      strings.TrimSpace(q.Get("to")),
		Bucket:  strings.TrimSpace(q.Get("bucket")),
		GroupBy: strings.TrimSpace(q.Get("group_by")),
		Filters: filters,
	}
}

func metricBreakdownOptions(r *http.Request, filterKeys []string) (usage.BreakdownOptions, error) {
	q := r.URL.Query()
	limit, err := metricLimit(q.Get("limit"))
	if err != nil {
		return usage.BreakdownOptions{}, err
	}
	filters := map[string]string{}
	for _, key := range filterKeys {
		if value := strings.TrimSpace(q.Get(key)); value != "" {
			filters[key] = value
		}
	}
	return usage.BreakdownOptions{
		From:    strings.TrimSpace(q.Get("from")),
		To:      strings.TrimSpace(q.Get("to")),
		GroupBy: strings.TrimSpace(q.Get("group_by")),
		OrderBy: strings.TrimSpace(q.Get("order_by")),
		Limit:   limit,
		Filters: filters,
	}, nil
}

func metricSummaryOptions(r *http.Request, filterKeys []string) (usage.SummaryOptions, error) {
	q := r.URL.Query()
	limit, err := metricLimit(q.Get("limit"))
	if err != nil {
		return usage.SummaryOptions{}, err
	}
	filters := map[string]string{}
	for _, key := range filterKeys {
		if value := strings.TrimSpace(q.Get(key)); value != "" {
			filters[key] = value
		}
	}
	return usage.SummaryOptions{
		From:    strings.TrimSpace(q.Get("from")),
		To:      strings.TrimSpace(q.Get("to")),
		Limit:   limit,
		Filters: filters,
	}, nil
}

func metricLimit(raw string) (int, error) {
	limit := 100
	if raw = strings.TrimSpace(raw); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return 0, fmt.Errorf("limit must be an integer")
		}
		limit = parsed
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	return limit, nil
}

func (h *Handler) providerStore() configstore.ConfigStore {
	if h.configStoreBackend == nil {
		return nil
	}
	providerConfigStore, err := h.configStoreBackend.Get(schema.StoreProviders)
	if err != nil {
		return nil
	}
	return providerConfigStore
}

func (h *Handler) providerManagerForRoutes() *gateway.ProviderManager {
	if h.providerManager != nil {
		return h.providerManager
	}

	store := h.providerStore()
	if store == nil {
		return nil
	}
	return gateway.NewProviderManager(store)
}

func (h *Handler) routeStore() configstore.ConfigStore {
	if h.configStoreBackend == nil {
		return nil
	}
	store, err := h.configStoreBackend.Get(schema.StoreRoutes)
	if err != nil {
		return nil
	}
	return store
}

func (h *Handler) routeConfigManagerForRoutes() *routecore.AgentRouteConfigManager {
	if h.routeConfigManager != nil {
		return h.routeConfigManager
	}

	store := h.routeStore()
	if store == nil {
		return nil
	}
	return routecore.NewAgentRouteConfigManager(store)
}

func filterRouteConfigsByKind(items []routecore.AgentRouteConfig, kind routecore.RouteKind) []routecore.AgentRouteConfig {
	filtered := make([]routecore.AgentRouteConfig, 0, len(items))
	for _, item := range items {
		if item.Kind == kind {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func (h *Handler) virtualKeyStore() configstore.ConfigStore {
	if h.configStoreBackend == nil {
		return nil
	}
	store, err := h.configStoreBackend.Get(schema.StoreVirtualKeys)
	if err != nil {
		return nil
	}
	return store
}

func (h *Handler) virtualKeyManagerForRoutes() *virtualkeypkg.VirtualKeyManager {
	if h.virtualKeyManager != nil {
		return h.virtualKeyManager
	}

	store := h.virtualKeyStore()
	if store == nil {
		return nil
	}
	return virtualkeypkg.NewVirtualKeyManager(store)
}

func virtualKeyViewFromKey(key virtualkeypkg.VirtualKey) VirtualKeyView {
	return VirtualKeyView{
		VirtualKey: key,
		Source:     "store",
		ReadOnly:   false,
	}
}

func providerViewFromConfig(manager *gateway.ProviderManager, cfg provider.ProviderConfig) ProviderView {
	view := ProviderView{
		ProviderConfig: cfg,
		Source:         "store",
		ReadOnly:       false,
	}
	if manager != nil && manager.IsStatic(cfg.Id) {
		view.Source = "caddyfile"
		view.ReadOnly = true
	}
	return view
}
