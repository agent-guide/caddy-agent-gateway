package dispatcher

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	"go.uber.org/zap"
)

// dispatchAgent serves the unified kind=agent ingress
// (docs/plans/unified-agent-runtime.md §6.4-§6.5): resolve the AgentRoute,
// resolve the target Agent from the manager's definition snapshot (never the
// config store), reject disabled Agents before any stream starts, match the
// endpoint operation, resolve the backend by runtime.type, check the backend
// capability for optional operations, and execute through the backend adapter.
// The guaranteed operation is POST /turn (SSE with the common event envelope);
// /permission, /sessions, and /sessions/{id}/transcript are capabilities and
// fail closed with capability_not_supported.
func (h *Handler) dispatchAgent(w http.ResponseWriter, r *http.Request, next NextHandler, cfg routecore.AgentRouteConfig) error {
	if !h.agentEnabled {
		return serveNextOrNotFound(next, w, r)
	}
	routeResolver := h.gateway.AgentRouteResolver()
	if routeResolver == nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusServiceUnavailable, w, r, "resolve agent route", "agent route resolver is not configured", fmt.Errorf("agent route resolver is not configured"))
	}
	route, err := routeResolver.Resolve(r.Context(), cfg)
	if err != nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusBadGateway, w, r, "resolve agent route", "failed to resolve agent route", err)
	}
	if route == nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusServiceUnavailable, w, r, "resolve agent route", "agent route is not configured", fmt.Errorf("agent route %q is not configured", cfg.ID))
	}
	logRequestPhase(h.logger, "dispatcher: agent route resolved", r,
		zap.String("route_id", route.ID),
		zap.String("agent_id", route.AgentID),
		zap.String("path_prefix", route.MatchPolicy.PathPrefix),
	)

	rewritten := RewriteLLMRoutePath(r, route.MatchPolicy.PathPrefix)
	endpoint, sessionID, matched := matchACPRouteEndpoint(rewritten.URL.Path)
	if !matched {
		return serveNextOrNotFound(next, w, r)
	}

	// The Agent definition comes from the manager's immutable generation
	// snapshot; per-request dispatch must not read the config store (M4 gate).
	agentManager := h.gateway.AgentManager()
	if agentManager == nil {
		return WriteDispatchError(h.logger, string(route.Protocol), route.ID, "", http.StatusServiceUnavailable, w, rewritten, "dispatch agent request", "agent manager is not configured", fmt.Errorf("agent manager is not configured"))
	}
	a, ok := agentManager.GetSnapshot(route.AgentID)
	if !ok {
		return writeRuntimePreStreamError(w, rewritten, runtimeapi.NewError(runtimeapi.ErrorAgentNotFound, "agent not found"))
	}
	if a.Disabled {
		return writeRuntimePreStreamError(w, rewritten, runtimeapi.NewError(runtimeapi.ErrorAgentDisabled, "agent is disabled"))
	}
	backend, err := h.gateway.RuntimeRegistry().Resolve(a.Runtime.Type)
	if err != nil {
		return writeRuntimePreStreamError(w, rewritten, err)
	}
	caps, err := backend.Capabilities(rewritten.Context(), a)
	if err != nil {
		return writeRuntimePreStreamError(w, rewritten, err)
	}
	if !caps.Executable {
		return writeRuntimePreStreamError(w, rewritten, runtimeapi.NewError(runtimeapi.ErrorRuntimeNotExecutable, "agent runtime is not executable"))
	}

	switch endpoint {
	case "turn":
		if rewritten.Method != http.MethodPost {
			usage.SpanFromContext(rewritten.Context()).AddAnnotation("error_type", "method_not_allowed")
			return httpjson.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return h.serveAgentTurn(w, rewritten, backend, a)
	case "permission":
		if rewritten.Method != http.MethodPost {
			usage.SpanFromContext(rewritten.Context()).AddAnnotation("error_type", "method_not_allowed")
			return httpjson.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return h.serveAgentPermission(w, rewritten, backend, a, caps)
	case "sessions":
		if rewritten.Method != http.MethodGet {
			usage.SpanFromContext(rewritten.Context()).AddAnnotation("error_type", "method_not_allowed")
			return httpjson.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return h.serveAgentSessions(w, rewritten, backend, a, caps)
	case "transcript":
		if rewritten.Method != http.MethodGet {
			usage.SpanFromContext(rewritten.Context()).AddAnnotation("error_type", "method_not_allowed")
			return httpjson.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return h.serveAgentTranscript(w, rewritten, backend, a, sessionID, caps)
	default:
		return serveNextOrNotFound(next, w, r)
	}
}

func (h *Handler) serveAgentTurn(w http.ResponseWriter, r *http.Request, backend runtimeapi.Backend, a agentpkg.Agent) error {
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, MaxACPRequestBodyBytes)
	}
	// The AgentRoute wire accepts only the common fields plus the versioned
	// options envelope; unknown top-level, envelope, or selected-runtime
	// fields fail with unsupported_option instead of being ignored (§6.5).
	req, err := runtimeapi.DecodeTurnRequest(r.Body)
	if err != nil {
		return writeRuntimePreStreamError(w, r, err)
	}
	req.Input = strings.TrimSpace(req.Input)
	if req.Input == "" && req.Permission == nil {
		return writeRuntimePreStreamError(w, r, runtimeapi.NewError(runtimeapi.ErrorInvalidRequest, "input or permission is required"))
	}
	sequencer, err := runtimeapi.NewTurnSequencer(r.Context(), backend, a, req)
	if err != nil {
		return writeRuntimePreStreamError(w, r, err)
	}
	req.RunID = sequencer.RunID()
	r = r.WithContext(bindRuntimeRequestContext(r.Context(), a.ID, a.Runtime.Type, req.RunID, req.SessionID, permissionRequestID(req.Permission)))
	usage.SpanFromContext(r.Context()).SetExtension(usage.CommonExtension{AgentID: a.ID, RuntimeType: a.Runtime.Type, RunID: req.RunID})
	logRequestPhase(h.logger, "dispatcher: agent run initialized", r)

	// SSE headers are written lazily on the first event so synchronous backend
	// validation failures return real HTTP status codes; mid-stream failures
	// become the sequencer-owned terminal SSE error event.
	sink := newAgentSSESink(w)
	result, serveErr := sequencer.ServeSegment(r.Context(), backend, a, req, sink.emit)
	if serveErr != nil {
		code, _ := runtimeapi.ErrorCodeOf(serveErr)
		if !result.Started {
			usage.SpanFromContext(r.Context()).AddAnnotation("error_type", string(code))
			return writeRuntimePreStreamError(w, r, serveErr)
		}
		usage.SpanFromContext(r.Context()).AddAnnotation("error_type", string(code))
	}
	return nil
}

func (h *Handler) serveAgentPermission(w http.ResponseWriter, r *http.Request, backend runtimeapi.Backend, a agentpkg.Agent, caps runtimeapi.Capabilities) error {
	resolver, ok := backend.(runtimeapi.PermissionResolver)
	// Route-scoped /permission resolves a pending request while its original
	// turn stream stays active. A new_stream backend (builtin) continues its
	// permission decision on POST /turn, so this operation stays fail-closed
	// there until a separate permission-contract decision changes it (§6.5).
	if !ok || !caps.Permissions.Interactive || caps.Permissions.ResumeMode != runtimeapi.PermissionResumeActiveStream {
		return writeRuntimePreStreamError(w, r, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "permission resolution is not supported on this route"))
	}
	var decision runtimeapi.PermissionDecision
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, MaxACPRequestBodyBytes)
	}
	if err := json.NewDecoder(r.Body).Decode(&decision); err != nil {
		return httpjson.Error(w, RequestBodyErrorStatus(err, http.StatusBadRequest), fmt.Sprintf("decode request: %v", err))
	}
	decision.RequestID = strings.TrimSpace(decision.RequestID)
	decision.Outcome = strings.TrimSpace(decision.Outcome)
	decision.OptionID = strings.TrimSpace(decision.OptionID)
	if err := resolver.ResolvePermission(runtimeapi.WithPermissionSource(r.Context(), "agent_route"), a, decision); err != nil {
		public := runtimeapi.PublicError(err)
		usage.SpanFromContext(r.Context()).AddAnnotation("error_type", string(public.ErrorType))
		return httpjson.Write(w, runtimeapi.HTTPStatus(err), public)
	}
	return httpjson.Write(w, http.StatusOK, map[string]string{"status": "resolved"})
}

