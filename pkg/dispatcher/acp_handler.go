package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	"go.uber.org/zap"
)

func (h *Handler) dispatchACP(w http.ResponseWriter, r *http.Request, next NextHandler, cfg routecore.AgentRouteConfig) error {
	return serveNextOrNotFound(next, w, r)
}

// dispatchACPRemoved is retained as dead source until the M7 physical cleanup.
// No dispatcher or public configuration path can call it after the M5 cutover.
func (h *Handler) dispatchACPRemoved(w http.ResponseWriter, r *http.Request, next NextHandler, cfg routecore.AgentRouteConfig) error {
	routeResolver := h.gateway.ACPRouteResolver()
	if routeResolver == nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusServiceUnavailable, w, r, "resolve acp route", "acp route resolver is not configured", fmt.Errorf("acp route resolver is not configured"))
	}
	route, err := routeResolver.Resolve(r.Context(), cfg)
	if err != nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusBadGateway, w, r, "resolve acp route", "failed to resolve acp route", err)
	}
	if route == nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusServiceUnavailable, w, r, "resolve acp route", "acp route is not configured", fmt.Errorf("acp route %q is not configured", cfg.ID))
	}
	logRequestPhase(h.logger, "dispatcher: acp route resolved", r,
		zap.String("route_id", route.ID),
		zap.String("service_id", route.ServiceID),
		zap.String("path_prefix", route.MatchPolicy.PathPrefix),
	)

	rewritten := RewriteLLMRoutePath(r, route.MatchPolicy.PathPrefix)
	endpoint, sessionID, matched := matchACPRouteEndpoint(rewritten.URL.Path)
	if !matched {
		return serveNextOrNotFound(next, w, r)
	}
	agentType := ""
	if svcMgr := h.gateway.ACPServiceManager(); svcMgr != nil {
		if cfg, err := svcMgr.Get(rewritten.Context(), route.ServiceID); err == nil {
			agentType = strings.TrimSpace(cfg.AgentType)
		}
	}
	usage.SpanFromContext(rewritten.Context()).SetExtension(usage.ACPExtension{
		ServiceID: route.ServiceID,
		AgentType: agentType,
		Operation: endpoint,
		SessionID: sessionID,
	})

	runtimeManager := h.gateway.ACPRuntimeManager()
	if runtimeManager == nil {
		return WriteDispatchError(h.logger, string(route.Protocol), route.ID, "", http.StatusServiceUnavailable, w, rewritten, "dispatch acp request", "acp runtime manager is not configured", fmt.Errorf("acp runtime manager is not configured"))
	}

	switch endpoint {
	case "turn":
		if rewritten.Method != http.MethodPost {
			return httpjson.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "permission":
		if rewritten.Method != http.MethodPost {
			return httpjson.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		if manager := h.gateway.AgentManager(); manager != nil {
			if agentID, bound := manager.ResolveAgentID(route.ID, route.ServiceID, ""); bound && h.gateway.PermissionBroker() != nil {
				return h.dispatchAgentACPPermission(w, rewritten, agentID)
			}
		}
		return h.dispatchACPPermission(w, rewritten, runtimeManager)
	case "sessions":
		if rewritten.Method != http.MethodGet {
			return httpjson.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return h.dispatchACPSessions(w, rewritten, runtimeManager, route.ServiceID)
	case "transcript":
		if rewritten.Method != http.MethodGet {
			return httpjson.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return h.dispatchACPTranscript(w, rewritten, runtimeManager, route.ServiceID, sessionID)
	default:
		return serveNextOrNotFound(next, w, r)
	}

	var req acpruntime.TurnRequest
	if rewritten.Body != nil {
		rewritten.Body = http.MaxBytesReader(w, rewritten.Body, MaxACPRequestBodyBytes)
	}
	if err := json.NewDecoder(rewritten.Body).Decode(&req); err != nil {
		return httpjson.Error(w, RequestBodyErrorStatus(err, http.StatusBadRequest), fmt.Sprintf("decode request: %v", err))
	}
	if strings.TrimSpace(req.ThreadID) == "" {
		return httpjson.Error(w, http.StatusBadRequest, "thread_id is required")
	}
	if strings.TrimSpace(req.Input) == "" {
		return httpjson.Error(w, http.StatusBadRequest, "input is required")
	}
	if dims, ok := usage.DimensionsFromContext(rewritten.Context()); ok && dims.AgentID != "" {
		agentManager := h.gateway.AgentManager()
		if agentManager == nil {
			return writeRuntimePreStreamError(w, rewritten, runtimeapi.NewError(runtimeapi.ErrorBackendUnavailable, "agent manager is unavailable"))
		}
		// Store-free lookup: the definition comes from the manager's immutable
		// generation snapshot (unified-agent-runtime M4 gate).
		a, ok := agentManager.GetSnapshot(dims.AgentID)
		if !ok {
			return writeRuntimePreStreamError(w, rewritten, runtimeapi.NewError(runtimeapi.ErrorAgentNotFound, "agent not found"))
		}
		if a.Runtime.Type != agentpkg.RuntimeTypeACP {
			return writeRuntimePreStreamError(w, rewritten, runtimeapi.NewError(runtimeapi.ErrorRuntimeNotExecutable, "ACP route is bound to a non-ACP agent"))
		}
		backend, resolveErr := h.gateway.RuntimeRegistry().Resolve(a.Runtime.Type)
		if resolveErr != nil {
			return writeRuntimePreStreamError(w, rewritten, resolveErr)
		}
		runtimeOptions, marshalErr := json.Marshal(map[string]any{
			"thread_id": req.ThreadID, "cwd": req.CWD, "model": req.Model,
			"fresh_session": req.FreshSession, "config_overrides": req.ConfigOverrides,
		})
		if marshalErr != nil {
			return writeRuntimePreStreamError(w, rewritten, runtimeapi.WrapError(runtimeapi.ErrorInvalidRequest, "invalid acp options", marshalErr))
		}
		commonReq := runtimeapi.TurnRequest{
			Input: req.Input, SessionID: req.SessionID,
			Options: runtimeapi.TurnOptions{Version: runtimeapi.TurnOptionsVersionV1, Runtime: runtimeOptions},
		}
		sequencer, sequenceErr := runtimeapi.NewTurnSequencer(rewritten.Context(), backend, a, commonReq)
		if sequenceErr != nil {
			return writeRuntimePreStreamError(w, rewritten, sequenceErr)
		}
		commonReq.RunID = sequencer.RunID()
		rewritten = rewritten.WithContext(bindRuntimeRequestContext(rewritten.Context(), a.ID, agentpkg.RuntimeTypeACP, commonReq.RunID, commonReq.SessionID, ""))
		usage.SpanFromContext(rewritten.Context()).SetExtension(usage.CommonExtension{AgentID: a.ID, RuntimeType: agentpkg.RuntimeTypeACP, RunID: commonReq.RunID})
		logRequestPhase(h.logger, "dispatcher: agent run initialized", rewritten)

		counts := map[string]int{}
		nativeSink := wrapACPEventSinkForUsage(rewritten, newACPSSESink(w), counts)
		commonSink := func(ev runtimeapi.TurnEvent) error { return nativeSink(nativeACPEvent(ev)) }
		usage.SpanFromContext(rewritten.Context()).SetExtension(usage.ACPExtension{
			ThreadID: strings.TrimSpace(req.ThreadID), SessionID: strings.TrimSpace(req.SessionID),
			FreshSession: usage.Bool(req.FreshSession), ResultStatus: "success",
		})
		result, serveErr := sequencer.ServeSegment(rewritten.Context(), backend, a, commonReq, commonSink)
		if serveErr != nil {
			usage.SpanFromContext(rewritten.Context()).SetExtension(usage.ACPExtension{ResultStatus: "error", EventCounts: counts})
			if !result.Started {
				return writeRuntimePreStreamError(w, rewritten, serveErr)
			}
			code, _ := runtimeapi.ErrorCodeOf(serveErr)
			usage.SpanFromContext(rewritten.Context()).AddAnnotation("error_type", string(code))
			return nil
		}
		usage.SpanFromContext(rewritten.Context()).SetExtension(usage.ACPExtension{EventCounts: counts})
		return nil
	}

	emit := newACPSSESink(w)
	counts := map[string]int{}
	emit = wrapACPEventSinkForUsage(rewritten, emit, counts)
	usage.SpanFromContext(rewritten.Context()).SetExtension(usage.ACPExtension{
		ThreadID:     strings.TrimSpace(req.ThreadID),
		SessionID:    strings.TrimSpace(req.SessionID),
		FreshSession: usage.Bool(req.FreshSession),
		ResultStatus: "success",
	})

	if err := runtimeManager.ServeTurn(rewritten.Context(), route.ServiceID, req, emit); err != nil {
		_ = emit(acpruntime.TurnEvent{Event: "error", Message: err.Error()})
		usage.SpanFromContext(rewritten.Context()).SetExtension(usage.ACPExtension{ResultStatus: "error", EventCounts: counts})
		return nil
	}
	usage.SpanFromContext(rewritten.Context()).SetExtension(usage.ACPExtension{EventCounts: counts})
	return nil
}

func (h *Handler) dispatchAgentACPPermission(w http.ResponseWriter, r *http.Request, agentID string) error {
	var decision acpruntime.PermissionDecision
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, MaxACPRequestBodyBytes)
	}
	if err := json.NewDecoder(r.Body).Decode(&decision); err != nil {
		return httpjson.Error(w, RequestBodyErrorStatus(err, http.StatusBadRequest), fmt.Sprintf("decode request: %v", err))
	}
	common := runtimeapi.PermissionDecision{RequestID: strings.TrimSpace(decision.RequestID), Outcome: strings.TrimSpace(decision.Outcome), OptionID: strings.TrimSpace(decision.OptionID)}
	if err := h.gateway.PermissionBroker().Resolve(runtimeapi.WithPermissionSource(r.Context(), "acp_route"), agentID, common); err != nil {
		return httpjson.Write(w, runtimeapi.HTTPStatus(err), runtimeapi.PublicError(err))
	}
	return httpjson.Write(w, http.StatusOK, map[string]string{"status": "resolved"})
}

func matchACPRouteEndpoint(path string) (endpoint string, sessionID string, matched bool) {
	switch path {
	case "/turn":
		return "turn", "", true
	case "/permission":
		return "permission", "", true
	case "/sessions":
		return "sessions", "", true
	}
	if !strings.HasPrefix(path, "/sessions/") || !strings.HasSuffix(path, "/transcript") {
		return "", "", false
	}
	rawID := strings.TrimSuffix(strings.TrimPrefix(path, "/sessions/"), "/transcript")
	rawID = strings.Trim(rawID, "/")
	if rawID == "" || strings.Contains(rawID, "/") {
		return "", "", false
	}
	id, err := url.PathUnescape(rawID)
	if err != nil || strings.TrimSpace(id) == "" {
		return "", "", false
	}
	return "transcript", strings.TrimSpace(id), true
}

// newACPSSESink builds the SSE EventSink used by the ACP turn handler. It writes
// one "event:/data:" frame per TurnEvent and flushes through the ResponseController
// Unwrap chain so frames reach the client incrementally; Caddy (v2.7+) wraps the
// ResponseWriter so a direct http.Flusher assertion no longer succeeds.
func newACPSSESink(w http.ResponseWriter) acpruntime.EventSink {
	flusher := NewResponseFlusher(w)
	started := false
	return func(event acpruntime.TurnEvent) error {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		name := strings.TrimSpace(event.Event)
		if name == "" {
			name = "delta"
		}
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
}

func nativeACPEvent(ev runtimeapi.TurnEvent) acpruntime.TurnEvent {
	native := acpruntime.TurnEvent{Event: ev.Event, SessionID: ev.SessionID, RequestID: ev.RequestID, Text: ev.Text, Data: ev.Data}
	if ev.Event == runtimeapi.EventDone || ev.Event == runtimeapi.EventError {
		var terminal struct {
			StopReason string `json:"stop_reason"`
			Message    string `json:"message"`
		}
		if json.Unmarshal(ev.Data, &terminal) == nil && (terminal.StopReason != "" || terminal.Message != "") {
			native.StopReason = terminal.StopReason
			native.Message = terminal.Message
			native.Data = nil
		}
	}
	return native
}

func wrapACPEventSinkForUsage(r *http.Request, next acpruntime.EventSink, counts map[string]int) acpruntime.EventSink {
	return func(event acpruntime.TurnEvent) error {
		name := strings.TrimSpace(event.Event)
		if name == "" {
			name = "delta"
		}
		counts[name]++
		ext := usage.ACPExtension{EventCounts: counts}
		if name == "usage" && len(event.Data) > 0 {
			ext.UsageJSON = string(event.Data)
		}
		if event.SessionID != "" {
			ext.SessionID = event.SessionID
		}
		usage.SpanFromContext(r.Context()).SetExtension(ext)
		return next(event)
	}
}

// dispatchACPPermission answers one pending interactive permission request
// surfaced to the turn client as a "permission" SSE event.
type acpPermissionRuntime interface {
	ResolvePermission(acpruntime.PermissionDecision) error
}

type acpSessionRuntime interface {
	ListSessions(context.Context, string, acpruntime.ListSessionsRequest) (acpruntime.ListSessionsResponse, error)
}

type acpTranscriptRuntime interface {
	LoadTranscript(context.Context, string, acpruntime.TranscriptRequest) (acpruntime.TranscriptResponse, error)
}

func (h *Handler) dispatchACPPermission(w http.ResponseWriter, r *http.Request, runtimeManager acpPermissionRuntime) error {
	var decision acpruntime.PermissionDecision
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, MaxACPRequestBodyBytes)
	}
	if err := json.NewDecoder(r.Body).Decode(&decision); err != nil {
		return httpjson.Error(w, RequestBodyErrorStatus(err, http.StatusBadRequest), fmt.Sprintf("decode request: %v", err))
	}
	usage.SpanFromContext(r.Context()).SetExtension(usage.ACPExtension{PermissionRequestID: strings.TrimSpace(decision.RequestID)})
	if err := runtimeManager.ResolvePermission(decision); err != nil {
		if errors.Is(err, acpruntime.ErrPermissionNotFound) {
			return httpjson.Error(w, http.StatusNotFound, "permission request not found (already answered or expired)")
		}
		return httpjson.Error(w, http.StatusBadRequest, err.Error())
	}
	usage.SpanFromContext(r.Context()).SetExtension(usage.ACPExtension{ResultStatus: "success"})
	return httpjson.Write(w, http.StatusOK, map[string]string{"status": "resolved"})
}

