package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	acproute "github.com/agent-guide/agent-gateway/pkg/gateway/acproute"
)

type ACPServiceView struct {
	acpservice.ServiceConfig
	Source   string `json:"source"`
	ReadOnly bool   `json:"read_only"`
}

type ACPRouteView struct {
	acproute.ACPRouteConfig
	Source   string `json:"source"`
	ReadOnly bool   `json:"read_only"`
}

// MarshalJSON merges the view fields into the embedded config JSON. Without
// this the embedded ACPRouteConfig.MarshalJSON is promoted and silently drops
// source and read_only from admin responses.
func (v ACPRouteView) MarshalJSON() ([]byte, error) {
	return marshalRouteView(v.ACPRouteConfig, v.Source, v.ReadOnly)
}

func (v *ACPRouteView) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &v.ACPRouteConfig); err != nil {
		return err
	}
	return unmarshalRouteViewExtras(data, &v.Source, &v.ReadOnly)
}

func (h *Handler) handleListACPServices(w http.ResponseWriter, r *http.Request) {
	manager, err := h.acpServiceManager()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	items, err := manager.List(r.Context())
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := make([]ACPServiceView, 0, len(items))
	for _, item := range items {
		views = append(views, ACPServiceView{ServiceConfig: item, Source: "config_store"})
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": views})
}

func (h *Handler) handleCreateACPService(w http.ResponseWriter, r *http.Request) {
	manager, err := h.acpServiceManager()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	var cfg acpservice.ServiceConfig
	if err := httpjson.Decode(r, &cfg); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	cfg.Normalize()
	if err := manager.Create(r.Context(), cfg); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := manager.Get(r.Context(), cfg.ID)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusCreated, ACPServiceView{ServiceConfig: created, Source: "config_store"})
}

func (h *Handler) handleGetACPService(w http.ResponseWriter, r *http.Request) {
	manager, err := h.acpServiceManager()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	cfg, err := manager.Get(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, acpservice.ErrServiceNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "acp service not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, ACPServiceView{ServiceConfig: cfg, Source: "config_store"})
}

func (h *Handler) handleUpdateACPService(w http.ResponseWriter, r *http.Request) {
	manager, err := h.acpServiceManager()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	var cfg acpservice.ServiceConfig
	if err := httpjson.Decode(r, &cfg); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	if err := manager.Update(r.Context(), id, cfg); err != nil {
		if errors.Is(err, acpservice.ErrServiceNotConfigured) || errors.Is(err, configstore.ErrNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "acp service not found")
			return
		}
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.acpRuntimeManager != nil {
		h.acpRuntimeManager.CloseService(id)
	}
	updated, err := manager.Get(r.Context(), id)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, ACPServiceView{ServiceConfig: updated, Source: "config_store"})
}

func (h *Handler) handleDeleteACPService(w http.ResponseWriter, r *http.Request) {
	manager, err := h.acpServiceManager()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if err := manager.Delete(r.Context(), strings.TrimSpace(r.PathValue("id"))); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "acp service not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if h.acpRuntimeManager != nil {
		h.acpRuntimeManager.CloseService(strings.TrimSpace(r.PathValue("id")))
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) handleListACPSessions(w http.ResponseWriter, r *http.Request) {
	serviceID := strings.TrimSpace(r.PathValue("id"))
	span := h.beginACPAdminAudit(r, serviceID, "sessions", "")
	defer finishAdminAudit(span, http.StatusOK, "")
	if h.acpRuntimeManager == nil {
		finishAdminAudit(span, http.StatusServiceUnavailable, "service_unavailable")
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "acp runtime manager is not configured")
		return
	}
	result, err := h.acpRuntimeManager.ListSessions(r.Context(), serviceID, acpruntime.ListSessionsRequest{
		CWD:    strings.TrimSpace(r.URL.Query().Get("cwd")),
		Cursor: strings.TrimSpace(r.URL.Query().Get("cursor")),
	})
	if err != nil {
		status := acpRequestErrorStatus(err)
		finishAdminAudit(span, status, "upstream_error")
		_ = httpjson.Error(w, status, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, result)
}

func (h *Handler) handleGetACPSessionTranscript(w http.ResponseWriter, r *http.Request) {
	serviceID := strings.TrimSpace(r.PathValue("id"))
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	span := h.beginACPAdminAudit(r, serviceID, "transcript", sessionID)
	defer finishAdminAudit(span, http.StatusOK, "")
	if h.acpRuntimeManager == nil {
		finishAdminAudit(span, http.StatusServiceUnavailable, "service_unavailable")
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "acp runtime manager is not configured")
		return
	}
	result, err := h.acpRuntimeManager.LoadTranscript(r.Context(), serviceID, acpruntime.TranscriptRequest{
		SessionID: sessionID,
		CWD:       strings.TrimSpace(r.URL.Query().Get("cwd")),
	})
	if err != nil {
		status := acpRequestErrorStatus(err)
		finishAdminAudit(span, status, "upstream_error")
		_ = httpjson.Error(w, status, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, result)
}

