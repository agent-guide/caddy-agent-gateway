package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

const (
	maxDeferredTextBytes = 1 << 20
	maxToolArgumentBytes = 1 << 20
)

type anthropicStreamEvent struct {
	Event string
	Data  json.RawMessage
}

type providerStreamEvent struct {
	Generic *schema.Message
	Native  *provider.AnthropicStreamEvent
}

type streamEventSink interface {
	Emit(context.Context, anthropicStreamEvent) error
}

type streamEncoderOptions struct {
	Model     string
	MessageID string
}

type streamToolBlock struct {
	blockIndex       int
	started          bool
	closed           bool
	id               string
	name             string
	arguments        strings.Builder
	pendingArguments []string
	identityBuffered bool
}

// anthropicStreamEncoder is the sole owner of downstream message, block and
// terminal state. HTTP and provider adapters only feed typed events into it.
type anthropicStreamEncoder struct {
	ctx       context.Context
	sink      streamEventSink
	lifecycle responseLifecycle
	options   streamEncoderOptions

	opened  bool
	started bool
	ended   bool

	nextBlockIndex int
	activeKind     string
	activeIndex    int

	reasoningSourceIndex int
	reasoningSignature   string
	reasoningContent     strings.Builder

	nativeBlockIndices map[int]int
	toolBlocks         map[int]*streamToolBlock
	toolBlockOrder     []int
	toolCallIDs        map[string]struct{}
	deferredText       strings.Builder

	stopReason      string
	inputTokens     int
	outputTokens    int
	cachedTokens    int
	reasoningTokens int
	usageObserved   bool
	usageFinalized  bool
	toolNames       map[string]struct{}
}

func newAnthropicStreamEncoder(ctx context.Context, options streamEncoderOptions, sink streamEventSink, lifecycle responseLifecycle) *anthropicStreamEncoder {
	return &anthropicStreamEncoder{
		ctx:                  ctx,
		options:              options,
		sink:                 sink,
		lifecycle:            lifecycle,
		activeIndex:          -1,
		reasoningSourceIndex: -1,
		nativeBlockIndices:   map[int]int{},
		toolBlocks:           map[int]*streamToolBlock{},
		toolCallIDs:          map[string]struct{}{},
		stopReason:           "end_turn",
		toolNames:            map[string]struct{}{},
	}
}

func (e *anthropicStreamEncoder) Open() error {
	if e.opened || e.ended {
		return fmt.Errorf("open encoder: %w", errResponseLifecycleFinalized)
	}
	e.opened = true
	return nil
}

func (e *anthropicStreamEncoder) Accept(event providerStreamEvent) error {
	if !e.opened || e.ended {
		return fmt.Errorf("accept stream event in invalid encoder state")
	}
	if (event.Generic == nil) == (event.Native == nil) {
		return fmt.Errorf("provider stream event must contain exactly one payload")
	}
	if event.Native != nil {
		return e.acceptNative(*event.Native)
	}
	return e.acceptGeneric(event.Generic)
}

func (e *anthropicStreamEncoder) Finish() error {
	if e.ended {
		return errResponseLifecycleFinalized
	}
	if err := e.ensureMessageStarted(); err != nil {
		return e.fail(err, "sink_error")
	}
	if err := e.closeToolBlocks(true); err != nil {
		return e.fail(err, "invalid_state")
	}
	if e.deferredText.Len() > 0 {
		text := e.deferredText.String()
		e.deferredText.Reset()
		if err := e.writeText(text); err != nil {
			return e.fail(err, "sink_error")
		}
	}
	if err := e.closeActiveBlock(); err != nil {
		return e.fail(err, "sink_error")
	}
	if err := e.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": e.stopReason, "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": e.outputTokens},
	}); err != nil {
		return e.fail(err, "sink_error")
	}
	if err := e.emit("message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return e.fail(err, "sink_error")
	}
	e.ended = true
	e.observeUsage()
	return e.lifecycle.Finish(responseFinish{StatusCode: 200, Outcome: "completed"})
}

func (e *anthropicStreamEncoder) Fail(err error) error {
	return e.fail(err, "provider_stream_failed")
}

func (e *anthropicStreamEncoder) Cancel(err error) error {
	if e.ended {
		return errResponseLifecycleFinalized
	}
	e.ended = true
	e.observeUsage()
	if e.started {
		_ = e.emitError(err)
	}
	return e.lifecycle.Cancel(responseFailure{StatusCode: 499, Outcome: "client_cancel", ErrorType: "client_cancelled"})
}

