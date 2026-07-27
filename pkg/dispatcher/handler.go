package dispatcher

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpcapture"
	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/internal/statuserr"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	agentroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/agentroute"
	builtinroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/builtinroute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	virtualkeypkg "github.com/agent-guide/agent-gateway/pkg/gateway/virtualkey"
	mcpruntime "github.com/agent-guide/agent-gateway/pkg/mcp/runtime"
	"go.uber.org/zap"
)

// NextHandler is the small subset of a middleware next handler needed by the dispatcher.
type NextHandler interface {
	ServeHTTP(http.ResponseWriter, *http.Request) error
}

// Handler dispatches gateway requests to the route-selected LLM API handler.
type Handler struct {
	apiHandlers    map[string]LLMApiHandler
	gateway        *gateway.AgentGateway
	logger         *zap.Logger
	mcpEnabled     bool
	acpEnabled     bool
	builtinEnabled bool
	agentEnabled   bool
}

type HandlerOptions struct {
	EnableMCP     bool
	EnableACP     bool
	EnableBuiltin bool
	// EnableAgent turns on the unified kind=agent ingress dispatch. It is
	// internal during M4 (tests/fixtures only); the Caddyfile/standalone
	// enablement switch arrives with the M5 public cutover.
	EnableAgent bool
}

// NewHandler constructs a runtime dispatcher handler.
func NewHandler(agentGateway *gateway.AgentGateway, apiHandlers map[string]LLMApiHandler, logger *zap.Logger, opts HandlerOptions) *Handler {
	if logger == nil {
		logger = zap.NewNop()
	}
	if apiHandlers == nil {
		apiHandlers = map[string]LLMApiHandler{}
	}
	handler := &Handler{
		apiHandlers:    apiHandlers,
		gateway:        agentGateway,
		logger:         logger,
		mcpEnabled:     opts.EnableMCP,
		acpEnabled:     opts.EnableACP,
		builtinEnabled: opts.EnableBuiltin,
		agentEnabled:   opts.EnableAgent,
	}
	return handler
}

// Validate verifies the dispatcher has at least one configured ingress protocol handler.
func (h *Handler) Validate() error {
	if h == nil || (len(h.apiHandlers) == 0 && !h.mcpEnabled && !h.acpEnabled && !h.builtinEnabled && !h.agentEnabled) {
		return fmt.Errorf("agent_route_dispatcher requires at least one llm_api, mcp, acp, builtin, or agent")
	}
	return nil
}

// ServeHTTP implements http.Handler. Requests not handled by the dispatcher receive 404.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.Dispatch(w, r, nil); err != nil {
		return
	}
}

