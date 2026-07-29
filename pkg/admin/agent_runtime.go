package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
)

func (h *Handler) agentBackend(a agentpkg.Agent) (runtimeapi.Backend, error) {
	if h.runtimeRegistry == nil {
		return nil, runtimeapi.NewError(runtimeapi.ErrorRuntimeNotExecutable, "runtime registry is not configured")
	}
	return h.runtimeRegistry.Resolve(a.Runtime.Type)
}

func (h *Handler) agentCapabilities(ctx context.Context, a agentpkg.Agent) (runtimeapi.Capabilities, error) {
	if a.Disabled {
		return runtimeapi.Capabilities{Executable: false}, nil
	}
	b, err := h.agentBackend(a)
	if err != nil {
		return runtimeapi.Capabilities{Executable: false}, nil
	}
	return b.Capabilities(ctx, a)
}

func (h *Handler) agentRuntimeRead(ctx context.Context, a agentpkg.Agent) (*runtimeapi.RuntimeSummary, *runtimeapi.Capabilities, error) {
	caps, err := h.agentCapabilities(ctx, a)
	if err != nil {
		return nil, nil, err
	}
	if a.Disabled {
		s := runtimeapi.RuntimeSummary{Type: a.Runtime.Type, State: runtimeapi.RuntimeStateDisabled}
		return &s, &caps, nil
	}
	b, resolveErr := h.agentBackend(a)
	if resolveErr != nil {
		s := runtimeapi.RuntimeSummary{Type: a.Runtime.Type, State: runtimeapi.RuntimeStateNotExecutable}
		return &s, &caps, nil
	}
	inspector, ok := b.(runtimeapi.RuntimeInspector)
	if !ok {
		s := runtimeapi.RuntimeSummary{Type: a.Runtime.Type, Executable: caps.Executable, State: runtimeapi.RuntimeStateUnknown}
		return &s, &caps, nil
	}
	summary, err := inspector.RuntimeSummary(ctx, a)
	if err != nil {
		return nil, nil, err
	}
	return &summary, &caps, nil
}

func writeRuntimeError(w http.ResponseWriter, err error) {
	_ = httpjson.Write(w, runtimeapi.HTTPStatus(err), runtimeapi.PublicError(err))
}

func (h *Handler) handleGetAgentCapabilities(w http.ResponseWriter, r *http.Request) {
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	caps, err := h.agentCapabilities(r.Context(), a)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	_ = httpjson.Write(w, http.StatusOK, caps)
}

