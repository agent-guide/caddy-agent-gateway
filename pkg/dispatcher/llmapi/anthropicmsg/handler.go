package anthropicmsg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	einojsonschema "github.com/eino-contrib/jsonschema"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/internal/statuserr"
	dispatcher "github.com/agent-guide/agent-gateway/pkg/dispatcher"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

// Handler handles Anthropic-format API requests (/v1/messages).
type Handler struct {
	logger              *zap.Logger
	name                string
	estimateCountTokens bool
}

// Profile declares the observable ingress differences layered over the shared
// Anthropic Messages protocol core.
type Profile struct {
	Name                string
	EstimateCountTokens bool
}

func StandardProfile() Profile {
	return Profile{Name: "anthropic"}
}

func ClaudeCodeProfile() Profile {
	return Profile{Name: "cc", EstimateCountTokens: true}
}

// NewHandler creates the shared Messages handler for one explicit profile.
func NewHandler(profile Profile) *Handler {
	name := strings.TrimSpace(profile.Name)
	if name == "" {
		name = "anthropic"
	}
	return &Handler{
		logger:              zap.NewNop(),
		name:                name,
		estimateCountTokens: profile.EstimateCountTokens,
	}
}

func (h *Handler) Name() string {
	if h == nil || strings.TrimSpace(h.name) == "" {
		return "anthropic"
	}
	return h.name
}

// SetLogger configures the handler logger.
func (h *Handler) SetLogger(logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}
	h.logger = logger
}

func (h *Handler) MatchLLMApi(r *http.Request) bool {
	return r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/messages/count_tokens"
}

func (h *Handler) PrepareLLMApiRequest(r *http.Request) (*dispatcher.PreparedLLMApiRequest, llmroutepkg.RequestRequirements, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, llmroutepkg.RequestRequirements{}, fmt.Errorf("failed to read request body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var req MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, llmroutepkg.RequestRequirements{}, fmt.Errorf("invalid request: %s", err)
	}
	if err := validateToolDefinitions(req.Tools); err != nil {
		return nil, llmroutepkg.RequestRequirements{}, fmt.Errorf("invalid request: %w", err)
	}
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		usage.SpanFromContext(r.Context()).SetExtension(usage.LLMExtension{
			LLMAPI: h.Name(), APIOperation: "count_tokens", Stream: usage.Bool(false), Execution: string(dispatcher.ExecutionLocal),
		})
		return &dispatcher.PreparedLLMApiRequest{
			Disposition: dispatcher.ExecutionLocal, Type: provider.LLMApiRequestTypeChat, RawRequest: &req,
		}, llmroutepkg.RequestRequirements{}, nil
	}

	h.logger.Debug(h.Name()+": request prepared",
		zap.String("model", req.Model),
		zap.Bool("stream", req.Stream),
		zap.Int("message_count", len(req.Messages)),
		zap.Int("max_tokens", req.MaxTokens),
	)

	conv := &Converter{}
	chatRequest := conv.ToInternal(&req)
	prepared := &dispatcher.PreparedLLMApiRequest{
		Disposition:     dispatcher.ExecutionProvider,
		Type:            provider.LLMApiRequestTypeChat,
		ChatRequest:     chatRequest,
		StreamRequested: req.Stream,
		RawRequest:      &req,
	}
	requestRequirements := llmroutepkg.RequestRequirements{
		Model: req.Model, RequireStreaming: req.Stream, RequireTools: hasAnthropicClientTools(req.Tools),
	}
	if chatRequest.ProtocolState != nil {
		requestRequirements.ProtocolRequirements = provider.CloneProtocolRequirementSet(chatRequest.ProtocolState.Requirements)
	}
	usage.SpanFromContext(r.Context()).SetExtension(usage.LLMExtension{
		LLMAPI:           h.Name(),
		APIOperation:     "messages",
		Stream:           usage.Bool(req.Stream),
		RequestToolCount: usage.Int(len(req.Tools)),
		RequestToolNames: anthropicToolNames(req.Tools),
		Execution:        string(dispatcher.ExecutionProvider),
	})
	return prepared, requestRequirements, nil
}

func hasAnthropicClientTools(tools []ToolDefinition) bool {
	for _, tool := range tools {
		if !tool.isServerTool() {
			return true
		}
	}
	return false
}

