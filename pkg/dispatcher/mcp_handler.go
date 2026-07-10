package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/gateway/mcproute"
	"github.com/agent-guide/agent-gateway/pkg/gateway/routecore"
	basemcp "github.com/agent-guide/agent-gateway/pkg/mcp"
	mcpservice "github.com/agent-guide/agent-gateway/pkg/mcp/service"
	"github.com/agent-guide/agent-gateway/pkg/mcp/transport"
	"go.uber.org/zap"
)

func (h *Handler) dispatchMCP(w http.ResponseWriter, r *http.Request, next NextHandler, cfg routecore.AgentRouteConfig) error {
	if !h.mcpEnabled {
		return serveNextOrNotFound(next, w, r)
	}

	serviceManager := h.gateway.MCPServiceManager()
	if serviceManager == nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusServiceUnavailable, w, r, "dispatch mcp route", "mcp dispatcher is not configured", fmt.Errorf("mcp dispatcher is not configured"))
	}

	routeResolver := h.gateway.MCPRouteResolver()
	if routeResolver == nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusServiceUnavailable, w, r, "resolve mcp route", "route resolver is not configured", fmt.Errorf("route resolver is not configured"))
	}
	route, err := routeResolver.Resolve(r.Context(), cfg)
	if err != nil {
		status := statuserr.StatusCode(err, http.StatusBadGateway)
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", status, w, r, "resolve mcp route", "failed to resolve mcp route", err)
	}
	if route == nil {
		return WriteDispatchError(h.logger, string(cfg.Protocol), cfg.ID, "", http.StatusServiceUnavailable, w, r, "resolve mcp route", "mcp route is not configured", fmt.Errorf("mcp route %q is not configured", cfg.ID))
	}
	logRequestPhase(h.logger, "dispatcher: mcp route resolved", r,
		zap.String("route_id", route.ID),
		zap.String("service_id", route.ServiceID),
		zap.String("path_prefix", route.MatchPolicy.PathPrefix),
	)

	return h.dispatchJSONRPC(w, r, route)
}