func (h *Handler) handleListAgentRuns(w http.ResponseWriter, r *http.Request) {
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	items := []runtimeapi.RunInfo{}
	if h.runRegistry != nil {
		items = h.runRegistry.List(a.ID)
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": items, "durable": false})
}

func (h *Handler) handleCancelAgentRun(w http.ResponseWriter, r *http.Request) {
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	runID := strings.TrimSpace(r.PathValue("run_id"))
	span := h.beginAgentControlAudit(r, a, "run_cancel", runID, "", "")
	auditStatus, auditError := http.StatusOK, ""
	defer func() { finishAgentControlAudit(span, a.Runtime.Type, auditStatus, auditError) }()
	b, err := h.agentBackend(a)
	if err != nil {
		auditStatus, auditError = runtimeapi.HTTPStatus(err), string(runtimeapi.PublicError(err).ErrorType)
		writeRuntimeError(w, err)
		return
	}
	c, ok := b.(runtimeapi.RunCanceller)
	if !ok {
		auditStatus, auditError = http.StatusNotImplemented, string(runtimeapi.ErrorCapabilityNotSupported)
		writeRuntimeError(w, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "run cancellation is not supported"))
		return
	}
	mode := runtimeapi.CancelMode(strings.TrimSpace(r.URL.Query().Get("mode")))
	if mode == "" {
		mode = runtimeapi.CancelModeForce
	}
	if mode != runtimeapi.CancelModeForce && mode != runtimeapi.CancelModeGraceful {
		auditStatus, auditError = http.StatusBadRequest, string(runtimeapi.ErrorInvalidRequest)
		writeRuntimeError(w, runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "invalid cancel mode"))
		return
	}
	caps, err := b.Capabilities(r.Context(), a)
	if err != nil {
		auditStatus, auditError = runtimeapi.HTTPStatus(err), string(runtimeapi.PublicError(err).ErrorType)
		writeRuntimeError(w, err)
		return
	}
	if (mode == runtimeapi.CancelModeForce && !caps.Cancellation.Force) ||
		(mode == runtimeapi.CancelModeGraceful && !caps.Cancellation.Graceful) {
		auditStatus, auditError = http.StatusNotImplemented, string(runtimeapi.ErrorCapabilityNotSupported)
		writeRuntimeError(w, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "cancel mode is not supported"))
		return
	}
	result, err := c.CancelRun(r.Context(), a, runtimeapi.CancelRequest{RunID: runID, Mode: mode})
	if err != nil {
		auditStatus, auditError = runtimeapi.HTTPStatus(err), string(runtimeapi.PublicError(err).ErrorType)
		if code, ok := runtimeapi.ErrorCodeOf(err); ok && code == runtimeapi.ErrorBackendUnavailable {
			w.Header().Set("Retry-After", strconv.Itoa(1))
		}
		writeRuntimeError(w, err)
		return
	}
	_ = httpjson.Write(w, http.StatusOK, result)
}

func (h *Handler) handleListAgentPermissions(w http.ResponseWriter, r *http.Request) {
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	items := []runtimeapi.PendingPermission{}
	if h.permissionBroker != nil {
		items = h.permissionBroker.List(a.ID)
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) handleResolveAgentPermission(w http.ResponseWriter, r *http.Request) {
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	pathID := strings.TrimSpace(r.PathValue("request_id"))
	runID, sessionID := "", ""
	if h.permissionBroker != nil {
		if correlation, found := h.permissionBroker.LookupPermission(pathID); found && correlation.AgentID == a.ID {
			runID, sessionID = correlation.RunID, correlation.SessionID
		}
	}
	span := h.beginAgentControlAudit(r, a, "permission", runID, sessionID, pathID)
	auditStatus, auditError := http.StatusOK, ""
	defer func() { finishAgentControlAudit(span, a.Runtime.Type, auditStatus, auditError) }()
	var decision runtimeapi.PermissionDecision
	if err := httpjson.Decode(r, &decision); err != nil {
		auditStatus, auditError = http.StatusBadRequest, string(runtimeapi.ErrorInvalidRequest)
		writeRuntimeError(w, runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "invalid permission decision"))
		return
	}
	if decision.RequestID == "" {
		decision.RequestID = pathID
	}
	if decision.RequestID != pathID {
		auditStatus, auditError = http.StatusBadRequest, string(runtimeapi.ErrorInvalidRequest)
		writeRuntimeError(w, runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "request_id does not match path"))
		return
	}
	b, err := h.agentBackend(a)
	if err != nil {
		auditStatus, auditError = runtimeapi.HTTPStatus(err), string(runtimeapi.PublicError(err).ErrorType)
		writeRuntimeError(w, err)
		return
	}
	resolver, supported := b.(runtimeapi.PermissionResolver)
	if !supported {
		auditStatus, auditError = http.StatusNotImplemented, string(runtimeapi.ErrorCapabilityNotSupported)
		writeRuntimeError(w, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "permission resolution is not supported"))
		return
	}
	caps, err := b.Capabilities(r.Context(), a)
	if err != nil {
		auditStatus, auditError = runtimeapi.HTTPStatus(err), string(runtimeapi.PublicError(err).ErrorType)
		writeRuntimeError(w, err)
		return
	}
	resumeRequired := caps.Permissions.ResumeMode == runtimeapi.PermissionResumeNewStream
	ctx := runtimeapi.WithPermissionSource(r.Context(), "agent_admin")
	if err := resolver.ResolvePermission(ctx, a, decision); err != nil {
		auditStatus, auditError = runtimeapi.HTTPStatus(err), string(runtimeapi.PublicError(err).ErrorType)
		writeRuntimeError(w, err)
		return
	}
	response := map[string]any{"status": "accepted", "request_id": pathID}
	if resumeRequired {
		response["resume_required"] = true
	}
	_ = httpjson.Write(w, http.StatusOK, response)
}