func validateToolDefinitions(tools []ToolDefinition) error {
	for i, tool := range tools {
		if tool.isServerTool() || isEmptyJSON(tool.InputSchema) {
			continue
		}
		normalized, err := provider.NormalizeObjectToolInputSchema(tool.InputSchema)
		if err != nil {
			return fmt.Errorf("tools[%d] %q input_schema %w", i, tool.Name, err)
		}
		var schemaValue einojsonschema.Schema
		if err := json.Unmarshal(normalized, &schemaValue); err != nil {
			return fmt.Errorf("tools[%d] %q input_schema is invalid: %w", i, tool.Name, err)
		}
	}
	return nil
}

// ServeLLMApi handles Anthropic-compatible API requests.
func (h *Handler) ServeLLMApi(w http.ResponseWriter, r *http.Request, prov provider.Provider, prepared *dispatcher.PreparedLLMApiRequest) error {
	if r.Method != http.MethodPost {
		h.writeError(w, r, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))
		return nil
	}

	if prepared == nil || !prepared.IsValid() {
		h.writeError(w, r, http.StatusBadRequest, fmt.Errorf("valid prepared request is required"))
		return nil
	}
	if prepared.Disposition == dispatcher.ExecutionLocal {
		h.handleCountTokens(w, r, prepared)
		return nil
	}
	h.handleMessages(w, r, prov, prepared)
	return nil
}

func (h *Handler) handleMessages(w http.ResponseWriter, r *http.Request, prov provider.Provider, prepared *dispatcher.PreparedLLMApiRequest) {
	var req *MessagesRequest
	ok := false
	if prepared != nil {
		req, ok = prepared.RawRequest.(*MessagesRequest)
	}
	if !ok || req == nil || prepared.Type != provider.LLMApiRequestTypeChat || prepared.ChatRequest == nil {
		h.writeError(w, r, http.StatusBadRequest, fmt.Errorf("invalid prepared request"))
		return
	}

	chatReq := prepared.ChatRequest
	if prov == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, fmt.Errorf("provider is not configured"))
		return
	}

	if prepared.Stream() {
		h.serveStream(w, r, prov, chatReq, req.Model)
		return
	}

	h.logger.Debug(h.Name()+": calling provider",
		zap.String("model", chatReq.Model),
		zap.Int("message_count", len(chatReq.Messages)),
		zap.String("provider_type", prov.Config().ProviderType),
	)
	usage.TransferFinishOwnership(r.Context())
	lifecycle := newSpanResponseLifecycle(usage.SpanFromContext(r.Context()), "batch")
	var resp *provider.ChatResponse
	var resolved *provider.ResolvedExecution
	var err error
	if executor, ok := prov.(provider.RoutedChatExecutor); ok {
		var execution *provider.ChatExecution
		execution, err = executor.ExecuteChat(r.Context(), chatReq)
		if execution != nil {
			resp = execution.Response
			resolved = &execution.Resolved
			lifecycle.ObserveExecution(execution.Resolved)
		}
	} else {
		resp, err = prov.Chat(r.Context(), chatReq)
	}
	if err != nil {
		status := statuserr.StatusCode(err, http.StatusBadGateway)
		if dispatcher.IsClientCanceled(err) {
			status = dispatcher.StatusClientClosedRequest
			_ = lifecycle.Cancel(responseFailure{StatusCode: status, Outcome: "client_cancel", ErrorType: "client_cancelled"})
		} else {
			_ = lifecycle.Fail(responseFailure{StatusCode: status, Outcome: "upstream_error", ErrorType: "provider_request_failed"})
		}
		h.writeProviderError(w, r, chatReq.Model, err)
		return
	}
	contentLen := 0
	finishReason := ""
	if resp != nil && resp.Message != nil {
		contentLen = len(resp.Message.Content)
		finishReason = provider.FinishReason(resp.Message)
		recordAnthropicToolCalls(r, resp.Message.ToolCalls)
	}
	h.logger.Debug(h.Name()+": provider response received",
		zap.String("model", chatReq.Model),
		zap.Int("content_length", contentLen),
		zap.String("finish_reason", finishReason),
	)
	open := responseOpen{Mode: responseModeNormalized, RewriteSet: rewriteSet{ClientModel: req.Model}, RelayIneligibleReason: "execution_metadata_unavailable"}
	if resolved != nil {
		open.Candidate = resolved.Candidate
		switch {
		case resolved.Candidate.Dialect != provider.ProtocolDialectAnthropic:
			open.RelayIneligibleReason = "served_dialect_mismatch"
		case !resolved.Candidate.Supports(provider.FeatureAnthropicBodyRelay):
			open.RelayIneligibleReason = "provider_feature_missing"
		case resp == nil || resp.Message == nil || len(anthropicbase.AnthropicResponseBodyFromMessage(resp.Message)) == 0:
			open.RelayIneligibleReason = "native_body_missing"
		default:
			open.Mode = responseModeNativeRelay
			open.RelayIneligibleReason = ""
		}
	}
	if err := newAnthropicResponseEncoder(lifecycle).Emit(r.Context(), open, resp, httpResponseBodySink{w: w}); err != nil {
		h.logger.Error(h.Name()+": encode response", zap.Error(err))
	}
}