func (h *Handler) serveAgentSessions(w http.ResponseWriter, r *http.Request, backend runtimeapi.Backend, a agentpkg.Agent, caps runtimeapi.Capabilities) error {
	lister, ok := backend.(runtimeapi.SessionLister)
	if !ok || !caps.Sessions.List {
		return writeRuntimePreStreamError(w, r, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "session listing is not supported on this route"))
	}
	result, err := lister.ListSessions(r.Context(), a, runtimeapi.ListSessionsRequest{
		CWD:    strings.TrimSpace(r.URL.Query().Get("cwd")),
		Cursor: strings.TrimSpace(r.URL.Query().Get("cursor")),
	})
	if err != nil {
		return writeRuntimePreStreamError(w, r, err)
	}
	return httpjson.Write(w, http.StatusOK, result)
}

func (h *Handler) serveAgentTranscript(w http.ResponseWriter, r *http.Request, backend runtimeapi.Backend, a agentpkg.Agent, sessionID string, caps runtimeapi.Capabilities) error {
	loader, ok := backend.(runtimeapi.TranscriptLoader)
	if !ok || !caps.Sessions.Transcript {
		return writeRuntimePreStreamError(w, r, runtimeapi.NewError(runtimeapi.ErrorCapabilityNotSupported, "transcript loading is not supported on this route"))
	}
	result, err := loader.LoadTranscript(r.Context(), a, runtimeapi.TranscriptRequest{
		SessionID: sessionID,
		CWD:       strings.TrimSpace(r.URL.Query().Get("cwd")),
	})
	if err != nil {
		return writeRuntimePreStreamError(w, r, err)
	}
	return httpjson.Write(w, http.StatusOK, result)
}

// agentSSESink writes common-envelope turn events as SSE frames. Headers are
// written lazily on the first event so pre-stream failures still return real
// HTTP status codes. Unlike the legacy ACP/builtin sinks, it emits the
// runtime-neutral envelope unchanged: agent_id, run_id, sequence, and
// segment_index stay on the wire.
type agentSSESink struct {
	w       http.ResponseWriter
	flusher ResponseFlusher
	started bool
}

func newAgentSSESink(w http.ResponseWriter) *agentSSESink {
	return &agentSSESink{w: w, flusher: NewResponseFlusher(w)}
}

func (s *agentSSESink) emit(ev runtimeapi.TurnEvent) error {
	if !s.started {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("Connection", "keep-alive")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}
	name := strings.TrimSpace(ev.Event)
	if name == "" {
		name = runtimeapi.EventDelta
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
