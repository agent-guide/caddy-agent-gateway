package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/httplog"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/internal/statuserr"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"go.uber.org/zap"
)

type LLMApiHandler interface {
	Name() string
	MatchLLMApi(*http.Request) bool
	PrepareLLMApiRequest(*http.Request) (*PreparedLLMApiRequest, llmroutepkg.RequestRequirements, error)
	ServeLLMApi(http.ResponseWriter, *http.Request, provider.Provider, *PreparedLLMApiRequest) error
}

type ExecutionDisposition string

const (
	ExecutionProvider ExecutionDisposition = "provider"
	ExecutionLocal    ExecutionDisposition = "local"
)

type PreparedLLMApiRequest struct {
	Disposition      ExecutionDisposition
	Type             provider.LLMApiRequestType
	ChatRequest      *provider.ChatRequest
	EmbeddingRequest *provider.EmbeddingRequest
	ResponsesRequest *provider.ResponsesRequest
	StreamRequested  bool
	RawRequest       any
}

func (r *PreparedLLMApiRequest) Model() string {
	if r == nil {
		return ""
	}
	switch r.Type {
	case provider.LLMApiRequestTypeEmbedding:
		if r.EmbeddingRequest != nil {
			return r.EmbeddingRequest.Model
		}
	case provider.LLMApiRequestTypeResponses:
		if r.ResponsesRequest != nil {
			return r.ResponsesRequest.Model
		}
	case provider.LLMApiRequestTypeModels:
		return ""
	default:
		if r.ChatRequest != nil {
			return r.ChatRequest.Model
		}
	}
	return ""
}

func (r *PreparedLLMApiRequest) Stream() bool {
	if r == nil {
		return false
	}
	if r.Disposition != ExecutionProvider && r.Disposition != ExecutionLocal {
		return false
	}
	return r.StreamRequested
}

func (r *PreparedLLMApiRequest) IsValid() bool {
	if r == nil {
		return false
	}
	if r.Disposition == ExecutionLocal {
		return r.RawRequest != nil
	}
	if r.Disposition != ExecutionProvider {
		return false
	}
	switch r.Type {
	case provider.LLMApiRequestTypeEmbedding:
		return r.EmbeddingRequest != nil
	case provider.LLMApiRequestTypeResponses:
		return r.ResponsesRequest != nil
	case provider.LLMApiRequestTypeModels:
		return true
	case provider.LLMApiRequestTypeChat:
		return r.ChatRequest != nil
	default:
		return r.ChatRequest != nil || r.EmbeddingRequest != nil || r.ResponsesRequest != nil
	}
}

func (r *PreparedLLMApiRequest) SetModel(model string) {
	if r == nil {
		return
	}
	switch r.Type {
	case provider.LLMApiRequestTypeEmbedding:
		if r.EmbeddingRequest != nil {
			r.EmbeddingRequest.Model = model
		}
	case provider.LLMApiRequestTypeResponses:
		if r.ResponsesRequest != nil {
			r.ResponsesRequest.Model = model
		}
	default:
		if r.ChatRequest != nil {
			r.ChatRequest.Model = model
		}
	}
}

const StatusClientClosedRequest = 499

const (
	MaxLLMRequestBodyBytes = 16 << 20
	MaxMCPRequestBodyBytes = 4 << 20
	MaxACPRequestBodyBytes = 4 << 20
)

func RequestBodyErrorStatus(err error, fallback int) int {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return http.StatusRequestEntityTooLarge
	}
	return fallback
}

func WriteDispatchError(logger *zap.Logger, protocol, routeID, model string, status int, w http.ResponseWriter, r *http.Request, phase string, clientMessage string, err error, fields ...zap.Field) error {
	if status <= 0 {
		status = statuserr.StatusCode(err, http.StatusBadGateway)
	}
	logFields := []zap.Field{
		zap.String("protocol", protocol),
		zap.String("route_id", routeID),
		zap.String("model", model),
	}
	logFields = append(logFields, fields...)

	WriteHttpErrorLog(logger, w, r, status, phase, err, logFields...)
	if clientMessage == "" && err != nil {
		clientMessage = err.Error()
	}
	if r != nil {
		usage.SpanFromContext(r.Context()).AddAnnotation("error_type", dispatchErrorType(phase, err))
	}
	return httpjson.Error(w, status, clientMessage)
}