func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, prov provider.Provider, chatReq *provider.ChatRequest, model string) {
	ctx := r.Context()
	h.logger.Debug(h.Name()+": opening stream",
		zap.String("model", chatReq.Model),
		zap.Int("message_count", len(chatReq.Messages)),
		zap.String("provider_type", prov.Config().ProviderType),
	)

	usage.TransferFinishOwnership(ctx)
	lifecycle := newSpanResponseLifecycle(usage.SpanFromContext(ctx), "stream")
	var stream *schema.StreamReader[*schema.Message]
	var resolved provider.ResolvedExecution
	var hasResolved bool
	var err error
	if executor, ok := prov.(provider.RoutedChatExecutor); ok {
		var execution *provider.StreamExecution
		execution, err = executor.ExecuteStreamChat(ctx, chatReq)
		if execution != nil {
			stream = execution.Stream
			resolved = execution.Resolved
			hasResolved = true
			lifecycle.ObserveExecution(execution.Resolved)
		}
	} else {
		stream, err = prov.StreamChat(ctx, chatReq)
	}
	if err != nil {
		status, _ := dispatcher.WriteProviderErrorLog(h.logger, w, r, h.Name(), chatReq.Model, "open stream", err)
		_ = lifecycle.Fail(responseFailure{StatusCode: status, Outcome: "upstream_open_error", ErrorType: "provider_stream_failed"})
		h.writeError(w, r, status, err)
		return
	}
	if stream == nil {
		err = fmt.Errorf("provider returned an empty stream")
		_ = lifecycle.Fail(responseFailure{StatusCode: http.StatusBadGateway, Outcome: "upstream_open_error", ErrorType: "provider_stream_failed"})
		h.writeError(w, r, http.StatusBadGateway, err)
		return
	}
	defer stream.Close()
	mode := streamModeNormalized
	relayIneligibleReason := "execution_metadata_unavailable"
	if hasResolved {
		switch {
		case resolved.Candidate.Dialect != provider.ProtocolDialectAnthropic:
			relayIneligibleReason = "served_dialect_mismatch"
		case !resolved.Candidate.Supports(provider.FeatureAnthropicStreamRelay):
			relayIneligibleReason = "provider_feature_missing"
		default:
			mode = streamModeNativeRelay
			relayIneligibleReason = ""
		}
	}

	sink := &httpAnthropicStreamSink{w: w, flusher: dispatcher.NewResponseFlusher(w), lifecycle: lifecycle}
	encoder := newAnthropicStreamEncoder(ctx, streamEncoderOptions{Model: model, MessageID: newAnthropicMessageID(), Mode: mode, RelayIneligibleReason: relayIneligibleReason}, sink, lifecycle)
	if err := encoder.Open(); err != nil {
		_ = lifecycle.Fail(responseFailure{StatusCode: http.StatusBadGateway, Outcome: "invalid_state", ErrorType: "invalid_state"})
		h.writeError(w, r, http.StatusBadGateway, err)
		return
	}

	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			if err := encoder.Finish(); err != nil {
				h.logger.Error(h.Name()+": finish stream", zap.Error(err))
				h.writePreCommitStreamError(w, r, sink, err)
			}
			break
		}
		if recvErr != nil {
			if dispatcher.IsClientCanceled(recvErr) {
				_ = encoder.Cancel(recvErr)
			} else {
				_ = encoder.Fail(recvErr)
				h.writePreCommitStreamError(w, r, sink, recvErr)
			}
			break
		}
		var nativeEvents []anthropicbase.AnthropicStreamEvent
		if mode == streamModeNativeRelay {
			nativeEvents = anthropicbase.AnthropicRelayStreamEventsFromMessage(chunk)
		} else {
			nativeEvents = anthropicbase.AnthropicStreamEventsFromMessage(chunk)
		}
		if len(nativeEvents) > 0 {
			for i := range nativeEvents {
				event := nativeEvents[i]
				if err := encoder.Accept(providerStreamEvent{Native: &event}); err != nil {
					_ = encoder.Fail(err)
					h.writePreCommitStreamError(w, r, sink, err)
					return
				}
			}
			continue
		}
		if err := encoder.Accept(providerStreamEvent{Generic: chunk}); err != nil {
			_ = encoder.Fail(err)
			h.writePreCommitStreamError(w, r, sink, err)
			return
		}
	}
	recordAnthropicToolNameSet(r, encoder.toolNames)
}