func (e *anthropicStreamEncoder) fail(cause error, errorType string) error {
	if e.ended {
		return errResponseLifecycleFinalized
	}
	e.ended = true
	e.observeUsage()
	if e.started {
		_ = e.emitError(cause)
	}
	status := 502
	outcome := "upstream_stream_error"
	if errors.Is(cause, context.Canceled) {
		status = 499
	}
	if errorType == "invalid_state" || errorType == "sink_error" {
		outcome = errorType
	}
	if err := e.lifecycle.Fail(responseFailure{StatusCode: status, Outcome: outcome, ErrorType: errorType}); err != nil {
		return err
	}
	return cause
}

func (e *anthropicStreamEncoder) ensureMessageStarted() error {
	if e.started {
		return nil
	}
	if e.options.MessageID == "" {
		return fmt.Errorf("message id is empty")
	}
	if err := e.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": e.options.MessageID, "type": "message", "role": "assistant",
			"model": e.options.Model, "content": []any{}, "stop_reason": nil,
			"usage": map[string]int{"input_tokens": e.inputTokens, "output_tokens": 0},
		},
	}); err != nil {
		return err
	}
	e.started = true
	return nil
}

func (e *anthropicStreamEncoder) acceptNative(event provider.AnthropicStreamEvent) error {
	switch event.Event {
	case "ping":
		if !e.started {
			return nil
		}
		return e.sink.Emit(e.ctx, anthropicStreamEvent{Event: event.Event, Data: append(json.RawMessage(nil), event.Data...)})
	case "error":
		return e.fail(fmt.Errorf("upstream anthropic error event"), "provider_stream_failed")
	case "message_start":
		// Message-level native production becomes authoritative in phase 3. The
		// phase-1 adapter recognizes the type but normalized mode owns identity.
		return e.ensureMessageStarted()
	case "message_delta":
		return e.acceptNativeMessageDelta(event.Data)
	case "message_stop":
		return nil
	}
	if err := e.ensureMessageStarted(); err != nil {
		return err
	}
	sourceIndex, hasIndex := anthropicSSEEventIndex(event.Data)
	switch event.Event {
	case "content_block_start":
		if !hasIndex {
			return fmt.Errorf("native content_block_start has no index")
		}
		if _, exists := e.nativeBlockIndices[sourceIndex]; exists {
			return fmt.Errorf("native block index %d started twice", sourceIndex)
		}
		if err := e.closeToolBlocks(true); err != nil {
			return err
		}
		if err := e.closeActiveBlock(); err != nil {
			return err
		}
		target := e.allocateBlockIndex()
		e.nativeBlockIndices[sourceIndex] = target
		e.activeKind, e.activeIndex = "native_block", target
		return e.sink.Emit(e.ctx, anthropicStreamEvent{Event: event.Event, Data: rewriteAnthropicSSEEventIndex(event.Data, target)})
	case "content_block_delta":
		target, ok := e.nativeBlockIndices[sourceIndex]
		if !ok && e.activeKind == "text" {
			target, ok = e.activeIndex, true
		}
		if !hasIndex || !ok || target != e.activeIndex {
			return fmt.Errorf("native delta references unopened block index %d", sourceIndex)
		}
		return e.sink.Emit(e.ctx, anthropicStreamEvent{Event: event.Event, Data: rewriteAnthropicSSEEventIndex(event.Data, target)})
	case "content_block_stop":
		target, ok := e.nativeBlockIndices[sourceIndex]
		if !hasIndex || !ok || target != e.activeIndex {
			return fmt.Errorf("native stop references unopened block index %d", sourceIndex)
		}
		if err := e.sink.Emit(e.ctx, anthropicStreamEvent{Event: event.Event, Data: rewriteAnthropicSSEEventIndex(event.Data, target)}); err != nil {
			return err
		}
		delete(e.nativeBlockIndices, sourceIndex)
		e.activeKind, e.activeIndex = "", -1
		return nil
	default:
		if !e.started {
			return fmt.Errorf("native event %q before message_start", event.Event)
		}
		return e.sink.Emit(e.ctx, anthropicStreamEvent{Event: event.Event, Data: append(json.RawMessage(nil), event.Data...)})
	}
}

