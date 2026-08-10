package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/httplog"
	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	dispatcher "github.com/agent-guide/agent-gateway/pkg/dispatcher"
	llmroutepkg "github.com/agent-guide/agent-gateway/pkg/gateway/llmroute"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

// ResponsesRequest reuses the provider-level request model directly because
// OpenAI Responses API payloads are forwarded to the selected provider.
type ResponsesRequest = provider.ResponsesRequest

// Handler handles OpenAI-format API requests (/v1/chat/completions, etc.).
type Handler struct {
	logger *zap.Logger
}

func init() {
	dispatcher.RegisterLLMApiHandlerType("openai")
}

// NewHandler creates a Handler.
func NewHandler() *Handler {
	return &Handler{logger: zap.NewNop()}
}

func (h *Handler) Name() string {
	return "openai"
}

// SetLogger configures the handler logger.
func (h *Handler) SetLogger(logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}
	h.logger = logger
}

func (h *Handler) MatchLLMApi(r *http.Request) bool {
	return r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/chat/completions" ||
		r.URL.Path == "/v1/responses" || r.URL.Path == "/responses" ||
		r.URL.Path == "/v1/models" || r.URL.Path == "/models" ||
		r.URL.Path == "/v1/embeddings" || r.URL.Path == "/embeddings"
}

func (h *Handler) PrepareLLMApiRequest(r *http.Request) (*dispatcher.PreparedLLMApiRequest, llmroutepkg.RequestRequirements, error) {
	if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
		return &dispatcher.PreparedLLMApiRequest{
			Type: provider.LLMApiRequestTypeModels,
			RawRequest: struct {
				Path string
			}{Path: r.URL.Path},
		}, llmroutepkg.RequestRequirements{}, nil
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, llmroutepkg.RequestRequirements{}, fmt.Errorf("failed to read request body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	if r.URL.Path == "/v1/responses" || r.URL.Path == "/responses" {
		var req ResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, llmroutepkg.RequestRequirements{}, fmt.Errorf("invalid request: %s", err)
		}
		h.logger.Debug("openai: request prepared",
			zap.String("request_type", string(provider.LLMApiRequestTypeResponses)),
			zap.String("model", req.Model),
			zap.Bool("stream", req.Stream),
			zap.Bool("has_input", req.Input != nil),
		)
		prepared := &dispatcher.PreparedLLMApiRequest{
			Type:             provider.LLMApiRequestTypeResponses,
			ResponsesRequest: &req,
			StreamRequested:  req.Stream,
			RawRequest:       &req,
		}
		requestRequirements := llmroutepkg.RequestRequirements{
			Model:            req.Model,
			RequireStreaming: req.Stream,
		}
		usage.SpanFromContext(r.Context()).SetExtension(usage.LLMExtension{
			LLMAPI:           h.Name(),
			APIOperation:     "responses",
			Stream:           usage.Bool(req.Stream),
			RequestToolCount: usage.Int(len(req.Tools)),
			RequestToolNames: responseToolNames(req.Tools),
		})
		return prepared, requestRequirements, nil
	}

	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, llmroutepkg.RequestRequirements{}, fmt.Errorf("invalid request: %s", err)
	}
	h.logger.Debug("openai: request prepared",
		zap.String("request_type", string(provider.LLMApiRequestTypeChat)),
		zap.String("model", req.Model),
		zap.Bool("stream", req.Stream),
		zap.Int("message_count", len(req.Messages)),
		zap.Int("max_tokens", req.MaxTokens),
	)

	conv := &Converter{}
	prepared := &dispatcher.PreparedLLMApiRequest{
		Type:            provider.LLMApiRequestTypeChat,
		ChatRequest:     conv.ToInternal(&req),
		StreamRequested: req.Stream,
		RawRequest:      &req,
	}
	requestRequirements := llmroutepkg.RequestRequirements{
		Model:            req.Model,
		RequireStreaming: req.Stream,
	}
	usage.SpanFromContext(r.Context()).SetExtension(usage.LLMExtension{
		LLMAPI:           h.Name(),
		APIOperation:     "chat_completions",
		Stream:           usage.Bool(req.Stream),
		RequestToolCount: usage.Int(len(req.Tools)),
		RequestToolNames: chatToolNames(req.Tools),
	})
	return prepared, requestRequirements, nil
}