func WriteHttpErrorLog(logger *zap.Logger, w http.ResponseWriter, r *http.Request, status int, phase string, err error, fields ...zap.Field) {
	httplog.Error(logger, "http request failed", r, status,
		fmt.Errorf("%s error: %w", phase, err),
		fields...,
	)
}

func IsClientCanceled(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "context canceled")
}

func WriteProviderErrorLog(logger *zap.Logger, w http.ResponseWriter, r *http.Request, protocol string, model string, phase string, err error) (int, string) {
	status := statuserr.StatusCode(err, http.StatusBadGateway)
	errmsg := err.Error()
	if IsClientCanceled(err) {
		status = StatusClientClosedRequest
		errmsg = "client canceled request"
	}
	fields := provider.UpstreamErrorFields(err)
	fields = append(fields, zap.String("protocol", protocol))
	fields = append(fields, zap.String("model", model))
	httplog.Error(logger, "http request failed", r, status,
		fmt.Errorf("%s error: %w", phase, err),
		fields...,
	)
	errorType := "provider_request_failed"
	if strings.Contains(strings.ToLower(phase), "stream") {
		errorType = "provider_stream_failed"
	}
	if r != nil {
		usage.SpanFromContext(r.Context()).AddAnnotation("error_type", errorType)
	}
	return status, errmsg
}

func dispatchErrorType(phase string, err error) string {
	var gap *provider.RequirementGap
	var gaps *provider.RequirementGapsError
	if errors.As(err, &gap) || errors.As(err, &gaps) {
		return "protocol_requirement_rejected"
	}
	phase = strings.ToLower(phase)
	switch {
	case strings.Contains(phase, "virtual key"):
		return "virtual_key_rejected"
	case strings.Contains(phase, "prepare"):
		return "protocol_validation_failed"
	case strings.Contains(phase, "provider"):
		return "provider_not_configured"
	case strings.Contains(phase, "route"):
		return "route_not_found"
	case err != nil && IsClientCanceled(err):
		return "client_cancelled"
	default:
		return "internal_error"
	}
}

func requestLogFields(r *http.Request) []zap.Field {
	if r == nil {
		return nil
	}

	headerNames := make([]string, 0, len(r.Header))
	for name := range r.Header {
		headerNames = append(headerNames, strings.ToLower(name))
	}
	slices.Sort(headerNames)

	fields := []zap.Field{
		zap.String("http_method", r.Method),
		zap.String("request_path", r.URL.Path),
		zap.Bool("auth_header_present", strings.TrimSpace(r.Header.Get("Authorization")) != ""),
		zap.Bool("x_api_key_present", strings.TrimSpace(r.Header.Get("x-api-key")) != ""),
		zap.Strings("request_header_names", headerNames),
	}
	if r.URL.RawQuery != "" {
		fields = append(fields, zap.String("request_query", r.URL.RawQuery))
	}
	if sessionID := strings.TrimSpace(r.Header.Get("MCP-Session-Id")); sessionID != "" {
		fields = append(fields, zap.Bool("mcp_session_id_present", true))
	}
	return fields
}

func logRequestPhase(logger *zap.Logger, message string, r *http.Request, fields ...zap.Field) {
	if logger == nil {
		return
	}
	logFields := requestLogFields(r)
	if r != nil {
		if dims, ok := usage.DimensionsFromContext(r.Context()); ok {
			logFields = append(logFields,
				zap.String("agent_id", dims.AgentID),
				zap.String("runtime_type", dims.RuntimeType),
				zap.String("run_id", dims.RunID),
				zap.String("trace_id", dims.TraceID),
				zap.String("span_id", dims.SpanID),
				zap.String("parent_span_id", dims.ParentSpanID),
			)
		}
	}
	logFields = append(logFields, fields...)
	logger.Debug(message, logFields...)
}
