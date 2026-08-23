package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	einojsonschema "github.com/eino-contrib/jsonschema"

	"github.com/agent-guide/agent-gateway/internal/httpjson"
	"github.com/agent-guide/agent-gateway/internal/httplog"
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
	resp, err := prov.Chat(r.Context(), chatReq)
	if err != nil {
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
	_ = httpjson.Write(w, http.StatusOK, conv.FromInternal(resp, req.Model))
}

func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, prov provider.Provider, chatReq *provider.ChatRequest, model string) {
	ctx := r.Context()
	h.logger.Debug(h.Name()+": starting stream",
		zap.String("model", chatReq.Model),
		zap.Int("message_count", len(chatReq.Messages)),
		zap.String("provider_type", prov.Config().ProviderType),
	)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher := dispatcher.NewResponseFlusher(w)
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	writeSSEEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant",
			"model": model, "content": []any{},
			"stop_reason": nil,
			"usage":       map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	flusher.Flush()

	stream, err := prov.StreamChat(ctx, chatReq)
	if err != nil {
		httplog.Error(h.logger, "http request failed", r, http.StatusOK, fmt.Errorf("open stream: %w", err),
			zap.String("protocol", h.Name()),
			zap.String("model", chatReq.Model),
		)
		writeSSEEvent(w, "error", anthropicErrorResponse{
			Type: "error",
			Error: anthropicErrorBody{
				Type:    "api_error",
				Message: err.Error(),
			},
		})
		flusher.Flush()
		return
	}
	defer stream.Close()
	h.logger.Debug(h.Name()+": provider stream opened",
		zap.String("model", chatReq.Model),
		zap.String("provider_type", prov.Config().ProviderType),
	)

	chunkCount := 0
	reasoningBlockStarted := false
	reasoningBlockIndex := -1
	reasoningSourceIndex := -1
	reasoningSignature := ""
	var reasoningContent strings.Builder
	textBlockStarted := false
	textBlockIndex := -1
	finalStopReason := "end_turn"
	finalInputTokens := 0
	finalOutputTokens := 0
	finalCachedTokens := 0
	finalReasoningTokens := 0
	usageFinalized := false
	nextBlockIndex := 0
	nativeBlockIndices := map[int]int{}
	emittedToolUse := false
	toolNames := map[string]struct{}{}
	// Streamed tool calls arrive as fragments: the first fragment for a tool-call
	// index carries id+name, later fragments carry argument deltas. Accumulate
	// them into one Anthropic tool_use content block per index instead of emitting
	// a separate block per fragment.
	type streamToolBlock struct {
		blockIndex       int
		started          bool
		closed           bool
		id               string
		name             string
		arguments        strings.Builder
		pendingArguments strings.Builder
	}
	toolBlocks := map[int]*streamToolBlock{}
	var toolBlockOrder []int
	var deferredText strings.Builder
	toolCallIDs := map[string]struct{}{}
	syntheticToolCallID := 0
	allocateBlockIndex := func() int {
		index := nextBlockIndex
		nextBlockIndex++
		return index
	}
	closeReasoningBlock := func() {
		if !reasoningBlockStarted {
			return
		}
		signature := reasoningSignature
		if signature == "" {
			signature = gatewayThinkingSignature(reasoningContent.String())
		}
		writeSSEEvent(w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": reasoningBlockIndex,
			"delta": map[string]string{
				"type":      "signature_delta",
				"signature": signature,
			},
		})
		writeSSEEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": reasoningBlockIndex})
		reasoningBlockStarted = false
		reasoningBlockIndex = -1
		reasoningSourceIndex = -1
		reasoningSignature = ""
		reasoningContent.Reset()
	}
	closeTextBlock := func() {
		if !textBlockStarted {
			return
		}
		writeSSEEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": textBlockIndex})
		textBlockStarted = false
		textBlockIndex = -1
	}
	nextSyntheticToolCallID := func() string {
		for {
			id := fmt.Sprintf("call_agw_%d", syntheticToolCallID)
			syntheticToolCallID++
			if _, exists := toolCallIDs[id]; exists {
				continue
			}
			toolCallIDs[id] = struct{}{}
			return id
		}
	}
	closeToolBlocks := func() {
		for _, key := range toolBlockOrder {
			block := toolBlocks[key]
			if block == nil || block.closed {
				continue
			}
			if !block.started && block.name != "" {
				if block.id == "" {
					block.id = nextSyntheticToolCallID()
				}
				block.blockIndex = allocateBlockIndex()
				writeSSEEvent(w, "content_block_start", map[string]any{
					"type": "content_block_start", "index": block.blockIndex,
					"content_block": map[string]any{
						"type": "tool_use", "id": block.id, "name": block.name, "input": map[string]any{},
					},
				})
				block.started = true
				finalStopReason = "tool_use"
				emittedToolUse = true
			}
			if block.started {
				if block.pendingArguments.Len() > 0 {
					writeSSEEvent(w, "content_block_delta", map[string]any{
						"type": "content_block_delta", "index": block.blockIndex,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": block.pendingArguments.String()},
					})
					block.pendingArguments.Reset()
				}
				writeSSEEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": block.blockIndex})
			} else {
				h.logger.Debug(h.Name()+": dropping incomplete streamed tool call",
					zap.Int("tool_call_index", key),
					zap.Bool("has_id", block.id != ""),
					zap.Bool("has_name", block.name != ""),
				)
			}
			block.closed = true
		}
	}
	toolBlocksHaveCompleteInput := func() bool {
		found := false
		for _, block := range toolBlocks {
			if block == nil || block.closed {
				continue
			}
			found = true
			if block.name == "" || !json.Valid([]byte(block.arguments.String())) {
				return false
			}
		}
		return found
	}
	hasOpenToolBlock := func() bool {
		for _, block := range toolBlocks {
			if block != nil && !block.closed {
				return true
			}
		}
		return false
	}
	writeText := func(text string) {
		if text == "" {
			return
		}
		if !textBlockStarted {
			textBlockIndex = allocateBlockIndex()
			writeSSEEvent(w, "content_block_start", map[string]any{
				"type": "content_block_start", "index": textBlockIndex,
				"content_block": map[string]string{"type": "text", "text": ""},
			})
			textBlockStarted = true
		}
		writeSSEEvent(w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": textBlockIndex,
			"delta": map[string]string{"type": "text_delta", "text": text},
		})
		flusher.Flush()
	}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			httplog.Error(h.logger, "http request failed", r, http.StatusOK, fmt.Errorf("receive stream chunk: %w", err),
				zap.String("protocol", h.Name()),
				zap.String("model", chatReq.Model),
				zap.Int("chunks_received", chunkCount),
			)
			writeSSEEvent(w, "error", anthropicErrorResponse{
				Type: "error",
				Error: anthropicErrorBody{
					Type:    "api_error",
					Message: err.Error(),
				},
			})
			flusher.Flush()
			return
		}
		chunkCount++
		if nativeEvents := provider.AnthropicStreamEventsFromMessage(chunk); len(nativeEvents) > 0 {
			for _, nativeEvent := range nativeEvents {
				data := nativeEvent.Data
				sourceIndex, hasIndex := anthropicSSEEventIndex(data)
				switch nativeEvent.Event {
				case "content_block_start":
					closeReasoningBlock()
					closeTextBlock()
					closeToolBlocks()
					targetIndex := allocateBlockIndex()
					if hasIndex {
						nativeBlockIndices[sourceIndex] = targetIndex
					}
					data = rewriteAnthropicSSEEventIndex(data, targetIndex)
				case "content_block_delta":
					targetIndex, ok := nativeBlockIndices[sourceIndex]
					switch {
					case hasIndex && ok:
						data = rewriteAnthropicSSEEventIndex(data, targetIndex)
					case hasIndex && textBlockStarted:
						// citations_delta belongs to the currently open text block,
						// whose downstream index may have been remapped.
						data = rewriteAnthropicSSEEventIndex(data, textBlockIndex)
					default:
						// The upstream index has no downstream block. Forwarding it
						// would point the client at a block that was never started.
						h.logger.Debug(h.Name()+": dropping unmappable native stream event",
							zap.String("event", nativeEvent.Event),
							zap.Int("upstream_index", sourceIndex),
						)
						continue
					}
				case "content_block_stop":
					targetIndex, ok := nativeBlockIndices[sourceIndex]
					if !hasIndex || !ok {
						h.logger.Debug(h.Name()+": dropping unmappable native stream event",
							zap.String("event", nativeEvent.Event),
							zap.Int("upstream_index", sourceIndex),
						)
						continue
					}
					data = rewriteAnthropicSSEEventIndex(data, targetIndex)
					delete(nativeBlockIndices, sourceIndex)
				}
				writeRawSSEEvent(w, nativeEvent.Event, data)
			}
			flusher.Flush()
			continue
		}
		handledStructuredReasoning := false
		if chunk != nil {
			for _, part := range provider.ReasoningPartsFromMessage(chunk) {
				sourceIndex := 0
				if part.StreamingMeta != nil {
					sourceIndex = part.StreamingMeta.Index
				}
				switch part.Type {
				case schema.ChatMessagePartTypeReasoning:
					handledStructuredReasoning = true
					if textBlockStarted || emittedToolUse || part.Reasoning == nil {
						continue
					}
					if reasoningBlockStarted && reasoningSourceIndex != sourceIndex {
						closeReasoningBlock()
					}
					if !reasoningBlockStarted {
						reasoningBlockIndex = allocateBlockIndex()
						reasoningSourceIndex = sourceIndex
						writeSSEEvent(w, "content_block_start", map[string]any{
							"type": "content_block_start", "index": reasoningBlockIndex,
							"content_block": map[string]string{"type": "thinking", "thinking": "", "signature": ""},
						})
						reasoningBlockStarted = true
					}
					if part.Reasoning.Text != "" {
						reasoningContent.WriteString(part.Reasoning.Text)
						writeSSEEvent(w, "content_block_delta", map[string]any{
							"type": "content_block_delta", "index": reasoningBlockIndex,
							"delta": map[string]string{"type": "thinking_delta", "thinking": part.Reasoning.Text},
						})
					}
					if part.Reasoning.Signature != "" {
						reasoningSignature += part.Reasoning.Signature
					}
				case provider.ChatMessagePartTypeEncryptedReasoning:
					handledStructuredReasoning = true
					if textBlockStarted || emittedToolUse {
						continue
					}
					data := provider.EncryptedReasoningData(part)
					if data == "" {
						continue
					}
					closeReasoningBlock()
					idx := allocateBlockIndex()
					writeSSEEvent(w, "content_block_start", map[string]any{
						"type": "content_block_start", "index": idx,
						"content_block": map[string]string{
							"type": "redacted_thinking", "data": data,
						},
					})
					writeSSEEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
				case provider.ChatMessagePartTypeReasoningEnd:
					handledStructuredReasoning = true
					if reasoningBlockStarted && reasoningSourceIndex == sourceIndex {
						closeReasoningBlock()
					}
				}
			}
		}

		if chunk != nil && !handledStructuredReasoning && chunk.ReasoningContent != "" && !textBlockStarted && !emittedToolUse {
			// Legacy chat providers expose reasoning only as a flat string. Keep
			// that path valid while structured parts preserve real signatures for
			// Anthropic-compatible providers.
			if !reasoningBlockStarted {
				reasoningBlockIndex = allocateBlockIndex()
				reasoningSourceIndex = 0
				writeSSEEvent(w, "content_block_start", map[string]any{
					"type": "content_block_start", "index": reasoningBlockIndex,
					"content_block": map[string]string{"type": "thinking", "thinking": "", "signature": ""},
				})
				reasoningBlockStarted = true
			}
			reasoningContent.WriteString(chunk.ReasoningContent)
			writeSSEEvent(w, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": reasoningBlockIndex,
				"delta": map[string]string{"type": "thinking_delta", "thinking": chunk.ReasoningContent},
			})
		}
		flusher.Flush()

		if text := extractText(chunk); text != "" {
			closeReasoningBlock()
			if toolBlocksHaveCompleteInput() {
				// A following text chunk is the only generic signal that an
				// OpenAI-compatible tool-call argument stream is complete. Once
				// every open call contains a complete JSON value, close the tool
				// blocks so subsequent text remains genuinely streaming.
				closeToolBlocks()
			}
			if hasOpenToolBlock() {
				// Some OpenAI-compatible providers interleave text between JSON
				// fragments of one indexed tool call. Anthropic content blocks are
				// sequential, so retain the text until the tool block can be closed
				// instead of splitting one call into duplicate tool_use blocks.
				deferredText.WriteString(text)
			} else {
				if deferredText.Len() > 0 {
					text = deferredText.String() + text
					deferredText.Reset()
				}
				writeText(text)
			}
		}

		// Accumulate streamed tool-call fragments into one content block per
		// tool-call index. OpenAI-compatible providers send id+name in the first
		// fragment and stream argument deltas afterward; emitting a block per
		// fragment would corrupt the tool call into many empty tool_use blocks.
		for _, tc := range chunk.ToolCalls {
			closeReasoningBlock()
			closeTextBlock()
			if name := strings.TrimSpace(tc.Function.Name); name != "" {
				toolNames[name] = struct{}{}
			}
			if tc.Index == nil {
				// Provider delivered the whole tool call in a single fragment.
				if strings.TrimSpace(tc.Function.Name) == "" {
					h.logger.Debug(h.Name() + ": dropping streamed tool call without a name")
					continue
				}
				if tc.ID == "" {
					tc.ID = nextSyntheticToolCallID()
				} else {
					toolCallIDs[tc.ID] = struct{}{}
				}
				idx := allocateBlockIndex()
				writeSSEEvent(w, "content_block_start", map[string]any{
					"type": "content_block_start", "index": idx,
					"content_block": map[string]any{
						"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{},
					},
				})
				if tc.Function.Arguments != "" {
					writeSSEEvent(w, "content_block_delta", map[string]any{
						"type": "content_block_delta", "index": idx,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
					})
				}
				writeSSEEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
				finalStopReason = "tool_use"
				emittedToolUse = true
				flusher.Flush()
				continue
			}
			block, exists := toolBlocks[*tc.Index]
			if !exists {
				block = &streamToolBlock{}
				toolBlocks[*tc.Index] = block
				toolBlockOrder = append(toolBlockOrder, *tc.Index)
			}
			if block.closed {
				h.logger.Debug(h.Name()+": dropping tool fragment for a closed block",
					zap.Int("tool_call_index", *tc.Index),
				)
				continue
			}
			if block.id == "" {
				block.id = tc.ID
				if block.id != "" {
					toolCallIDs[block.id] = struct{}{}
				}
			}
			if block.name == "" {
				block.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				block.arguments.WriteString(tc.Function.Arguments)
				block.pendingArguments.WriteString(tc.Function.Arguments)
			}
			// Anthropic requires id and name in content_block_start. Some
			// OpenAI-compatible streams omit either value from their first
			// indexed fragment, so buffer arguments until both arrive.
			if !block.started && block.id != "" && block.name != "" {
				block.blockIndex = allocateBlockIndex()
				writeSSEEvent(w, "content_block_start", map[string]any{
					"type": "content_block_start", "index": block.blockIndex,
					"content_block": map[string]any{
						"type": "tool_use", "id": block.id, "name": block.name, "input": map[string]any{},
					},
				})
				block.started = true
				finalStopReason = "tool_use"
				emittedToolUse = true
			}
			if block.started && block.pendingArguments.Len() > 0 {
				writeSSEEvent(w, "content_block_delta", map[string]any{
					"type": "content_block_delta", "index": block.blockIndex,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": block.pendingArguments.String()},
				})
				block.pendingArguments.Reset()
			}
			flusher.Flush()
		}
		if deferredText.Len() > 0 && toolBlocksHaveCompleteInput() {
			// Text may have arrived between the final two argument fragments and
			// there may be no later text chunk to trigger the boundary.
			closeToolBlocks()
			text := deferredText.String()
			deferredText.Reset()
			writeText(text)
		}

		if chunk != nil && chunk.ResponseMeta != nil {
			if chunk.ResponseMeta.FinishReason != "" {
				reason := mapAnthropicStopReason(chunk.ResponseMeta.FinishReason)
				if reason == "tool_use" && !emittedToolUse {
					reason = "end_turn"
				}
				if reason != "" {
					finalStopReason = reason
				}
			}
			if chunk.ResponseMeta.Usage != nil && chunk.ResponseMeta.Usage.CompletionTokens > 0 {
				finalOutputTokens = chunk.ResponseMeta.Usage.CompletionTokens
				usageFinalized = true
			}
			if chunk.ResponseMeta.Usage != nil && chunk.ResponseMeta.Usage.PromptTokens > 0 {
				finalInputTokens = chunk.ResponseMeta.Usage.PromptTokens
				usageFinalized = true
			}
			if chunk.ResponseMeta.Usage != nil && chunk.ResponseMeta.Usage.PromptTokenDetails.CachedTokens > 0 {
				finalCachedTokens = chunk.ResponseMeta.Usage.PromptTokenDetails.CachedTokens
			}
			if chunk.ResponseMeta.Usage != nil && chunk.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens > 0 {
				finalReasoningTokens = chunk.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens
			}
		}
	}

	h.logger.Debug(h.Name()+": stream completed",
		zap.String("model", model),
		zap.Int("chunks", chunkCount),
	)
	closeTextBlock()
	closeReasoningBlock()
	closeToolBlocks()
	if deferredText.Len() > 0 {
		writeText(deferredText.String())
		closeTextBlock()
	}
	writeSSEEvent(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": finalStopReason, "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": finalOutputTokens},
	})
	writeSSEEvent(w, "message_stop", map[string]any{"type": "message_stop"})
	flusher.Flush()
	recordAnthropicToolNameSet(r, toolNames)
	usage.SpanFromContext(r.Context()).SetExtension(usage.LLMExtension{
		InputTokens:     usage.Int(finalInputTokens),
		OutputTokens:    usage.Int(finalOutputTokens),
		TotalTokens:     usage.Int(finalInputTokens + finalOutputTokens),
		CachedTokens:    usage.Int(finalCachedTokens),
		ReasoningTokens: usage.Int(finalReasoningTokens),
		UsageFinalized:  usage.Bool(usageFinalized),
	})
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