func (h *Handler) dispatchJSONRPC(w http.ResponseWriter, r *http.Request, route *mcproute.MCPRoute) error {
	if route == nil {
		return WriteDispatchError(h.logger, string(routecore.RouteProtocolMCP), "", "", http.StatusServiceUnavailable, w, r, "dispatch json-rpc request", "mcp route is not configured", fmt.Errorf("mcp route is nil"))
	}
	if r.Method != http.MethodPost {
		return httpjson.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, MaxMCPRequestBodyBytes)
	}

	serviceManager := h.gateway.MCPServiceManager()
	if serviceManager == nil {
		return WriteDispatchError(h.logger, string(route.Protocol), route.ID, "", http.StatusServiceUnavailable, w, r, "dispatch json-rpc request", "mcp dispatcher is not configured", fmt.Errorf("mcp dispatcher is not configured"))
	}

	var msg transport.Message
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		return writeMCPError(w, nil, RequestBodyErrorStatus(err, http.StatusBadRequest), -32700, fmt.Sprintf("decode json-rpc request: %v", err))
	}
	if msg.JSONRPC == "" {
		msg.JSONRPC = "2.0"
	}
	if msg.JSONRPC != "2.0" {
		return writeMCPError(w, msg.ID, http.StatusBadRequest, -32600, "jsonrpc must be \"2.0\"")
	}
	if strings.TrimSpace(msg.Method) == "" {
		return writeMCPError(w, msg.ID, http.StatusBadRequest, -32600, "method is required")
	}
	logRequestPhase(h.logger, "dispatcher: mcp json-rpc request", r,
		zap.String("route_id", route.ID),
		zap.String("service_id", route.ServiceID),
		zap.String("jsonrpc_method", strings.TrimSpace(msg.Method)),
		zap.Bool("is_notification", isNotification(msg)),
		zap.Bool("has_request_id", msg.ID != nil),
	)
	usage.SpanFromContext(r.Context()).SetExtension(usage.MCPExtension{
		RequestID: jsonRPCIDString(msg.ID),
		ServiceID: route.ServiceID,
		Method:    strings.TrimSpace(msg.Method),
	})
	if isNotification(msg) {
		return h.dispatchNotification(w, r, route, msg)
	}
	var upstreamErr error
	requestCtx, finishRequest := h.beginRequest(r.Context(), route, msg)
	defer func() { finishRequest(upstreamErr) }()

	switch msg.Method {
	case "initialize":
		logRequestPhase(h.logger, "dispatcher: mcp initialize upstream", r,
			zap.String("route_id", route.ID),
			zap.String("service_id", route.ServiceID),
		)
		result, err := serviceManager.Initialize(requestCtx, route.ServiceID)
		if err != nil {
			upstreamErr = err
			return h.writeRequestError(w, r, route, msg, err)
		}
		logRequestPhase(h.logger, "dispatcher: mcp initialize completed", r,
			zap.String("route_id", route.ID),
			zap.String("service_id", route.ServiceID),
		)
		return writeMCPResult(w, msg.ID, result)
	case "ping":
		return writeMCPResult(w, msg.ID, map[string]any{})
	case "roots/list":
		return writeMCPResult(w, msg.ID, map[string]any{"roots": []any{}})
	case "tools/list":
		cursor, err := stringParamFromParams(msg.Params, "cursor", false)
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		logRequestPhase(h.logger, "dispatcher: mcp tools/list upstream", r,
			zap.String("route_id", route.ID),
			zap.String("service_id", route.ServiceID),
			zap.String("cursor", cursor),
		)
		result, err := serviceManager.ListToolsPage(requestCtx, route.ServiceID, cursor)
		if err != nil {
			upstreamErr = err
			return h.writeRequestError(w, r, route, msg, err)
		}
		return writeMCPResult(w, msg.ID, result)
	case "resources/list":
		cursor, err := stringParamFromParams(msg.Params, "cursor", false)
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		logRequestPhase(h.logger, "dispatcher: mcp resources/list upstream", r,
			zap.String("route_id", route.ID),
			zap.String("service_id", route.ServiceID),
			zap.String("cursor", cursor),
		)
		result, err := serviceManager.ListResourcesPage(requestCtx, route.ServiceID, cursor)
		if err != nil {
			upstreamErr = err
			return h.writeRequestError(w, r, route, msg, err)
		}
		return writeMCPResult(w, msg.ID, result)
	case "resources/templates/list":
		cursor, err := stringParamFromParams(msg.Params, "cursor", false)
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		logRequestPhase(h.logger, "dispatcher: mcp resource templates/list upstream", r,
			zap.String("route_id", route.ID),
			zap.String("service_id", route.ServiceID),
			zap.String("cursor", cursor),
		)
		result, err := serviceManager.ListResourceTemplatesPage(requestCtx, route.ServiceID, cursor)
		if err != nil {
			upstreamErr = err
			return h.writeRequestError(w, r, route, msg, err)
		}
		return writeMCPResult(w, msg.ID, result)
	case "prompts/list":
		cursor, err := stringParamFromParams(msg.Params, "cursor", false)
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		logRequestPhase(h.logger, "dispatcher: mcp prompts/list upstream", r,
			zap.String("route_id", route.ID),
			zap.String("service_id", route.ServiceID),
			zap.String("cursor", cursor),
		)
		result, err := serviceManager.ListPromptsPage(requestCtx, route.ServiceID, cursor)
		if err != nil {
			upstreamErr = err
			return h.writeRequestError(w, r, route, msg, err)
		}
		return writeMCPResult(w, msg.ID, result)
	case "tools/call":
		params, err := objectParams(msg.Params)
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		name, err := requiredStringParam(params, "name")
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		args, err := mapParam(params, "arguments")
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		argCount := len(args)
		ext := usage.MCPExtension{
			ToolName:     strings.TrimSpace(name),
			ArgCount:     usage.Int(argCount),
			ResultStatus: "success",
		}
		if captureMCPToolArgs(r.Context(), serviceManager, route.ServiceID) {
			if data, err := json.Marshal(args); err == nil {
				ext.ToolArgsJSON = string(data)
			}
		}
		usage.SpanFromContext(r.Context()).SetExtension(ext)
		logRequestPhase(h.logger, "dispatcher: mcp tools/call upstream", r,
			zap.String("route_id", route.ID),
			zap.String("service_id", route.ServiceID),
			zap.String("tool_name", strings.TrimSpace(name)),
		)
		progressCh := make(chan mcpservice.UpstreamProgress, 64)
		result, err := serviceManager.CallTool(requestCtx, route.ServiceID, strings.TrimSpace(name), args, progressCh)
		close(progressCh)
		h.drainUpstreamProgress(route, progressCh)
		if err != nil {
			upstreamErr = err
			return h.writeRequestError(w, r, route, msg, err)
		}
		return writeMCPResult(w, msg.ID, result)
	case "resources/read":
		params, err := objectParams(msg.Params)
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		uri, err := requiredStringParam(params, "uri")
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		usage.SpanFromContext(r.Context()).SetExtension(usage.MCPExtension{
			ResourceURI:  strings.TrimSpace(uri),
			ResultStatus: "success",
		})
		logRequestPhase(h.logger, "dispatcher: mcp resources/read upstream", r,
			zap.String("route_id", route.ID),
			zap.String("service_id", route.ServiceID),
			zap.String("resource_uri", strings.TrimSpace(uri)),
		)
		progressCh := make(chan mcpservice.UpstreamProgress, 64)
		result, err := serviceManager.ReadResource(requestCtx, route.ServiceID, strings.TrimSpace(uri), progressCh)
		close(progressCh)
		h.drainUpstreamProgress(route, progressCh)
		if err != nil {
			upstreamErr = err
			return h.writeRequestError(w, r, route, msg, err)
		}
		return writeMCPResult(w, msg.ID, result)
	case "prompts/get":
		params, err := objectParams(msg.Params)
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		name, err := requiredStringParam(params, "name")
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		usage.SpanFromContext(r.Context()).SetExtension(usage.MCPExtension{
			PromptName:   strings.TrimSpace(name),
			ResultStatus: "success",
		})
		args, err := mapParam(params, "arguments")
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		logRequestPhase(h.logger, "dispatcher: mcp prompts/get upstream", r,
			zap.String("route_id", route.ID),
			zap.String("service_id", route.ServiceID),
			zap.String("prompt_name", strings.TrimSpace(name)),
		)
		progressCh := make(chan mcpservice.UpstreamProgress, 64)
		result, err := serviceManager.GetPrompt(requestCtx, route.ServiceID, strings.TrimSpace(name), args, progressCh)
		close(progressCh)
		h.drainUpstreamProgress(route, progressCh)
		if err != nil {
			upstreamErr = err
			return h.writeRequestError(w, r, route, msg, err)
		}
		return writeMCPResult(w, msg.ID, result)
	case "completion/complete":
		params, err := objectParams(msg.Params)
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		ref, argument, args, err := decodeCompletionParams(params)
		if err != nil {
			return writeMCPError(w, msg.ID, http.StatusBadRequest, -32602, err.Error())
		}
		usage.SpanFromContext(r.Context()).SetExtension(usage.MCPExtension{
			CompletionRefType:  strings.TrimSpace(ref.Type),
			CompletionArgument: strings.TrimSpace(argument.Name),
			ArgCount:           usage.Int(len(args)),
			ResultStatus:       "success",
		})
		logRequestPhase(h.logger, "dispatcher: mcp completion upstream", r,
			zap.String("route_id", route.ID),
			zap.String("service_id", route.ServiceID),
			zap.String("completion_ref_type", ref.Type),
			zap.String("completion_argument_name", argument.Name),
		)
		result, err := serviceManager.Complete(requestCtx, route.ServiceID, ref, argument, args)
		if err != nil {
			upstreamErr = err
			return h.writeRequestError(w, r, route, msg, err)
		}
		return writeMCPResult(w, msg.ID, result)
	default:
		return writeMCPError(w, msg.ID, http.StatusNotImplemented, -32601, fmt.Sprintf("mcp method %q is not implemented by this dispatcher", msg.Method))
	}
}

