package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
)

func (h *Handler) handleGetACPRuntime(w http.ResponseWriter, r *http.Request) {
	span := h.beginACPAdminAudit(r, "", "runtime", "")
	defer finishAdminAudit(span, http.StatusOK, "")
	if h.acpRuntimeManager == nil {
		finishAdminAudit(span, http.StatusServiceUnavailable, "service_unavailable")
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "acp runtime manager is not configured")
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{
		"in_flight":           acpInFlightViews(h.acpRuntimeManager.ListInFlight()),
		"instances":           acpInstanceViews(h.acpRuntimeManager.ListInstances()),
		"pending_permissions": acpPermissionViews(h.acpRuntimeManager.ListPendingPermissions()),
	})
}

func (h *Handler) handleListACPInFlight(w http.ResponseWriter, r *http.Request) {
	span := h.beginACPAdminAudit(r, "", "runtime_inflight", "")
	defer finishAdminAudit(span, http.StatusOK, "")
	if h.acpRuntimeManager == nil {
		finishAdminAudit(span, http.StatusServiceUnavailable, "service_unavailable")
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "acp runtime manager is not configured")
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": acpInFlightViews(h.acpRuntimeManager.ListInFlight())})
}

type acpInFlightView struct {
	AgentID string `json:"agent_id"`
	Scope   string `json:"scope"`
}

type acpInstanceView struct {
	AgentID string `json:"agent_id"`
	acpruntime.PooledInstanceInfo
}

type acpPermissionView struct {
	RequestID string    `json:"request_id"`
	AgentID   string    `json:"agent_id"`
	SessionID string    `json:"session_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func acpInFlightViews(items []acpruntime.InFlightTurn) []acpInFlightView {
	out := make([]acpInFlightView, 0, len(items))
	for _, item := range items {
		out = append(out, acpInFlightView{AgentID: acpruntime.ScopeOwnerID(item.Scope), Scope: item.Scope})
	}
	return out
}

func acpInstanceViews(items []acpruntime.PooledInstanceInfo) []acpInstanceView {
	out := make([]acpInstanceView, 0, len(items))
	for _, item := range items {
		out = append(out, acpInstanceView{AgentID: acpruntime.ScopeOwnerID(item.Scope), PooledInstanceInfo: item})
	}
	return out
}

func acpPermissionViews(items []acpruntime.PendingPermissionInfo) []acpPermissionView {
	out := make([]acpPermissionView, 0, len(items))
	for _, item := range items {
		out = append(out, acpPermissionView{RequestID: item.RequestID, AgentID: item.OwnerID, SessionID: item.SessionID, CreatedAt: item.CreatedAt})
	}
	return out
}

func (h *Handler) handleCloseACPThread(w http.ResponseWriter, r *http.Request) {
	agentID := strings.TrimSpace(r.PathValue("agent_id"))
	threadID := strings.TrimSpace(r.PathValue("thread_id"))
	span := h.beginACPAdminAudit(r, agentID, "thread_close", "")
	span.SetExtension(usage.ACPExtension{ThreadID: threadID})
	defer finishAdminAudit(span, http.StatusOK, "")
	if h.acpRuntimeManager == nil {
		finishAdminAudit(span, http.StatusServiceUnavailable, "service_unavailable")
		_ = httpjson.Error(w, http.StatusServiceUnavailable, "acp runtime manager is not configured")
		return
	}
	if agentID == "" || threadID == "" {
		finishAdminAudit(span, http.StatusBadRequest, "invalid_request")
		_ = httpjson.Error(w, http.StatusBadRequest, "agent_id and thread_id are required")
		return
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"closed": h.acpRuntimeManager.CloseThread(agentID, threadID)})
}

func (h *Handler) beginACPAdminAudit(r *http.Request, agentID, operation, sessionID string) usage.InteractionSpan {
	observer := h.usageObserver
	if observer == nil {
		return usage.NoopSpan{}
	}
	span, _ := observer.Begin(r.Context(), usage.InteractionDimensions{
		RouteID: "/admin/acp", RouteKind: "acp", RouteProtocol: "admin",
		AgentID: agentID, RuntimeType: agentpkg.RuntimeTypeACP,
	})
	span.SetExtension(usage.ACPExtension{Operation: operation, SessionID: sessionID, ResultStatus: "success"})
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