// Dispatch handles a request and optionally delegates unmatched requests to next.
func (h *Handler) Dispatch(w http.ResponseWriter, r *http.Request, next NextHandler) error {
	if h == nil || h.gateway == nil {
		return WriteDispatchError(loggerOrNop(h), "", "", "", http.StatusServiceUnavailable, w, r, "dispatch request", "agent gateway is not configured", fmt.Errorf("agent gateway is not configured"))
	}

	logRequestPhase(h.logger, "dispatcher: received request", r)

	cfg, err := h.gateway.Match(r.Context(), r)
	if err != nil {
		status := statuserr.StatusCode(err, http.StatusBadGateway)
		return WriteDispatchError(h.logger, "", "", "", status, w, r, "resolve matched route", "failed to resolve matched route", err)
	}
	if cfg.ID == "" {
		h.logger.Debug("no route matched",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
		)
		return serveNextOrNotFound(next, w, r)
	}
	rec := httpcapture.NewResponseRecorder(w)
	traceCtx := extractTraceContext(r)
	writeTraceHeaders(rec, traceCtx)
	if maxDepth := h.gateway.UsageConfig().MaxAgentDepth; maxDepth > 0 && traceCtx.AgentDepth >= maxDepth {
		span, _ := h.gateway.UsageObserver().Begin(r.Context(), usage.InteractionDimensions{
			TraceID: traceCtx.TraceID, SpanID: traceCtx.SpanID, ParentSpanID: traceCtx.ParentSpanID, AgentDepth: traceCtx.AgentDepth,
			RouteID: cfg.ID, RouteKind: string(cfg.Kind), RouteProtocol: string(cfg.Protocol),
		})
		defer span.Finish(usage.InteractionOutcome{Success: false, StatusCode: http.StatusForbidden, ErrorType: "agent_depth_exceeded"})
		return httpjson.Error(rec, http.StatusForbidden, "agent depth limit exceeded")
	}
	if cfg.Disabled {
		span, _ := h.gateway.UsageObserver().Begin(r.Context(), usage.InteractionDimensions{
			TraceID: traceCtx.TraceID, SpanID: traceCtx.SpanID, ParentSpanID: traceCtx.ParentSpanID, AgentDepth: traceCtx.AgentDepth,
			RouteID: cfg.ID, RouteKind: string(cfg.Kind), RouteProtocol: string(cfg.Protocol),
		})
		defer span.Finish(usage.InteractionOutcome{Success: false, StatusCode: http.StatusForbidden, ErrorType: "route_disabled"})
		return httpjson.Error(rec, http.StatusForbidden, fmt.Sprintf("route %q is disabled", cfg.ID))
	}

	logRequestPhase(h.logger, "dispatcher: matched route", r,
		zap.String("route_id", cfg.ID),
		zap.String("route_kind", string(cfg.Kind)),
		zap.String("route_protocol", string(cfg.Protocol)),
		zap.String("path_prefix", cfg.MatchPolicy.PathPrefix),
		zap.Bool("require_virtual_key", cfg.AuthPolicy.RequireVirtualKey),
	)

	virtualKey, err := h.gateway.ResolveVirtualKey(r.Context(), r, cfg)
	if err != nil {
		span, _ := h.gateway.UsageObserver().Begin(r.Context(), usage.InteractionDimensions{
			TraceID: traceCtx.TraceID, SpanID: traceCtx.SpanID, ParentSpanID: traceCtx.ParentSpanID, AgentDepth: traceCtx.AgentDepth,
			RouteID: cfg.ID, RouteKind: string(cfg.Kind), RouteProtocol: string(cfg.Protocol),
		})
		defer span.Finish(usage.InteractionOutcome{Success: false, StatusCode: statuserr.StatusCode(err, http.StatusUnauthorized), ErrorType: "virtual_key_rejected"})
		return WriteDispatchError(h.logger, "", cfg.ID, "", 0, rec, r, "resolve virtual key", "", err,
			zap.Bool("require_virtual_key", cfg.AuthPolicy.RequireVirtualKey),
			zap.Bool("auth_header_present", strings.TrimSpace(r.Header.Get("Authorization")) != ""),
			zap.Bool("x_api_key_present", strings.TrimSpace(r.Header.Get("x-api-key")) != ""),
			zap.String("route_kind", string(cfg.Kind)),
			zap.String("route_protocol", string(cfg.Protocol)),
		)
	}
	logRequestPhase(h.logger, "dispatcher: virtual key accepted", r,
		zap.String("route_id", cfg.ID),
		zap.String("route_kind", string(cfg.Kind)),
		zap.String("route_protocol", string(cfg.Protocol)),
	)
	virtualKeyID := ""
	if virtualKey != nil {
		virtualKeyID = virtualKey.ID
	}
	dims := usage.InteractionDimensions{
		TraceID: traceCtx.TraceID, SpanID: traceCtx.SpanID, ParentSpanID: traceCtx.ParentSpanID, AgentDepth: traceCtx.AgentDepth,
		RouteID: cfg.ID, RouteKind: string(cfg.Kind), RouteProtocol: string(cfg.Protocol), VirtualKeyID: virtualKeyID,
	}
	// Builtin and agent routes carry their target agent in the route config;
	// stamp it explicitly so their attribution never depends on the
	// route -> agent index (§5.7.6, unified-agent-runtime §6.4).
	if cfg.Kind == routecore.RouteKindBuiltin {
		if agentID, err := builtinroutepkg.DecodeTargetAgentID(cfg.TargetPolicy); err == nil && agentID != "" {
			dims.AgentID = agentID
		}
	}
	if cfg.Kind == routecore.RouteKindAgent {
		if agentID, err := agentroutepkg.DecodeTargetAgentID(cfg.TargetPolicy); err == nil && agentID != "" {
			dims.AgentID = agentID
		}
	}
	span, spanCtx := h.gateway.UsageObserver().Begin(r.Context(), dims)
	spanCtx = bindAgentRuntimeIdentity(spanCtx, span, cfg.Kind)
	r = r.WithContext(spanCtx)
	defer func() {
		status := rec.StatusCode()
		success := status < 400
		span.Finish(usage.InteractionOutcome{Success: success, StatusCode: status})
	}()

	if virtualKey != nil {
		dimension, err := rateLimitDimension(cfg.Kind)
		if err != nil {
			return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusServiceUnavailable, rec, r, "rate limit admission", "route kind is not configured", err)
		}
		admission, err := h.gateway.VirtualKeyManager().Admit(*virtualKey, dimension)
		if err != nil {
			return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusInternalServerError, rec, r, "rate limit admission", "failed to apply virtual key rate limit", err)
		}
		if !admission.Allowed {
			span.AddAnnotation("error_type", "rate_limited")
			rec.Header().Set("Retry-After", strconv.Itoa(admission.RetryAfterSeconds))
			h.logger.Warn("virtual key rate limit exceeded",
				zap.String("virtual_key_id", virtualKey.ID),
				zap.String("route_id", cfg.ID),
				zap.String("route_kind", string(cfg.Kind)),
				zap.String("rate_limit_dimension", string(dimension)),
				zap.Int("requests_per_minute", admission.RequestsPerMinute),
				zap.Int("burst", admission.Burst),
				zap.Int("retry_after_seconds", admission.RetryAfterSeconds),
			)
			return httpjson.Error(rec, http.StatusTooManyRequests, "rate limit exceeded")
		}
	}

	switch cfg.Kind {
	case routecore.RouteKindLLM:
		return h.dispatchLLM(rec, r, next, cfg)
	case routecore.RouteKindMCP:
		return h.dispatchMCP(rec, r, next, cfg)
	case routecore.RouteKindACP:
		return h.dispatchACP(rec, r, next, cfg)
	case routecore.RouteKindBuiltin:
		return h.dispatchBuiltin(rec, r, next, cfg)
	case routecore.RouteKindAgent:
		return h.dispatchAgent(rec, r, next, cfg)
	default:
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusServiceUnavailable, rec, r, "dispatch route", "route kind is not configured", fmt.Errorf("route %q kind %q is not configured", cfg.ID, cfg.Kind))
	}
}

