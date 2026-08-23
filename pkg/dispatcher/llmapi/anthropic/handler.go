package anthropic

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
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

// Handler handles Anthropic-format API requests (/v1/messages).
type Handler struct {
	logger              *zap.Logger
	name                string
	estimateCountTokens bool
}

// HandlerOptions configures an Anthropic-format handler variant.
type HandlerOptions struct {
	Name                string
	EstimateCountTokens bool
}

func init() {
	dispatcher.RegisterLLMApiHandlerType("anthropic")
}

// NewHandler creates a Handler.
func NewHandler(_ provider.Provider) *Handler {
	return NewHandlerWithOptions(HandlerOptions{Name: "anthropic"})
}

// NewHandlerWithOptions creates a configured Anthropic-format handler.
func NewHandlerWithOptions(opts HandlerOptions) *Handler {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = "anthropic"
	}
	return &Handler{
		logger:              zap.NewNop(),
		name:                name,
		estimateCountTokens: opts.EstimateCountTokens,
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

	h.logger.Debug(h.Name()+": request prepared",
		zap.String("model", req.Model),
		zap.Bool("stream", req.Stream),
		zap.Int("message_count", len(req.Messages)),
		zap.Int("max_tokens", req.MaxTokens),
	)

	conv := &Converter{}
	chatRequest := conv.ToInternal(&req)
	prepared := &dispatcher.PreparedLLMApiRequest{
		Type:            provider.LLMApiRequestTypeChat,
		ChatRequest:     chatRequest,
		StreamRequested: req.Stream,
		RawRequest:      &req,
	}
	requestRequirements := llmroutepkg.RequestRequirements{
		Model:            req.Model,
		RequireStreaming: req.Stream,
		RequireTools:     hasAnthropicClientTools(req.Tools),
	}
	if hasAnthropicServerTools(req.Tools) || provider.HasAnthropicNativeContent(chatRequest.Messages) {
		requestRequirements = requestRequirements.WithNativeDialect(provider.ProtocolDialectAnthropic)
	}
	if provider.HasAnthropicNativeReasoning(chatRequest.Messages) {
		requestRequirements = requestRequirements.WithReasoningDialect(provider.ProtocolDialectAnthropic)
	}
	usage.SpanFromContext(r.Context()).SetExtension(usage.LLMExtension{
		LLMAPI:           h.Name(),
		APIOperation:     "messages",
		Stream:           usage.Bool(req.Stream),
		RequestToolCount: usage.Int(len(req.Tools)),
		RequestToolNames: anthropicToolNames(req.Tools),
	})
	return prepared, requestRequirements, nil
}

func hasAnthropicServerTools(tools []ToolDefinition) bool {
	for _, tool := range tools {
		if tool.isServerTool() {
			return true
		}
	}
	return false
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

	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
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
	if !ok || req == nil || prepared == nil || prepared.Type != provider.LLMApiRequestTypeChat || prepared.ChatRequest == nil {
		var err error
		prepared, _, err = h.PrepareLLMApiRequest(r)
		if err != nil {
			h.writeError(w, r, statuserr.StatusCode(err, http.StatusBadRequest), fmt.Errorf("prepare request: %w", err))
			return
		}
		var castOK bool
		req, castOK = prepared.RawRequest.(*MessagesRequest)
		if !castOK || req == nil || prepared.Type != provider.LLMApiRequestTypeChat || prepared.ChatRequest == nil {
			h.writeError(w, r, http.StatusBadRequest, fmt.Errorf("invalid request"))
			return
		}
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
	resp, err := prov.Chat(r.Context(), chatReq)
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
	conv := &Converter{}
	if resp != nil && resp.Message != nil {
		tokens := provider.UsageFromMessage(resp.Message)
		lifecycle.ObserveUsage(usageObservation{InputTokens: tokens.InputTokens, OutputTokens: tokens.OutputTokens, CachedTokens: tokens.CachedTokens, ReasoningTokens: tokens.ReasoningTokens, Final: true})
	}
	if err := httpjson.Write(w, http.StatusOK, conv.FromInternal(resp, req.Model)); err != nil {
		_ = lifecycle.Fail(responseFailure{StatusCode: http.StatusOK, Outcome: "sink_error", ErrorType: "response_write_failed"})
		return
	}
	lifecycle.Committed()
	_ = lifecycle.Finish(responseFinish{StatusCode: http.StatusOK, Outcome: "completed"})
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
	stream, err := prov.StreamChat(ctx, chatReq)
	if err != nil {
		status, _ := dispatcher.WriteProviderErrorLog(h.logger, w, r, h.Name(), chatReq.Model, "open stream", err)
		_ = lifecycle.Fail(responseFailure{StatusCode: status, Outcome: "upstream_open_error", ErrorType: "provider_stream_failed"})
		h.writeError(w, r, status, err)
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	sink := &httpAnthropicStreamSink{w: w, flusher: dispatcher.NewResponseFlusher(w), lifecycle: lifecycle}
	encoder := newAnthropicStreamEncoder(ctx, streamEncoderOptions{Model: model, MessageID: newAnthropicMessageID()}, sink, lifecycle)
	if err := encoder.Open(); err != nil {
		h.writeError(w, r, http.StatusBadGateway, err)
		return
	}

	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			if err := encoder.Finish(); err != nil {
				h.logger.Error(h.Name()+": finish stream", zap.Error(err))
			}
			break
		}
		if recvErr != nil {
			if dispatcher.IsClientCanceled(recvErr) {
				_ = encoder.Cancel(recvErr)
			} else {
				_ = encoder.Fail(recvErr)
			}
			break
		}
		nativeEvents := provider.AnthropicStreamEventsFromMessage(chunk)
		if len(nativeEvents) > 0 {
			for i := range nativeEvents {
				event := nativeEvents[i]
				if err := encoder.Accept(providerStreamEvent{Native: &event}); err != nil {
					_ = encoder.Fail(err)
					return
				}
			}
			continue
		}
		if err := encoder.Accept(providerStreamEvent{Generic: chunk}); err != nil {
			_ = encoder.Fail(err)
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
	if !h.estimateCountTokens {
		h.writeError(w, r, http.StatusNotImplemented, fmt.Errorf("count_tokens is not supported"))
		return
	}
	var req *MessagesRequest
	if prepared != nil {
		req, _ = prepared.RawRequest.(*MessagesRequest)
	}
	if req == nil {
		parsed, _, err := h.PrepareLLMApiRequest(r)
		if err != nil {
			h.writeError(w, r, statuserr.StatusCode(err, http.StatusBadRequest), fmt.Errorf("prepare request: %w", err))
			return
		}
		req, _ = parsed.RawRequest.(*MessagesRequest)
	}
	_ = httpjson.Write(w, http.StatusOK, map[string]any{
		"input_tokens": estimateAnthropicInputTokens(req),
	})
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