func (h *Handler) dispatchNotification(w http.ResponseWriter, r *http.Request, route *mcproute.MCPRoute, msg transport.Message) error {
	switch msg.Method {
	case "notifications/initialized":
		serviceManager := h.gateway.MCPServiceManager()
		if serviceManager == nil {
			return WriteDispatchError(h.logger, string(route.Protocol), route.ID, "", http.StatusServiceUnavailable, w, r, "dispatch mcp notification", "mcp dispatcher is not configured", fmt.Errorf("mcp dispatcher is not configured"))
		}
		if _, err := serviceManager.Initialize(r.Context(), route.ServiceID); err != nil {
			return httpjson.Error(w, http.StatusBadGateway, err.Error())
		}
	case "notifications/cancelled":
		cancelled, err := h.handleCancelledNotification(route, msg.Params)
		if err != nil {
			return httpjson.Error(w, http.StatusBadRequest, err.Error())
		}
		if cancelled {
			w.WriteHeader(http.StatusAccepted)
			return nil
		}
	case "notifications/progress":
		if err := h.handleProgressNotification(route, msg.Params); err != nil {
			return httpjson.Error(w, http.StatusBadRequest, err.Error())
		}
	case "notifications/message":
	default:
	}
	w.WriteHeader(http.StatusAccepted)
	return nil
}