func (e *anthropicStreamEncoder) acceptNativeMessageDelta(data json.RawMessage) error {
	var payload struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			InputTokens   int `json:"input_tokens"`
			OutputTokens  int `json:"output_tokens"`
			CacheCreation int `json:"cache_creation_input_tokens"`
			CacheRead     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("decode native message_delta: %w", err)
	}
	if payload.Delta.StopReason != "" {
		e.stopReason = mapAnthropicStopReason(payload.Delta.StopReason)
	}
	if payload.Usage.InputTokens != 0 || payload.Usage.OutputTokens != 0 || payload.Usage.CacheCreation != 0 || payload.Usage.CacheRead != 0 {
		e.inputTokens = payload.Usage.InputTokens
		e.outputTokens = payload.Usage.OutputTokens
		e.cachedTokens = payload.Usage.CacheRead
		e.usageObserved = true
		e.usageFinalized = true
	}
	return nil
}

func (e *anthropicStreamEncoder) acceptGeneric(chunk *schema.Message) error {
	if chunk == nil {
		return nil
	}
	e.observeChunkMeta(chunk)
	if err := e.ensureMessageStarted(); err != nil {
		return err
	}
	if err := e.acceptReasoning(chunk); err != nil {
		return err
	}
	if text := extractText(chunk); text != "" {
		if err := e.closeReasoning(); err != nil {
			return err
		}
		if e.toolBlocksHaveCompleteInput() {
			if err := e.closeToolBlocks(false); err != nil {
				return err
			}
		}
		if e.hasOpenToolBlock() {
			if e.deferredText.Len()+len(text) > maxDeferredTextBytes {
				return fmt.Errorf("deferred_text buffer exceeds %d bytes", maxDeferredTextBytes)
			}
			e.deferredText.WriteString(text)
		} else if err := e.writeText(text); err != nil {
			return err
		}
	}
	for _, call := range chunk.ToolCalls {
		if err := e.acceptToolCall(call); err != nil {
			return err
		}
	}
	if e.deferredText.Len() > 0 && e.toolBlocksHaveCompleteInput() {
		if err := e.closeToolBlocks(false); err != nil {
			return err
		}
		text := e.deferredText.String()
		e.deferredText.Reset()
		if err := e.writeText(text); err != nil {
			return err
		}
	}
	return nil
}

func (e *anthropicStreamEncoder) acceptReasoning(chunk *schema.Message) error {
	handled := false
	for _, part := range provider.ReasoningPartsFromMessage(chunk) {
		sourceIndex := 0
		if part.StreamingMeta != nil {
			sourceIndex = part.StreamingMeta.Index
		}
		switch part.Type {
		case schema.ChatMessagePartTypeReasoning:
			handled = true
			if part.Reasoning == nil || e.activeKind == "text" || e.activeKind == "tool_use" {
				continue
			}
			if e.activeKind == "reasoning" && e.reasoningSourceIndex != sourceIndex {
				if err := e.closeReasoning(); err != nil {
					return err
				}
			}
			if e.activeKind != "reasoning" {
				if err := e.openReasoning(sourceIndex); err != nil {
					return err
				}
			}
			if part.Reasoning.Text != "" {
				e.reasoningContent.WriteString(part.Reasoning.Text)
				if err := e.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": e.activeIndex, "delta": map[string]string{"type": "thinking_delta", "thinking": part.Reasoning.Text}}); err != nil {
					return err
				}
			}
			e.reasoningSignature += part.Reasoning.Signature
		case provider.ChatMessagePartTypeEncryptedReasoning:
			handled = true
			data := provider.EncryptedReasoningData(part)
			if data == "" || e.activeKind == "text" || e.activeKind == "tool_use" {
				continue
			}
			if err := e.closeActiveBlock(); err != nil {
				return err
			}
			index := e.allocateBlockIndex()
			if err := e.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]string{"type": "redacted_thinking", "data": data}}); err != nil {
				return err
			}
			if err := e.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
				return err
			}
		case provider.ChatMessagePartTypeReasoningEnd:
			handled = true
			if e.activeKind == "reasoning" && e.reasoningSourceIndex == sourceIndex {
				if err := e.closeReasoning(); err != nil {
					return err
				}
			}
		}
	}
	if !handled && chunk.ReasoningContent != "" && e.activeKind != "text" && e.activeKind != "tool_use" {
		if e.activeKind != "reasoning" {
			if err := e.openReasoning(0); err != nil {
				return err
			}
		}
		e.reasoningContent.WriteString(chunk.ReasoningContent)
		return e.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": e.activeIndex, "delta": map[string]string{"type": "thinking_delta", "thinking": chunk.ReasoningContent}})
	}
	return nil
}