// bindAgentRuntimeIdentity adds a runtime type only after Agent attribution is
// known. In particular, an unbound legacy ACP route is direct non-Agent
// traffic and must keep agent_id, run_id, and runtime_type empty.
func bindAgentRuntimeIdentity(ctx context.Context, span usage.InteractionSpan, kind routecore.RouteKind) context.Context {
	dims, ok := usage.DimensionsFromContext(ctx)
	if !ok || dims.AgentID == "" {
		return ctx
	}
	switch kind {
	case routecore.RouteKindACP:
		dims.RuntimeType = agentpkg.RuntimeTypeACP
	case routecore.RouteKindBuiltin:
		dims.RuntimeType = agentpkg.RuntimeTypeBuiltin
	default:
		return ctx
	}
	span.SetExtension(usage.CommonExtension{AgentID: dims.AgentID, RuntimeType: dims.RuntimeType})
	return usage.ContextWithDimensions(ctx, dims)
}

func rateLimitDimension(kind routecore.RouteKind) (virtualkeypkg.RateLimitDimension, error) {
	switch kind {
	case routecore.RouteKindLLM:
		return virtualkeypkg.RateLimitDimensionLLM, nil
	case routecore.RouteKindMCP:
		return virtualkeypkg.RateLimitDimensionMCP, nil
	case routecore.RouteKindACP:
		return virtualkeypkg.RateLimitDimensionACP, nil
	case routecore.RouteKindBuiltin:
		return virtualkeypkg.RateLimitDimensionBuiltin, nil
	case routecore.RouteKindAgent:
		return virtualkeypkg.RateLimitDimensionAgent, nil
	default:
		return "", fmt.Errorf("unsupported route kind %q", kind)
	}
}