func (h *Handler) beginRequest(parent context.Context, route *mcproute.MCPRoute, msg transport.Message) (context.Context, func(error)) {
	registry := h.mcpRuntimeRegistry()
	if registry == nil || route == nil || msg.ID == nil {
		return parent, func(error) {}
	}
	return registry.BeginRequest(
		parent,
		resolvedRouteID(route),
		msg.ID,
		strings.TrimSpace(msg.Method),
		extractProgressToken(msg.Params),
	)
}

// drainUpstreamProgress reads all buffered upstream progress notifications and
// stores them in the runtime registry so they are visible via the admin API.
func (h *Handler) drainUpstreamProgress(route *mcproute.MCPRoute, ch <-chan mcpservice.UpstreamProgress) {
	registry := h.mcpRuntimeRegistry()
	if registry == nil || route == nil {
		return
	}
	routeID := resolvedRouteID(route)
	for n := range ch {
		registry.StoreProgress(routeID, n.ProgressToken, n.Progress, n.Total, n.Message)
	}
}

func (h *Handler) handleCancelledNotification(route *mcproute.MCPRoute, params any) (bool, error) {
	object, err := objectParams(params)
	if err != nil {
		return false, err
	}
	requestID, ok := object["requestId"]
	if !ok || requestID == nil {
		return false, fmt.Errorf("requestId is required")
	}
	reason, err := optionalStringParam(object, "reason")
	if err != nil {
		return false, err
	}
	return h.cancelRequest(route, requestID, reason)
}

func (h *Handler) handleProgressNotification(route *mcproute.MCPRoute, params any) error {
	object, err := objectParams(params)
	if err != nil {
		return err
	}
	token, ok := object["progressToken"]
	if !ok || token == nil {
		return fmt.Errorf("progressToken is required")
	}
	progress, err := numericParam(object, "progress", true)
	if err != nil {
		return err
	}
	total, err := numericParamPointer(object, "total")
	if err != nil {
		return err
	}
	message, err := optionalStringParam(object, "message")
	if err != nil {
		return err
	}
	h.storeProgressNotification(route, token, progress, total, message)
	return nil
}