func (e *anthropicStreamEncoder) acceptToolCall(call schema.ToolCall) error {
	if err := e.closeReasoning(); err != nil {
		return err
	}
	if e.activeKind == "text" {
		if err := e.closeActiveBlock(); err != nil {
			return err
		}
	}
	name := strings.TrimSpace(call.Function.Name)
	if name != "" {
		e.toolNames[name] = struct{}{}
	}
	if call.Index == nil {
		if name == "" {
			return fmt.Errorf("streamed tool call is missing name")
		}
		id := call.ID
		if id == "" {
			id = e.nextToolCallID()
		}
		index := e.allocateBlockIndex()
		if err := e.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}}); err != nil {
			return err
		}
		if call.Function.Arguments != "" {
			if !json.Valid([]byte(call.Function.Arguments)) {
				return fmt.Errorf("complete tool call input is invalid JSON")
			}
			if err := e.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": call.Function.Arguments}}); err != nil {
				return err
			}
		}
		if err := e.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
			return err
		}
		e.stopReason = "tool_use"
		return nil
	}
	block := e.toolBlocks[*call.Index]
	if block == nil {
		block = &streamToolBlock{blockIndex: -1, identityBuffered: call.ID == "" || name == ""}
		e.toolBlocks[*call.Index] = block
		e.toolBlockOrder = append(e.toolBlockOrder, *call.Index)
	}
	if block.closed {
		return fmt.Errorf("tool fragment for closed upstream index %d", *call.Index)
	}
	if block.id == "" {
		block.id = call.ID
	}
	if block.name == "" {
		block.name = name
	}
	if call.Function.Arguments != "" {
		if block.arguments.Len()+len(call.Function.Arguments) > maxToolArgumentBytes {
			return fmt.Errorf("tool_arguments buffer exceeds %d bytes", maxToolArgumentBytes)
		}
		block.arguments.WriteString(call.Function.Arguments)
		block.pendingArguments = append(block.pendingArguments, call.Function.Arguments)
	}
	// Indexed calls may be interleaved. Keep them as upstream buffers and emit
	// complete calls sequentially in first-seen order at a safe boundary.
	return nil
}

func (e *anthropicStreamEncoder) openReasoning(sourceIndex int) error {
	if err := e.closeActiveBlock(); err != nil {
		return err
	}
	index := e.allocateBlockIndex()
	if err := e.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]string{"type": "thinking", "thinking": "", "signature": ""}}); err != nil {
		return err
	}
	e.activeKind, e.activeIndex = "reasoning", index
	e.reasoningSourceIndex = sourceIndex
	return nil
}

func (e *anthropicStreamEncoder) closeReasoning() error {
	if e.activeKind != "reasoning" {
		return nil
	}
	signature := e.reasoningSignature
	if signature == "" {
		signature = gatewayThinkingSignature(e.reasoningContent.String())
	}
	if err := e.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": e.activeIndex, "delta": map[string]string{"type": "signature_delta", "signature": signature}}); err != nil {
		return err
	}
	if err := e.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.activeIndex}); err != nil {
		return err
	}
	e.activeKind, e.activeIndex = "", -1
	e.reasoningSourceIndex = -1
	e.reasoningSignature = ""
	e.reasoningContent.Reset()
	return nil
}

func (e *anthropicStreamEncoder) writeText(text string) error {
	if text == "" {
		return nil
	}
	if e.activeKind != "text" {
		if err := e.closeActiveBlock(); err != nil {
			return err
		}
		index := e.allocateBlockIndex()
		if err := e.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]string{"type": "text", "text": ""}}); err != nil {
			return err
		}
		e.activeKind, e.activeIndex = "text", index
	}
	return e.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": e.activeIndex, "delta": map[string]string{"type": "text_delta", "text": text}})
}

func (e *anthropicStreamEncoder) closeActiveBlock() error {
	if e.activeKind == "" {
		return nil
	}
	if e.activeKind == "reasoning" {
		return e.closeReasoning()
	}
	if e.activeKind == "tool_use" {
		return e.closeToolBlocks(false)
	}
	if err := e.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.activeIndex}); err != nil {
		return err
	}
	e.activeKind, e.activeIndex = "", -1
	return nil
}