// ServeLLMApi handles OpenAI-compatible API requests.
func (h *Handler) ServeLLMApi(w http.ResponseWriter, r *http.Request, prov provider.Provider, prepared *dispatcher.PreparedLLMApiRequest) error {
	if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
		return h.serveModels(w, r, prov)
	}

	if r.Method != http.MethodPost {
		h.writeError(w, r, http.StatusMethodNotAllowed, "method not allowed", fmt.Errorf("method %s not allowed", r.Method))
		return nil
	}

	if r.URL.Path == "/v1/responses" || r.URL.Path == "/responses" {
		return h.serveResponses(w, r, prov, prepared)
	}

	var req *ChatCompletionRequest
	ok := false
	if prepared != nil {
		req, ok = prepared.RawRequest.(*ChatCompletionRequest)
	}
	if !ok || req == nil || prepared == nil || prepared.Type != provider.LLMApiRequestTypeChat || prepared.ChatRequest == nil {
		h.writeError(w, r, http.StatusBadRequest, "invalid request", fmt.Errorf("prepare request returned invalid openai payload"))
		return nil
	}

	chatReq := prepared.ChatRequest
	if prov == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, "provider is not configured", fmt.Errorf("provider is not configured"), zap.String("model", chatReq.Model))
		return nil
	}

	if prepared.Stream() {
		h.serveStream(w, r, prov, chatReq)
		return nil
	}

	h.logger.Debug("openai: calling provider",
		zap.String("request_type", string(provider.LLMApiRequestTypeChat)),
		zap.String("model", chatReq.Model),
		zap.Int("message_count", len(chatReq.Messages)),
		zap.String("provider_type", prov.Config().ProviderType),
	)
	resp, err := prov.Chat(r.Context(), chatReq)
	if err != nil {
		_ = writeProviderError(h.logger, w, r, chatReq.Model, "chat response", err)
		return nil
	}
	contentLen := 0
	finishReason := ""
	if resp != nil && resp.Message != nil {
		contentLen = len(resp.Message.Content)
		finishReason = provider.FinishReason(resp.Message)
		recordToolCalls(r, resp.Message.ToolCalls)
	}
	h.logger.Debug("openai: provider response received",
		zap.String("request_type", string(provider.LLMApiRequestTypeChat)),
		zap.String("model", chatReq.Model),
		zap.Int("content_length", contentLen),
		zap.String("finish_reason", finishReason),
	)
	conv := &Converter{}
	_ = httpjson.Write(w, http.StatusOK, conv.FromInternal(resp, chatReq.Model))
	return nil
}

func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request, prov provider.Provider) error {
	if r.Method != http.MethodGet {
		h.writeError(w, r, http.StatusMethodNotAllowed, "method not allowed", fmt.Errorf("method %s not allowed", r.Method))
		return nil
	}
	if prov == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, "provider is not configured", fmt.Errorf("provider is not configured"))
		return nil
	}
	models, err := prov.ListModels(r.Context())
	if err != nil {
		_ = writeProviderError(h.logger, w, r, "", "list models", err)
		return nil
	}
	type modelData struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	resp := struct {
		Object string      `json:"object"`
		Data   []modelData `json:"data"`
	}{
		Object: "list",
		Data:   make([]modelData, 0, len(models)),
	}
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			id = strings.TrimSpace(model.Name)
		}
		if id == "" {
			continue
		}
		resp.Data = append(resp.Data, modelData{
			ID:      id,
			Object:  "model",
			OwnedBy: strings.TrimSpace(prov.Config().ProviderType),
		})
	}
	_ = httpjson.Write(w, http.StatusOK, resp)
	return nil
}