func (h *Handler) cancelRequest(route *mcproute.MCPRoute, requestID any, reason string) (bool, error) {
	registry := h.mcpRuntimeRegistry()
	if registry == nil || route == nil {
		return false, nil
	}
	return registry.Cancel(resolvedRouteID(route), requestID, reason)
}

func (h *Handler) storeProgressNotification(route *mcproute.MCPRoute, token any, progress float64, total *float64, message string) {
	if registry := h.mcpRuntimeRegistry(); registry != nil && route != nil {
		registry.StoreProgress(resolvedRouteID(route), token, progress, total, message)
	}
}

func (h *Handler) writeRequestError(w http.ResponseWriter, r *http.Request, route *mcproute.MCPRoute, msg transport.Message, err error) error {
	if errors.Is(err, context.Canceled) {
		usage.SpanFromContext(r.Context()).SetExtension(usage.MCPExtension{
			ResultStatus: "cancelled",
			Cancelled:    usage.Bool(true),
		})
		reason := h.cancelReason(route, msg.ID)
		message := "request cancelled"
		if reason != "" {
			message += ": " + reason
		}
		return writeMCPErrorWithData(w, msg.ID, http.StatusOK, -32000, message, map[string]any{
			"cancelled": true,
			"method":    msg.Method,
		})
	}
	usage.SpanFromContext(r.Context()).SetExtension(usage.MCPExtension{ResultStatus: "error"})
	return writeMCPError(w, msg.ID, http.StatusBadGateway, -32002, err.Error())
}

func (h *Handler) cancelReason(route *mcproute.MCPRoute, requestID any) string {
	if registry := h.mcpRuntimeRegistry(); registry != nil && route != nil {
		return registry.CancelReason(resolvedRouteID(route), requestID)
	}
	return ""
}