// acpRequestErrorStatus maps a session/transcript error to an HTTP status: 404
// when the service is not configured, 400 for a client-correctable request
// problem, and 502 for an upstream agent/transport failure.
func acpRequestErrorStatus(err error) int {
	switch {
	case errors.Is(err, acpservice.ErrServiceNotConfigured) || errors.Is(err, configstore.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, acpruntime.ErrInvalidRequest):
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

func (h *Handler) handleListACPRoutes(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.acpRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	items, err := resolver.ListConfigs(r.Context(), acproute.RouteListOptions{})
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	items = filterRouteConfigsByKind(items, acproute.RouteKindACP)
	views := make([]ACPRouteView, 0, len(items))
	for _, item := range items {
		views = append(views, acpRouteViewFromConfig(resolver, item))
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": views})
}

func (h *Handler) handleCreateACPRoute(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.acpRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	var route acproute.ACPRouteConfig
	if err := httpjson.Decode(r, &route); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	if !route.CreatedAt.IsZero() || !route.UpdatedAt.IsZero() {
		_ = httpjson.Error(w, http.StatusBadRequest, "created_at and updated_at are managed by the server and must be omitted")
		return
	}
	route.Normalize()
	if route.ServiceID == "" {
		_ = httpjson.Error(w, http.StatusBadRequest, "service_id is required")
		return
	}
	cfg, err := route.ToConfig()
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := resolver.CreateConfig(r.Context(), cfg, ""); err != nil {
		if errors.Is(err, acproute.ErrInvalidRouteID) {
			_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	created, err := resolver.GetConfig(r.Context(), route.ID)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusCreated, acpRouteViewFromConfig(resolver, created))
}

func (h *Handler) handleGetACPRoute(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.acpRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	item, err := resolver.GetConfig(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, acproute.ErrRouteNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "acp route not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if item.Kind != acproute.RouteKindACP {
		_ = httpjson.Error(w, http.StatusNotFound, "acp route not found")
		return
	}
	_ = httpjson.Write(w, http.StatusOK, acpRouteViewFromConfig(resolver, item))
}

func (h *Handler) handleUpdateACPRoute(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.acpRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := r.PathValue("id")
	current, err := resolver.GetConfig(r.Context(), id)
	if err != nil {
		if errors.Is(err, acproute.ErrRouteNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "acp route not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if current.Kind != acproute.RouteKindACP {
		_ = httpjson.Error(w, http.StatusNotFound, "acp route not found")
		return
	}
	var route acproute.ACPRouteConfig
	if err := httpjson.Decode(r, &route); err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	if route.ID == "" {
		route.ID = id
	}
	if route.ID != id {
		_ = httpjson.Error(w, http.StatusBadRequest, "route id in body must match path")
		return
	}
	if !route.CreatedAt.IsZero() || !route.UpdatedAt.IsZero() {
		_ = httpjson.Error(w, http.StatusBadRequest, "created_at and updated_at are managed by the server and must be omitted")
		return
	}
	route.Normalize()
	if route.ServiceID == "" {
		_ = httpjson.Error(w, http.StatusBadRequest, "service_id is required")
		return
	}
	cfg, err := route.ToConfig()
	if err != nil {
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := resolver.UpdateConfig(r.Context(), id, cfg); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "acp route not found")
			return
		}
		if errors.Is(err, acproute.ErrInvalidRouteID) {
			_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	item, err := resolver.GetConfig(r.Context(), id)
	if err != nil {
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, acpRouteViewFromConfig(resolver, item))
}

func (h *Handler) handleDeleteACPRoute(w http.ResponseWriter, r *http.Request) {
	resolver, err := h.acpRouteResolver()
	if err != nil {
		_ = httpjson.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := r.PathValue("id")
	item, err := resolver.GetConfig(r.Context(), id)
	if err != nil {
		if errors.Is(err, acproute.ErrRouteNotConfigured) {
			_ = httpjson.Error(w, http.StatusNotFound, "acp route not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if item.Kind != acproute.RouteKindACP {
		_ = httpjson.Error(w, http.StatusNotFound, "acp route not found")
		return
	}
	if err := resolver.DeleteConfig(r.Context(), id); err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			_ = httpjson.Error(w, http.StatusNotFound, "acp route not found")
			return
		}
		_ = httpjson.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) handleGetACPRuntime(w http.ResponseWriter, r *http.Request) {
	span := h.beginACPAdminAudit(r, "", "runtime", "")
	defer finishAdminAudit(span, http.StatusOK, "")
	if h.acpRuntimeManager == nil {
		finishAdminAudit(span, http.StatusServiceUnavailable, "service_unavailable")
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "acp runtime manager is not configured")
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{
		"in_flight":           h.acpRuntimeManager.ListInFlight(),
		"instances":           h.acpRuntimeManager.ListInstances(),
		"pending_permissions": h.acpRuntimeManager.ListPendingPermissions(),
	})
}

// handleResolveACPPermission is the operator escape hatch for answering a
// pending interactive permission request (e.g. when the turn client cannot
// reach the route-side decision endpoint).
func (h *Handler) handleResolveACPPermission(w http.ResponseWriter, r *http.Request) {
	requestID := strings.TrimSpace(r.PathValue("request_id"))
	span := h.beginACPAdminAudit(r, "", "permission", "")
	span.SetExtension(usage.ACPExtension{PermissionRequestID: requestID})
	defer finishAdminAudit(span, http.StatusOK, "")
	if h.acpRuntimeManager == nil {
		finishAdminAudit(span, http.StatusServiceUnavailable, "service_unavailable")
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "acp runtime manager is not configured")
		return
	}
	var decision acpruntime.PermissionDecision
	if err := httpjson.Decode(r, &decision); err != nil {
		finishAdminAudit(span, http.StatusBadRequest, "invalid_request")
		_ = httpjson.Error(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	if decision.RequestID == "" {
		decision.RequestID = requestID
	}
	if decision.RequestID != requestID {
		finishAdminAudit(span, http.StatusBadRequest, "invalid_request")
		_ = httpjson.Error(w, http.StatusBadRequest, "request_id in body must match path")
		return
	}
	if err := h.acpRuntimeManager.ResolvePermission(decision); err != nil {
		if errors.Is(err, acpruntime.ErrPermissionNotFound) {
			finishAdminAudit(span, http.StatusNotFound, "permission_not_found")
			_ = httpjson.Error(w, http.StatusNotFound, "permission request not found (already answered or expired)")
			return
		}
		finishAdminAudit(span, http.StatusBadRequest, "invalid_request")
		_ = httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]string{"status": "resolved"})
}

func (h *Handler) handleListACPInFlight(w http.ResponseWriter, r *http.Request) {
	span := h.beginACPAdminAudit(r, "", "runtime_inflight", "")
	defer finishAdminAudit(span, http.StatusOK, "")
	if h.acpRuntimeManager == nil {
		finishAdminAudit(span, http.StatusServiceUnavailable, "service_unavailable")
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "acp runtime manager is not configured")
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": h.acpRuntimeManager.ListInFlight()})
}

func (h *Handler) handleCloseACPThread(w http.ResponseWriter, r *http.Request) {
	serviceID := strings.TrimSpace(r.PathValue("service_id"))
	threadID := strings.TrimSpace(r.PathValue("thread_id"))
	span := h.beginACPAdminAudit(r, serviceID, "thread_close", "")
	span.SetExtension(usage.ACPExtension{ThreadID: threadID})
	defer finishAdminAudit(span, http.StatusOK, "")
	if h.acpRuntimeManager == nil {
		finishAdminAudit(span, http.StatusServiceUnavailable, "service_unavailable")
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "acp runtime manager is not configured")
		return
	}
	if serviceID == "" || threadID == "" {
		finishAdminAudit(span, http.StatusBadRequest, "invalid_request")
		_ = httpjson.Error(w, http.StatusBadRequest, "service_id and thread_id are required")
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"closed": h.acpRuntimeManager.CloseThread(serviceID, threadID)})
}

func (h *Handler) beginACPAdminAudit(r *http.Request, serviceID, operation, sessionID string) usage.InteractionSpan {
	observer := h.usageObserver
	if observer == nil {
		return usage.NoopSpan{}
	}
	// An admin request carries no inbound traceparent, so the observer generates a
	// fresh W3C trace and span id for it.
	span, _ := observer.Begin(r.Context(), usage.InteractionDimensions{
		RouteID:       "/admin/acp",
		RouteKind:     "acp",
		RouteProtocol: "admin",
	})
	span.SetExtension(usage.ACPExtension{
		ServiceID:    serviceID,
		Operation:    operation,
		SessionID:    sessionID,
		ResultStatus: "success",
	})
	return span
}

func finishAdminAudit(span usage.InteractionSpan, status int, errorType string) {
	if span == nil {
		return
	}
	if errorType != "" {
		span.SetExtension(usage.ACPExtension{ResultStatus: "error"})
	}
	span.Finish(usage.InteractionOutcome{Success: status < 400, StatusCode: status, ErrorType: errorType})
}

func acpRouteViewFromConfig(resolver *acproute.ACPRouteResolver, cfg acproute.AgentRouteConfig) ACPRouteView {
	item, _ := acproute.NewACPRouteConfigFromConfig(cfg)
	view := ACPRouteView{
		ACPRouteConfig: item,
		Source:         "store",
		ReadOnly:       false,
	}
	if resolver != nil {
		if configManager := resolver.ConfigManager(); configManager != nil && configManager.IsStatic(cfg.ID) {
			view.Source = "caddyfile"
			view.ReadOnly = true
		}
	}
	return view
}