func (h *Handler) serveResponses(w http.ResponseWriter, r *http.Request, prov provider.Provider, prepared *dispatcher.PreparedLLMApiRequest) error {
	if prepared == nil || prepared.Type != provider.LLMApiRequestTypeResponses || prepared.ResponsesRequest == nil {
		h.writeError(w, r, http.StatusBadRequest, "invalid request", fmt.Errorf("prepare request returned invalid responses payload"))
		return nil
	}
	req, ok := prepared.RawRequest.(*ResponsesRequest)
	if !ok || req == nil {
		h.writeError(w, r, http.StatusBadRequest, "invalid request", fmt.Errorf("prepare request returned invalid responses payload"))
		return nil
	}

	respReq := prepared.ResponsesRequest
	if prov == nil {
		h.writeError(w, r, http.StatusServiceUnavailable, "provider is not configured", fmt.Errorf("provider is not configured"), zap.String("model", respReq.Model))
		return nil
	}

	if prepared.Stream() {
		responsesProv, ok := prov.(provider.ResponsesProvider)
		if !ok {
			h.writeError(w, r, http.StatusNotImplemented, "responses api is not supported", fmt.Errorf("provider %q does not implement responses api", prov.Config().ProviderType), zap.String("model", respReq.Model))
			return nil
		}
		h.logger.Debug("openai: starting responses stream",
			zap.String("request_type", string(provider.LLMApiRequestTypeResponses)),
			zap.String("model", respReq.Model),
			zap.String("provider_type", prov.Config().ProviderType),
		)
		stream, err := responsesProv.StreamResponses(r.Context(), respReq)
		if err != nil {
			_ = writeProviderError(h.logger, w, r, respReq.Model, "stream responses response", err)
			return nil
		}
		h.writeProviderResponsesStream(w, r, stream, respReq.Model)
		return nil
	}

	responsesProv, ok := prov.(provider.ResponsesProvider)
	if !ok {
		h.writeError(w, r, http.StatusNotImplemented, "responses api is not supported", fmt.Errorf("provider %q does not implement responses api", prov.Config().ProviderType), zap.String("model", respReq.Model))
		return nil
	}
	h.logger.Debug("openai: calling provider",
		zap.String("request_type", string(provider.LLMApiRequestTypeResponses)),
		zap.String("model", respReq.Model),
		zap.String("provider_type", prov.Config().ProviderType),
	)
	resp, err := responsesProv.CreateResponses(r.Context(), respReq)
	if err != nil {
		_ = writeProviderError(h.logger, w, r, respReq.Model, "create responses", err)
		return nil
	}
	h.logger.Debug("openai: provider response received",
		zap.String("request_type", string(provider.LLMApiRequestTypeResponses)),
		zap.String("model", respReq.Model),
	)
	recordResponsesUsage(r, resp)
	recordResponsesToolCalls(r, resp)
	writeResponsesJSON(w, http.StatusOK, resp)
	return nil
}