func (e *anthropicStreamEncoder) closeToolBlocks(terminal bool) error {
	for _, key := range e.toolBlockOrder {
		block := e.toolBlocks[key]
		if block == nil || block.closed {
			continue
		}
		if block.name == "" {
			return fmt.Errorf("streamed tool call %d is missing name", key)
		}
		if !toolArgumentsComplete(block.arguments.String()) {
			if !terminal {
				return nil
			}
			return fmt.Errorf("streamed tool call %d has incomplete JSON input", key)
		}
		if !block.started {
			if block.id == "" {
				block.id = e.nextToolCallID()
			}
			block.blockIndex = e.allocateBlockIndex()
			if err := e.emit("content_block_start", map[string]any{"type": "content_block_start", "index": block.blockIndex, "content_block": map[string]any{"type": "tool_use", "id": block.id, "name": block.name, "input": map[string]any{}}}); err != nil {
				return err
			}
			block.started = true
		}
		if block.arguments.Len() == 0 {
			block.pendingArguments = append(block.pendingArguments, "{}")
		}
		fragments := block.pendingArguments
		if block.identityBuffered && len(fragments) > 1 {
			fragments = []string{strings.Join(fragments, "")}
		}
		for _, fragment := range fragments {
			if err := e.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": block.blockIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": fragment}}); err != nil {
				return err
			}
		}
		block.pendingArguments = nil
		if err := e.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": block.blockIndex}); err != nil {
			return err
		}
		block.closed = true
		e.stopReason = "tool_use"
	}
	if e.activeKind == "tool_use" {
		e.activeKind, e.activeIndex = "", -1
	}
	return nil
}

func (e *anthropicStreamEncoder) toolBlocksHaveCompleteInput() bool {
	found := false
	for _, block := range e.toolBlocks {
		if block == nil || block.closed {
			continue
		}
		found = true
		if block.name == "" || !toolArgumentsComplete(block.arguments.String()) {
			return false
		}
	}
	return found
}

func toolArgumentsComplete(arguments string) bool {
	return arguments == "" || json.Valid([]byte(arguments))
}

func (e *anthropicStreamEncoder) hasOpenToolBlock() bool {
	for _, block := range e.toolBlocks {
		if block != nil && !block.closed {
			return true
		}
	}
	return false
}

func (e *anthropicStreamEncoder) observeChunkMeta(chunk *schema.Message) {
	if chunk.ResponseMeta == nil {
		return
	}
	if reason := mapAnthropicStopReason(chunk.ResponseMeta.FinishReason); reason != "" {
		if reason != "tool_use" || e.stopReason == "tool_use" {
			e.stopReason = reason
		}
	}
	if observed := provider.UsageFromMessage(chunk); observed.InputTokens != 0 || observed.OutputTokens != 0 || observed.CachedTokens != 0 || observed.ReasoningTokens != 0 {
		e.inputTokens = observed.InputTokens
		e.outputTokens = observed.OutputTokens
		e.cachedTokens = observed.CachedTokens
		e.reasoningTokens = observed.ReasoningTokens
		e.usageObserved = true
		e.usageFinalized = true
	}
}

func (e *anthropicStreamEncoder) observeUsage() {
	if !e.usageObserved {
		return
	}
	e.lifecycle.ObserveUsage(usageObservation{InputTokens: e.inputTokens, OutputTokens: e.outputTokens, CachedTokens: e.cachedTokens, ReasoningTokens: e.reasoningTokens, Final: e.usageFinalized})
}

func (e *anthropicStreamEncoder) allocateBlockIndex() int {
	index := e.nextBlockIndex
	e.nextBlockIndex++
	return index
}

func (e *anthropicStreamEncoder) nextToolCallID() string {
	id := "toolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	e.toolCallIDs[id] = struct{}{}
	return id
}

func newAnthropicMessageID() string {
	return "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func (e *anthropicStreamEncoder) emit(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return e.sink.Emit(e.ctx, anthropicStreamEvent{Event: event, Data: data})
}

func (e *anthropicStreamEncoder) emitError(cause error) error {
	message := "stream failed"
	if cause != nil {
		message = cause.Error()
	}
	return e.emit("error", anthropicErrorResponse{Type: "error", Error: anthropicErrorBody{Type: "api_error", Message: message}})
}