func (h *Handler) dispatchLLM(w http.ResponseWriter, r *http.Request, next NextHandler, cfg routecore.AgentRouteConfig) error {
	routeResolver := h.gateway.LLMRouteResolver()
	if routeResolver == nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusServiceUnavailable, w, r, "resolve llm route", "llm route resolver is not configured", fmt.Errorf("llm route resolver is not configured"))
	}
	route, err := routeResolver.Resolve(r.Context(), cfg)
	if err != nil {
		status := statuserr.StatusCode(err, http.StatusBadGateway)
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", status, w, r, "resolve llm route", "failed to resolve llm route", err)
	}
	logRequestPhase(h.logger, "dispatcher: llm route resolved", r,
		zap.String("route_id", route.ID),
		zap.String("route_protocol", string(route.Protocol)),
		zap.String("path_prefix", route.MatchPolicy.PathPrefix),
		zap.Bool("uses_logical_model", route.UsesLogicalModel()),
	)

	apiName := strings.TrimSpace(string(route.Protocol))
	apiHandler := h.apiHandlers[apiName]
	if apiHandler == nil {
		return WriteDispatchError(h.logger, apiName, route.ID, "", http.StatusServiceUnavailable, w, r, "dispatch llm route", "llm route protocol is not configured", fmt.Errorf("llm route %q protocol %q is not configured", route.ID, apiName))
	}

	rewritten := RewriteLLMRoutePath(r, route.MatchPolicy.PathPrefix)
	if !apiHandler.MatchLLMApi(rewritten) {
		return serveNextOrNotFound(next, w, r)
	}
	if rewritten.Body != nil {
		rewritten.Body = http.MaxBytesReader(w, rewritten.Body, MaxLLMRequestBodyBytes)
	}
	logRequestPhase(h.logger, "dispatcher: llm api matched", rewritten,
		zap.String("route_id", route.ID),
		zap.String("llm_api", apiHandler.Name()),
	)

	prepared, requestRequirements, err := apiHandler.PrepareLLMApiRequest(rewritten)
	if err != nil {
		model := ""
		if prepared != nil {
			model = prepared.Model()
		}
		return WriteDispatchError(h.logger, apiHandler.Name(), route.ID, model, RequestBodyErrorStatus(err, 0), w, rewritten, "prepare request", "", err)
	}
	if !prepared.IsValid() {
		return WriteDispatchError(h.logger, apiHandler.Name(), route.ID, "", 0, w, rewritten, "prepare request", "", fmt.Errorf("llm api handler returned invalid prepared request"))
	}
	logRequestPhase(h.logger, "dispatcher: llm request prepared", rewritten,
		zap.String("route_id", route.ID),
		zap.String("llm_api", apiHandler.Name()),
		zap.String("request_type", string(prepared.Type)),
		zap.String("requested_model", prepared.Model()),
		zap.Bool("stream", prepared.Stream()),
		zap.Bool("require_streaming", requestRequirements.RequireStreaming),
	)

	routedProvider, err := h.gateway.NewRoutedProvider(route, requestRequirements)
	if err != nil {
		return WriteDispatchError(h.logger, apiHandler.Name(), route.ID, prepared.Model(), 0, w, rewritten, "resolve provider", "", err)
	}
	logRequestPhase(h.logger, "dispatcher: llm provider resolved", rewritten,
		zap.String("route_id", route.ID),
		zap.String("llm_api", apiHandler.Name()),
		zap.String("requested_model", prepared.Model()),
	)

	return apiHandler.ServeLLMApi(w, rewritten, routedProvider, prepared)
}

// RewriteLLMRoutePath returns a cloned request with the matched LLM route prefix stripped.
func RewriteLLMRoutePath(r *http.Request, prefix string) *http.Request {
	rewritten := r.Clone(r.Context())
	urlCopy := *r.URL
	rewritten.URL = &urlCopy
	if prefix == "" || !strings.HasPrefix(rewritten.URL.Path, prefix) {
		return rewritten
	}

	path := strings.TrimPrefix(rewritten.URL.Path, prefix)
	if path == "" {
		path = "/"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	rewritten.URL.Path = path
	rewritten.URL.RawPath = ""
	return rewritten
}

func serveNextOrNotFound(next NextHandler, w http.ResponseWriter, r *http.Request) error {
	// The dispatcher is not handling this request; drop its interaction span
	// so passthrough requests do not emit usage events.
	usage.SpanFromContext(r.Context()).Discard()
	if next != nil {
		return next.ServeHTTP(w, r)
	}
	http.NotFound(w, r)
	return nil
}

func loggerOrNop(h *Handler) *zap.Logger {
	if h != nil && h.logger != nil {
		return h.logger
	}
	return zap.NewNop()
}

func (h *Handler) mcpRuntimeRegistry() *mcpruntime.Registry {
	if h == nil || h.gateway == nil {
		return nil
	}
	return h.gateway.MCPRuntimeRegistry()
}
