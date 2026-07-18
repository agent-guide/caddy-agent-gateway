package dispatcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	builtinhost "github.com/agent-guide/agent-gateway/pkg/agent/builtin"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	"go.uber.org/zap"
)

// dispatchBuiltin serves the builtin-agent turn ingress: POST /<route>/turn
// with SSE, mirroring the ACP turn shape with the builtin event vocabulary
// subset (session, delta, content, tool_call, usage, permission, done,
// error). A permission-carrying request resumes a suspended turn on this
// request's stream (§5.7.7).
func (h *Handler) dispatchBuiltin(w http.ResponseWriter, r *http.Request, next NextHandler, cfg routecore.AgentRouteConfig) error {
	if !h.builtinEnabled {
		return serveNextOrNotFound(next, w, r)
	}
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

	host := h.gateway.BuiltinHost()
	if host == nil {
		return WriteDispatchError(h.logger, string(route.Protocol), route.ID, "", http.StatusServiceUnavailable, w, rewritten, "dispatch builtin turn", "builtin host is not configured", fmt.Errorf("builtin host is not configured"))
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

	// SSE headers are written lazily on the first event: everything the host
	// validates synchronously (unknown agent, disabled agent, empty input,
	// depth gate, concurrency limit, materialization) fails BEFORE the first
	// event, so those failures return real HTTP status codes instead of a 200
	// stream — the ErrInvalidRequest -> 400 contract, mirroring ACP.
	sink := newBuiltinSSESink(w)
	if err := host.ServeTurn(rewritten.Context(), route.AgentID, req, sink.emit); err != nil {
		if !sink.started {
			status := builtinTurnErrorStatus(err)
			usage.SpanFromContext(rewritten.Context()).AddAnnotation("error_type", builtinTurnErrorType(err))
			return httpjson.Error(w, status, err.Error())
		}
		// Mid-stream failures keep the 200 SSE stream (the host has already
		// emitted a terminal error event); mark the turn on the span.
		usage.SpanFromContext(rewritten.Context()).SetExtension(usage.BuiltinExtension{ResultStatus: "error"})
		usage.SpanFromContext(rewritten.Context()).AddAnnotation("error_type", builtinTurnErrorType(err))
	}
	return nil
}

// builtinTurnErrorStatus maps pre-stream host errors onto HTTP status codes.
func builtinTurnErrorStatus(err error) int {
	switch {
	case errors.Is(err, builtinhost.ErrAgentNotFound):
		return http.StatusNotFound
	case errors.Is(err, builtinhost.ErrTurnLimitExceeded),
		errors.Is(err, builtinhost.ErrSessionBusy),
		errors.Is(err, builtinhost.ErrSessionLimitExceeded),
		errors.Is(err, builtinhost.ErrPermissionCapacity):
		return http.StatusTooManyRequests
	case errors.Is(err, builtinhost.ErrInvalidRequest):
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

// builtinTurnErrorType maps host errors onto the normalized error_type
// vocabulary for the turn usage event.
func builtinTurnErrorType(err error) string {
	switch {
	case errors.Is(err, builtinhost.ErrAgentNotFound):
		return "agent_not_found"
	case errors.Is(err, builtinhost.ErrTurnLimitExceeded):
		return "turn_limit_exceeded"
	case errors.Is(err, builtinhost.ErrSessionBusy):
		return "session_busy"
	case errors.Is(err, builtinhost.ErrSessionLimitExceeded):
		return "session_limit_exceeded"
	case errors.Is(err, builtinhost.ErrPermissionCapacity):
		return "permission_capacity_exceeded"
	case errors.Is(err, builtinhost.ErrInvalidRequest):
		return "invalid_request"
	default:
		return "turn_failed"
	}
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
