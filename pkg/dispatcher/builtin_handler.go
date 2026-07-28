package dispatcher

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	builtinhost "github.com/agent-guide/agent-gateway/pkg/agent/builtin"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	"go.uber.org/zap"
)

// dispatchBuiltin serves the builtin-agent turn ingress: POST /<route>/turn
// with SSE, mirroring the ACP turn shape with the builtin event vocabulary
// subset (session, delta, content, tool_call, usage, permission, done,
// error). A permission-carrying request resumes a suspended turn on this
// request's stream (§5.7.7).
func (h *Handler) dispatchBuiltin(w http.ResponseWriter, r *http.Request, next NextHandler, cfg routecore.AgentRouteConfig) error {
	return serveNextOrNotFound(next, w, r)
}

// dispatchBuiltinRemoved is retained as dead source until the M7 physical
// cleanup. No dispatcher or public configuration path can call it after M5.
func (h *Handler) dispatchBuiltinRemoved(w http.ResponseWriter, r *http.Request, next NextHandler, cfg routecore.AgentRouteConfig) error {
	routeResolver := h.gateway.BuiltinRouteResolver()
	if routeResolver == nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusServiceUnavailable, w, r, "resolve builtin route", "builtin route resolver is not configured", fmt.Errorf("builtin route resolver is not configured"))
	}
	route, err := routeResolver.Resolve(r.Context(), cfg)
	if err != nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusBadGateway, w, r, "resolve builtin route", "failed to resolve builtin route", err)
	}
	if route == nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusServiceUnavailable, w, r, "resolve builtin route", "builtin route is not configured", fmt.Errorf("builtin route %q is not configured", cfg.ID))
	}
	logRequestPhase(h.logger, "dispatcher: builtin route resolved", r,
		zap.String("route_id", route.ID),
		zap.String("agent_id", route.AgentID),
		zap.String("path_prefix", route.MatchPolicy.PathPrefix),
	)

	rewritten := RewriteLLMRoutePath(r, route.MatchPolicy.PathPrefix)
	if rewritten.URL.Path != "/turn" {
		return serveNextOrNotFound(next, w, r)
	}
	if rewritten.Method != http.MethodPost {
		usage.SpanFromContext(rewritten.Context()).AddAnnotation("error_type", "method_not_allowed")
		return httpjson.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}

	var req builtinhost.TurnRequest
	if rewritten.Body != nil {
		rewritten.Body = http.MaxBytesReader(w, rewritten.Body, MaxACPRequestBodyBytes)
	}
	if err := json.NewDecoder(rewritten.Body).Decode(&req); err != nil {
		usage.SpanFromContext(rewritten.Context()).AddAnnotation("error_type", "invalid_request")
		return httpjson.Error(w, RequestBodyErrorStatus(err, http.StatusBadRequest), fmt.Sprintf("decode request: %v", err))
	}
	req.Input = strings.TrimSpace(req.Input)
	req.SessionID = strings.TrimSpace(req.SessionID)
	// Exactly one of input and permission: input starts a turn, permission
	// resumes one suspended on a tool-permission interrupt (§5.7.7). The host
	// re-validates; this guard just gives body-shape mistakes a crisp 400.
	if req.Input == "" && req.Permission == nil {
		usage.SpanFromContext(rewritten.Context()).AddAnnotation("error_type", "invalid_request")
		return httpjson.Error(w, http.StatusBadRequest, "input or permission is required")
	}
	agentManager := h.gateway.AgentManager()
	if agentManager == nil {
		return WriteDispatchError(h.logger, string(route.Protocol), route.ID, "", http.StatusServiceUnavailable, w, rewritten, "dispatch builtin turn", "agent manager is not configured", fmt.Errorf("agent manager is not configured"))
	}
	// Store-free lookup: the definition comes from the manager's immutable
	// generation snapshot (unified-agent-runtime M4 gate).
	a, ok := agentManager.GetSnapshot(route.AgentID)
	if !ok {
		return writeRuntimePreStreamError(w, rewritten, runtimeapi.NewError(runtimeapi.ErrorAgentNotFound, "agent not found"))
	}
	if a.Runtime.Type != agentpkg.RuntimeTypeBuiltin {
		return writeRuntimePreStreamError(w, rewritten, runtimeapi.NewError(runtimeapi.ErrorRuntimeNotExecutable, "builtin route is bound to a non-builtin agent"))
	}
	backend, err := h.gateway.RuntimeRegistry().Resolve(a.Runtime.Type)
	if err != nil {
		return writeRuntimePreStreamError(w, rewritten, err)
	}
	commonReq := runtimeapi.TurnRequest{Input: req.Input, SessionID: req.SessionID}
	if req.Permission != nil {
		commonReq.Permission = &runtimeapi.PermissionDecision{RequestID: req.Permission.RequestID, Outcome: req.Permission.Outcome}
		for _, decision := range req.Permission.Decisions {
			commonReq.Permission.Decisions = append(commonReq.Permission.Decisions, runtimeapi.PermissionActionDecision{ActionID: decision.CallID, Outcome: decision.Outcome})
		}
	}
	sequencer, err := runtimeapi.NewTurnSequencer(rewritten.Context(), backend, a, commonReq)
	if err != nil {
		return writeRuntimePreStreamError(w, rewritten, err)
	}
	commonReq.RunID = sequencer.RunID()
	rewritten = rewritten.WithContext(bindRuntimeRequestContext(rewritten.Context(), a.ID, a.Runtime.Type, commonReq.RunID, commonReq.SessionID, permissionRequestID(commonReq.Permission)))
	usage.SpanFromContext(rewritten.Context()).SetExtension(usage.CommonExtension{AgentID: a.ID, RuntimeType: a.Runtime.Type, RunID: commonReq.RunID})
	logRequestPhase(h.logger, "dispatcher: agent run initialized", rewritten)

	// SSE headers are written lazily on the first event: everything the host
	// validates synchronously (unknown agent, disabled agent, empty input,
	// depth gate, concurrency limit, materialization) fails BEFORE the first
	// event, so those failures return real HTTP status codes instead of a 200
	// stream — the ErrInvalidRequest -> 400 contract, mirroring ACP.
	sink := newBuiltinSSESink(w)
	result, serveErr := sequencer.ServeSegment(rewritten.Context(), backend, a, commonReq, sink.emitCommon)
	if serveErr != nil {
		code, _ := runtimeapi.ErrorCodeOf(serveErr)
		if !result.Started {
			usage.SpanFromContext(rewritten.Context()).AddAnnotation("error_type", string(code))
			return writeRuntimePreStreamError(w, rewritten, serveErr)
		}
		// Mid-stream failures keep the 200 SSE stream (the host has already
		// emitted a terminal error event); mark the turn on the span.
		usage.SpanFromContext(rewritten.Context()).SetExtension(usage.BuiltinExtension{ResultStatus: "error"})
		usage.SpanFromContext(rewritten.Context()).AddAnnotation("error_type", string(code))
	}
	return nil
}

// builtinSSESink writes turn events as SSE frames, mirroring the ACP sink.
// Headers are written lazily on the first event so pre-stream failures can
// still return real HTTP status codes.
type builtinSSESink struct {
	w       http.ResponseWriter
	flusher ResponseFlusher
	started bool
}

func newBuiltinSSESink(w http.ResponseWriter) *builtinSSESink {
	return &builtinSSESink{w: w, flusher: NewResponseFlusher(w)}
}

func (s *builtinSSESink) emit(ev builtinhost.TurnEvent) error {
	if !s.started {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("Connection", "keep-alive")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}
	name := ev.Event
	if name == "" {
		name = builtinhost.EventDelta
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, payload); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s *builtinSSESink) emitCommon(ev runtimeapi.TurnEvent) error {
	native := builtinhost.TurnEvent{
		Event: ev.Event, SessionID: ev.SessionID, RunID: ev.RunID,
		RequestID: ev.RequestID, Text: ev.Text, Data: ev.Data,
	}
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
	return s.emit(native)
}