func (h *Handler) beginAgentControlAudit(r *http.Request, a agentpkg.Agent, operation, runID, sessionID, requestID string) usage.InteractionSpan {
	if h.usageObserver == nil {
		return usage.NoopSpan{}
	}
	span, _ := h.usageObserver.Begin(r.Context(), usage.InteractionDimensions{
		RouteID: "/admin/agents", RouteKind: "agent", RouteProtocol: "admin",
		AgentID: a.ID, RuntimeType: a.Runtime.Type, RunID: strings.TrimSpace(runID),
	})
	switch a.Runtime.Type {
	case agentpkg.RuntimeTypeACP:
		span.SetExtension(usage.ACPExtension{Operation: operation, SessionID: sessionID, PermissionRequestID: requestID, ResultStatus: "success"})
	case agentpkg.RuntimeTypeBuiltin:
		span.SetExtension(usage.BuiltinExtension{Operation: operation, RunID: runID, SessionID: sessionID, PermissionRequestID: requestID, ResultStatus: "success"})
	}
	return span
}

func finishAgentControlAudit(span usage.InteractionSpan, runtimeType string, status int, errorType string) {
	if errorType != "" {
		switch runtimeType {
		case agentpkg.RuntimeTypeACP:
			span.SetExtension(usage.ACPExtension{ResultStatus: "error"})
		case agentpkg.RuntimeTypeBuiltin:
			span.SetExtension(usage.BuiltinExtension{ResultStatus: "error"})
		}
	}
	span.Finish(usage.InteractionOutcome{Success: status < 400, StatusCode: status, ErrorType: errorType})
}

func (h *Handler) handleListAgentSessions(w http.ResponseWriter, r *http.Request) {
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	b, err := h.agentBackend(a)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	lister, ok := b.(runtimeapi.SessionLister)
	if !ok {
		writeRuntimeError(w, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "session listing is not supported"))
		return
	}
	caps, err := b.Capabilities(r.Context(), a)
	if err != nil || !caps.Sessions.List {
		if err == nil {
			err = runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "session listing is not supported")
		}
		writeRuntimeError(w, err)
		return
	}
	resp, err := lister.ListSessions(r.Context(), a, runtimeapi.ListSessionsRequest{CWD: strings.TrimSpace(r.URL.Query().Get("cwd")), Cursor: strings.TrimSpace(r.URL.Query().Get("cursor"))})
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}

func (h *Handler) handleGetAgentTranscript(w http.ResponseWriter, r *http.Request) {
	a, ok := h.getAgentOr404(w, r)
	if !ok {
		return
	}
	b, err := h.agentBackend(a)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	loader, ok := b.(runtimeapi.TranscriptLoader)
	if !ok {
		writeRuntimeError(w, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "transcript is not supported"))
		return
	}
	caps, capErr := b.Capabilities(r.Context(), a)
	if capErr != nil {
		writeRuntimeError(w, capErr)
		return
	}
	if !caps.Sessions.Transcript {
		writeRuntimeError(w, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "transcript is not supported"))
		return
	}
	resp, err := loader.LoadTranscript(r.Context(), a, runtimeapi.TranscriptRequest{SessionID: strings.TrimSpace(r.PathValue("session_id")), CWD: strings.TrimSpace(r.URL.Query().Get("cwd"))})
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
}