func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, prov provider.Provider, chatReq *provider.ChatRequest) {
	ctx := r.Context()
	h.logger.Debug("openai: starting stream",
		zap.String("request_type", string(provider.LLMApiRequestTypeChat)),
		zap.String("model", chatReq.Model),
		zap.Int("message_count", len(chatReq.Messages)),
		zap.String("provider_type", prov.Config().ProviderType),
	)
	stream, err := prov.StreamChat(ctx, chatReq)
	if err != nil {
		_ = writeProviderError(h.logger, w, r, chatReq.Model, "stream chat response", err)
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher := dispatcher.NewResponseFlusher(w)
	chunkCount := 0
	toolNames := map[string]struct{}{}
	inputTokens := 0
	outputTokens := 0
	cachedTokens := 0
	reasoningTokens := 0
	usageFinalized := false
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			httplog.Error(h.logger, "http request failed", r, http.StatusOK, fmt.Errorf("receive stream chunk: %w", err),
				zap.String("protocol", "openai"),
				zap.String("model", chatReq.Model),
			)
			break
		}
		chunkCount++
		for _, tc := range chunk.ToolCalls {
			if name := strings.TrimSpace(tc.Function.Name); name != "" {
				toolNames[name] = struct{}{}
			}
		}
		if chunk.ResponseMeta != nil && chunk.ResponseMeta.Usage != nil {
			if chunk.ResponseMeta.Usage.PromptTokens > 0 {
				inputTokens = chunk.ResponseMeta.Usage.PromptTokens
			}
			if chunk.ResponseMeta.Usage.CompletionTokens > 0 {
				outputTokens = chunk.ResponseMeta.Usage.CompletionTokens
			}
			if chunk.ResponseMeta.Usage.PromptTokenDetails.CachedTokens > 0 {
				cachedTokens = chunk.ResponseMeta.Usage.PromptTokenDetails.CachedTokens
			}
			if chunk.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens > 0 {
				reasoningTokens = chunk.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens
			}
			usageFinalized = inputTokens > 0 || outputTokens > 0
		}

		payload, err := json.Marshal(toStreamChunk(chatReq.Model, chunk))
		if err != nil {
			httplog.Error(h.logger, "http request failed", r, http.StatusOK, fmt.Errorf("marshal stream chunk: %w", err),
				zap.String("protocol", "openai"),
				zap.String("model", chatReq.Model),
			)
			break
		}
		fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
	}
	h.logger.Debug("openai: stream completed",
		zap.String("request_type", string(provider.LLMApiRequestTypeChat)),
		zap.String("model", chatReq.Model),
		zap.Int("chunks", chunkCount),
	)
	recordToolNameSet(r, toolNames)
	usage.SpanFromContext(r.Context()).SetExtension(usage.LLMExtension{
		InputTokens:     usage.Int(inputTokens),
		OutputTokens:    usage.Int(outputTokens),
		TotalTokens:     usage.Int(inputTokens + outputTokens),
		CachedTokens:    usage.Int(cachedTokens),
		ReasoningTokens: usage.Int(reasoningTokens),
		UsageFinalized:  usage.Bool(usageFinalized),
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (h *Handler) writeProviderResponsesStream(w http.ResponseWriter, r *http.Request, stream *schema.StreamReader[*provider.ResponsesStreamEvent], model string) {
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher := dispatcher.NewResponseFlusher(w)
	eventCount := 0
	toolNames := map[string]struct{}{}
	inputTokens := 0
	outputTokens := 0
	totalTokens := 0
	cachedTokens := 0
	reasoningTokens := 0
	usageFinalized := false
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			httplog.Error(h.logger, "http request failed", r, http.StatusOK, fmt.Errorf("receive provider responses stream chunk: %w", err),
				zap.String("protocol", "openai"),
				zap.String("model", model),
			)
			break
		}
		if event == nil {
			continue
		}
		if event.Item != nil {
			if name := strings.TrimSpace(event.Item.Name); name != "" {
				toolNames[name] = struct{}{}
			}
		}
		if event.Response != nil && event.Response.Usage != nil {
			inputTokens = event.Response.Usage.InputTokens
			outputTokens = event.Response.Usage.OutputTokens
			totalTokens = event.Response.Usage.TotalTokens
			cachedTokens = event.Response.Usage.InputTokensDetails.CachedTokens
			reasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
			usageFinalized = totalTokens > 0 || inputTokens > 0 || outputTokens > 0
		}
		eventCount++
		if err := writeResponsesEvent(w, event); err != nil {
			httplog.Error(h.logger, "http request failed", r, http.StatusOK, fmt.Errorf("marshal provider responses stream chunk: %w", err),
				zap.String("protocol", "openai"),
				zap.String("model", model),
			)
			break
		}
		flusher.Flush()
	}
	h.logger.Debug("openai: responses stream completed",
		zap.String("request_type", string(provider.LLMApiRequestTypeResponses)),
		zap.String("model", model),
		zap.Int("events", eventCount),
	)
	recordToolNameSet(r, toolNames)
	if totalTokens == 0 {
		totalTokens = inputTokens + outputTokens
	}
	usage.SpanFromContext(r.Context()).SetExtension(usage.LLMExtension{
		InputTokens:     usage.Int(inputTokens),
		OutputTokens:    usage.Int(outputTokens),
		TotalTokens:     usage.Int(totalTokens),
		CachedTokens:    usage.Int(cachedTokens),
		ReasoningTokens: usage.Int(reasoningTokens),
		UsageFinalized:  usage.Bool(usageFinalized),
	})
}