type httpAnthropicStreamSink struct {
	w         http.ResponseWriter
	flusher   dispatcher.ResponseFlusher
	lifecycle responseLifecycle
	committed bool
}

func (s *httpAnthropicStreamSink) Emit(_ context.Context, event anthropicStreamEvent) error {
	if !s.committed {
		if event.Event != "message_start" {
			return fmt.Errorf("first committing event is %q, want message_start", event.Event)
		}
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("Connection", "keep-alive")
		s.w.WriteHeader(http.StatusOK)
		s.committed = true
		s.lifecycle.Committed()
	}
	if err := writeRawSSEEventChecked(s.w, event.Event, event.Data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (h *Handler) writePreCommitStreamError(w http.ResponseWriter, r *http.Request, sink *httpAnthropicStreamSink, err error) {
	if sink == nil || sink.committed || dispatcher.IsClientCanceled(err) {
		return
	}
	status := statuserr.StatusCode(err, http.StatusBadGateway)
	h.writeError(w, r, status, err)
}
func anthropicToolNames(tools []ToolDefinition) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if name := strings.TrimSpace(tool.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func recordAnthropicToolCalls(r *http.Request, calls []schema.ToolCall) {
	names := map[string]struct{}{}
	for _, call := range calls {
		if name := strings.TrimSpace(call.Function.Name); name != "" {
			names[name] = struct{}{}
		}
	}
	recordAnthropicToolNameSet(r, names)
}

func recordAnthropicToolNameSet(r *http.Request, set map[string]struct{}) {
	if r == nil {
		return
	}
	if len(set) == 0 {
		usage.SpanFromContext(r.Context()).SetExtension(usage.LLMExtension{ToolCallCount: usage.Int(0)})
		return
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	usage.SpanFromContext(r.Context()).SetExtension(usage.LLMExtension{
		ToolCallCount: usage.Int(len(names)),
		ToolNames:     names,
	})
}

func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request, prepared *dispatcher.PreparedLLMApiRequest) {
	usage.TransferFinishOwnership(r.Context())
	lifecycle := newSpanResponseLifecycle(usage.SpanFromContext(r.Context()), "batch")
	lifecycle.ObserveResponse(responseObservation{Mode: "local", MessageIDSource: "none"})
	if !h.estimateCountTokens {
		_ = lifecycle.Fail(responseFailure{StatusCode: http.StatusNotImplemented, Outcome: "unsupported_local", ErrorType: "not_implemented"})
		h.writeError(w, r, http.StatusNotImplemented, fmt.Errorf("count_tokens is not supported"))
		return
	}
	var req *MessagesRequest
	if prepared != nil {
		req, _ = prepared.RawRequest.(*MessagesRequest)
	}
	if req == nil {
		_ = lifecycle.Fail(responseFailure{StatusCode: http.StatusBadRequest, Outcome: "invalid_state", ErrorType: "invalid_prepared_request"})
		h.writeError(w, r, http.StatusBadRequest, fmt.Errorf("invalid prepared request"))
		return
	}
	estimate := estimateAnthropicInputTokens(req)
	lifecycle.ObserveResponse(responseObservation{Mode: "local", MessageIDSource: "none", UsageSource: "estimated"})
	lifecycle.ObserveUsage(usageObservation{InputTokens: estimate, Final: true})
	if err := httpjson.Write(w, http.StatusOK, map[string]any{"input_tokens": estimate}); err != nil {
		_ = lifecycle.Fail(responseFailure{StatusCode: http.StatusOK, Outcome: "sink_error", ErrorType: "response_write_failed"})
		return
	}
	lifecycle.Committed()
	_ = lifecycle.Finish(responseFinish{StatusCode: http.StatusOK, Outcome: "completed"})
}

func estimateAnthropicInputTokens(req *MessagesRequest) int {
	if req == nil {
		return 1
	}
	chars := len(req.System.Text())
	for _, msg := range req.Messages {
		chars += len(msg.Role)
		chars += len(msg.Content.Text())
		for _, block := range msg.Content {
			chars += len(block.Type) + len(block.Text) + len(block.ID) + len(block.Name) + len(block.ToolUseID)
			chars += len(block.Input)
			if block.Source != nil {
				chars += len(block.Source.Type) + len(block.Source.MediaType) + len(block.Source.Data) + len(block.Source.URL)
			}
		}
	}
	for _, tool := range req.Tools {
		chars += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}
	tokens := chars / 4
	if tokens < 1 {
		return 1
	}
	return tokens
}

// writeError logs and writes an Anthropic-format error response.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, status int, cause error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	if r != nil {
		logFields := []zap.Field{zap.String("protocol", h.Name())}
		dispatcher.WriteHttpErrorLog(h.logger, w, r, status, "", cause, logFields...)
	}
	_ = httpjson.Write(w, status, anthropicErrorResponse{
		Type: "error",
		Error: anthropicErrorBody{
			Type:    errTypeForStatus(status),
			Message: msg,
		},
	})
}

// writeProviderError logs and writes an Anthropic-format error response for upstream errors.
func (h *Handler) writeProviderError(w http.ResponseWriter, r *http.Request, model string, err error) {
	status, msg := dispatcher.WriteProviderErrorLog(h.logger, w, r, h.Name(), model, "generate response", err)
	_ = httpjson.Write(w, status, anthropicErrorResponse{
		Type: "error",
		Error: anthropicErrorBody{
			Type:    errTypeForStatus(status),
			Message: msg,
		},
	})
}

// anthropicErrorResponse is the error format the Anthropic SDK expects.
type anthropicErrorResponse struct {
	Type  string             `json:"type"`
	Error anthropicErrorBody `json:"error"`
}

type anthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func errTypeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// extractText returns the text content from a streaming message chunk.
func extractText(msg *schema.Message) string {
	if msg == nil {
		return ""
	}
	return msg.Content
}

// mapAnthropicStopReason converts provider finish reasons to Anthropic stop reasons.
func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "stop", "end_turn":
		return "end_turn"
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_calls", "tool_use":
		return "tool_use"
	case "stop_sequence":
		return "stop_sequence"
	default:
		return ""
	}
}

func writeSSEEvent(w http.ResponseWriter, event string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
}

func writeRawSSEEvent(w http.ResponseWriter, event string, data json.RawMessage) {
	if strings.TrimSpace(event) == "" || len(data) == 0 {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

func writeRawSSEEventChecked(w http.ResponseWriter, event string, data json.RawMessage) error {
	if strings.TrimSpace(event) == "" || len(data) == 0 {
		return fmt.Errorf("empty SSE event")
	}
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

func anthropicSSEEventIndex(data json.RawMessage) (int, bool) {
	var event struct {
		Index *int `json:"index"`
	}
	if json.Unmarshal(data, &event) != nil || event.Index == nil {
		return 0, false
	}
	return *event.Index, true
}

func rewriteAnthropicSSEEventIndex(data json.RawMessage, index int) json.RawMessage {
	var event map[string]any
	if json.Unmarshal(data, &event) != nil {
		return data
	}
	event["index"] = index
	rewritten, err := json.Marshal(event)
	if err != nil {
		return data
	}
	return rewritten
}

var (
	_ dispatcher.LLMApiHandler = (*Handler)(nil)
)