func writeMCPResult(w http.ResponseWriter, id any, result any) error {
	return httpjson.Write(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func isNotification(msg transport.Message) bool {
	return msg.ID == nil && strings.HasPrefix(strings.TrimSpace(msg.Method), "notifications/")
}

func objectParams(src any) (map[string]any, error) {
	if src == nil {
		return map[string]any{}, nil
	}
	params, ok := src.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("params must be an object")
	}
	return params, nil
}

func stringParamFromParams(src any, key string, required bool) (string, error) {
	params, err := objectParams(src)
	if err != nil {
		return "", err
	}
	if required {
		return requiredStringParam(params, key)
	}
	if _, ok := params[key]; !ok {
		return "", nil
	}
	return optionalStringParam(params, key)
}

func requiredStringParam(params map[string]any, key string) (string, error) {
	value, err := optionalStringParam(params, key)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return value, nil
}

func optionalStringParam(params map[string]any, key string) (string, error) {
	raw, ok := params[key]
	if !ok || raw == nil {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return strings.TrimSpace(value), nil
}

func mapParam(params map[string]any, key string) (map[string]any, error) {
	raw, ok := params[key]
	if !ok || raw == nil {
		return nil, nil
	}
	value, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", key)
	}
	return value, nil
}

func numericParam(params map[string]any, key string, required bool) (float64, error) {
	raw, ok := params[key]
	if !ok || raw == nil {
		if required {
			return 0, fmt.Errorf("%s is required", key)
		}
		return 0, nil
	}
	value, ok := raw.(float64)
	if !ok {
		return 0, fmt.Errorf("%s must be a number", key)
	}
	return value, nil
}

func numericParamPointer(params map[string]any, key string) (*float64, error) {
	if _, ok := params[key]; !ok {
		return nil, nil
	}
	value, err := numericParam(params, key, false)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func extractProgressToken(params any) any {
	object, err := objectParams(params)
	if err != nil {
		return nil
	}
	meta, err := mapParam(object, "_meta")
	if err != nil || meta == nil {
		return nil
	}
	token, ok := meta["progressToken"]
	if !ok || token == nil {
		return nil
	}
	return token
}

func resolvedRouteID(route *mcproute.MCPRoute) string {
	if route == nil {
		return ""
	}
	routeID := strings.TrimSpace(route.ID)
	if routeID != "" {
		return routeID
	}
	return "mcp:" + strings.TrimSpace(route.ServiceID)
}

func decodeCompletionParams(params map[string]any) (basemcp.CompletionReference, basemcp.CompletionArgument, map[string]string, error) {
	refMap, err := mapParam(params, "ref")
	if err != nil {
		return basemcp.CompletionReference{}, basemcp.CompletionArgument{}, nil, err
	}
	if refMap == nil {
		return basemcp.CompletionReference{}, basemcp.CompletionArgument{}, nil, fmt.Errorf("ref is required")
	}
	refType, err := requiredStringParam(refMap, "type")
	if err != nil {
		return basemcp.CompletionReference{}, basemcp.CompletionArgument{}, nil, fmt.Errorf("ref: %w", err)
	}
	ref := basemcp.CompletionReference{Type: refType}
	switch refType {
	case "ref/prompt":
		ref.Name, err = requiredStringParam(refMap, "name")
		if err != nil {
			return basemcp.CompletionReference{}, basemcp.CompletionArgument{}, nil, fmt.Errorf("ref: %w", err)
		}
	case "ref/resource":
		ref.URI, err = requiredStringParam(refMap, "uri")
		if err != nil {
			return basemcp.CompletionReference{}, basemcp.CompletionArgument{}, nil, fmt.Errorf("ref: %w", err)
		}
	default:
		return basemcp.CompletionReference{}, basemcp.CompletionArgument{}, nil, fmt.Errorf("ref.type %q is not supported", refType)
	}

	argumentMap, err := mapParam(params, "argument")
	if err != nil {
		return basemcp.CompletionReference{}, basemcp.CompletionArgument{}, nil, err
	}
	if argumentMap == nil {
		return basemcp.CompletionReference{}, basemcp.CompletionArgument{}, nil, fmt.Errorf("argument is required")
	}
	argumentName, err := requiredStringParam(argumentMap, "name")
	if err != nil {
		return basemcp.CompletionReference{}, basemcp.CompletionArgument{}, nil, fmt.Errorf("argument: %w", err)
	}
	argumentValue, err := optionalStringParam(argumentMap, "value")
	if err != nil {
		return basemcp.CompletionReference{}, basemcp.CompletionArgument{}, nil, fmt.Errorf("argument: %w", err)
	}

	argumentsMap, err := mapParam(params, "arguments")
	if err != nil {
		return basemcp.CompletionReference{}, basemcp.CompletionArgument{}, nil, err
	}
	arguments := make(map[string]string, len(argumentsMap))
	for key, raw := range argumentsMap {
		value, ok := raw.(string)
		if !ok {
			return basemcp.CompletionReference{}, basemcp.CompletionArgument{}, nil, fmt.Errorf("arguments.%s must be a string", key)
		}
		arguments[key] = value
	}

	return ref, basemcp.CompletionArgument{
		Name:  argumentName,
		Value: argumentValue,
	}, arguments, nil
}

func writeMCPError(w http.ResponseWriter, id any, status int, code int, message string) error {
	return writeMCPErrorWithData(w, id, status, code, message, nil)
}

func writeMCPErrorWithData(w http.ResponseWriter, id any, status int, code int, message string, data any) error {
	return httpjson.Write(w, status, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
			"data":    data,
		},
	})
}

func jsonRPCIDString(id any) string {
	if id == nil {
		return ""
	}
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(data)
	}
}

func captureMCPToolArgs(ctx context.Context, manager *mcpservice.Manager, serviceID string) bool {
	if manager == nil || strings.TrimSpace(serviceID) == "" {
		return false
	}
	cfg, err := manager.Get(ctx, serviceID)
	if err != nil {
		return false
	}
	return cfg.Audit.CaptureToolArgs
}