func chatToolNames(tools []ToolDefinition) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if name := strings.TrimSpace(tool.Function.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func responseToolNames(tools []provider.ResponsesToolDefinition) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" && tool.Function != nil {
			name = strings.TrimSpace(tool.Function.Name)
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func recordToolCalls(r *http.Request, calls []schema.ToolCall) {
	names := map[string]struct{}{}
	for _, call := range calls {
		if name := strings.TrimSpace(call.Function.Name); name != "" {
			names[name] = struct{}{}
		}
	}
	recordToolNameSet(r, names)
}

func recordResponsesToolCalls(r *http.Request, resp *provider.ResponsesResponse) {
	names := map[string]struct{}{}
	if resp != nil {
		for _, item := range resp.Output {
			if name := strings.TrimSpace(item.Name); name != "" {
				names[name] = struct{}{}
			}
		}
	}
	recordToolNameSet(r, names)
}

func recordResponsesUsage(r *http.Request, resp *provider.ResponsesResponse) {
	if r == nil || resp == nil || resp.Usage == nil {
		return
	}
	totalTokens := resp.Usage.TotalTokens
	if totalTokens == 0 {
		totalTokens = resp.Usage.InputTokens + resp.Usage.OutputTokens
	}
	usage.SpanFromContext(r.Context()).SetExtension(usage.LLMExtension{
		InputTokens:     usage.Int(resp.Usage.InputTokens),
		OutputTokens:    usage.Int(resp.Usage.OutputTokens),
		TotalTokens:     usage.Int(totalTokens),
		CachedTokens:    usage.Int(resp.Usage.InputTokensDetails.CachedTokens),
		ReasoningTokens: usage.Int(resp.Usage.OutputTokensDetails.ReasoningTokens),
		UsageFinalized:  usage.Bool(totalTokens > 0 || resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0),
	})
}

func recordToolNameSet(r *http.Request, set map[string]struct{}) {
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

func writeResponsesEvent(w http.ResponseWriter, event *provider.ResponsesStreamEvent) error {
	payload := event.RawJSON
	if len(payload) == 0 {
		var err error
		payload, err = json.Marshal(event)
		if err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, payload)
	return err
}

func writeResponsesJSON(w http.ResponseWriter, status int, resp *provider.ResponsesResponse) error {
	if resp != nil && len(resp.RawJSON) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, err := w.Write(resp.RawJSON)
		return err
	}
	return httpjson.Write(w, status, resp)
}

func writeProviderError(logger *zap.Logger, w http.ResponseWriter, r *http.Request, model string, phase string, err error) error {
	status, clientMessage := dispatcher.WriteProviderErrorLog(logger, w, r, "openai", model, phase, err)
	return httpjson.Write(w, status, openAIErrorResponse{
		Error: openAIErrorBody{
			Message: clientMessage,
			Type:    openAIErrorTypeForStatus(status),
			Param:   nil,
			Code:    nil,
		},
	})
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, status int, clientMessage string, cause error, fields ...zap.Field) {
	if r != nil {
		logFields := []zap.Field{zap.String("protocol", "openai")}
		logFields = append(logFields, fields...)
		dispatcher.WriteHttpErrorLog(h.logger, w, r, status, "serve openai request", cause, logFields...)
	}
	_ = httpjson.Write(w, status, openAIErrorResponse{
		Error: openAIErrorBody{
			Message: clientMessage,
			Type:    openAIErrorTypeForStatus(status),
			Param:   nil,
			Code:    nil,
		},
	})
}

type openAIErrorResponse struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    any    `json:"code"`
}

func openAIErrorTypeForStatus(status int) string {
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

type chatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason string     `json:"finish_reason,omitempty"`
}

type chunkDelta struct {
	Role             string            `json:"role,omitempty"`
	Content          string            `json:"content,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []schema.ToolCall `json:"tool_calls,omitempty"`
}

func toStreamChunk(model string, msg *schema.Message) *chatCompletionChunk {
	chunk := &chatCompletionChunk{
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chunkChoice{{
			Index: 0,
			Delta: chunkDelta{
				Role:             string(msg.Role),
				Content:          msg.Content,
				ReasoningContent: msg.ReasoningContent,
			},
		}},
	}
	if len(msg.ToolCalls) > 0 {
		chunk.Choices[0].Delta.ToolCalls = msg.ToolCalls
	}
	if msg.ResponseMeta != nil {
		chunk.Choices[0].FinishReason = msg.ResponseMeta.FinishReason
	}
	return chunk
}

var (
	_ dispatcher.LLMApiHandler = (*Handler)(nil)
)