func (h *Handler) dispatchACPSessions(w http.ResponseWriter, r *http.Request, runtimeManager acpSessionRuntime, serviceID string) error {
	result, err := runtimeManager.ListSessions(r.Context(), serviceID, acpruntime.ListSessionsRequest{
		CWD:    strings.TrimSpace(r.URL.Query().Get("cwd")),
		Cursor: strings.TrimSpace(r.URL.Query().Get("cursor")),
	})
	if err != nil {
		return httpjson.Error(w, acpRequestErrorStatus(err), err.Error())
	}
	usage.SpanFromContext(r.Context()).SetExtension(usage.ACPExtension{ResultStatus: "success"})
	return httpjson.Write(w, http.StatusOK, result)
}

func (h *Handler) dispatchACPTranscript(w http.ResponseWriter, r *http.Request, runtimeManager acpTranscriptRuntime, serviceID, sessionID string) error {
	result, err := runtimeManager.LoadTranscript(r.Context(), serviceID, acpruntime.TranscriptRequest{
		SessionID: sessionID,
		CWD:       strings.TrimSpace(r.URL.Query().Get("cwd")),
	})
	if err != nil {
		return httpjson.Error(w, acpRequestErrorStatus(err), err.Error())
	}
	usage.SpanFromContext(r.Context()).SetExtension(usage.ACPExtension{SessionID: sessionID, ResultStatus: "success"})
	return httpjson.Write(w, http.StatusOK, result)
}

// acpRequestErrorStatus maps a session/transcript error to an HTTP status: 404
// when the route's service is not configured, 400 for a client-correctable
// request problem, and 502 for an upstream agent/transport failure.
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
