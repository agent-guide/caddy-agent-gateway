package anthropicmsg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
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
	Native  *anthropicbase.AnthropicStreamEvent
}

type streamFailureError struct {
	cause     error
	errorType string
}

func (e *streamFailureError) Error() string { return e.cause.Error() }
func (e *streamFailureError) Unwrap() error { return e.cause }

func invalidStreamState(cause error) error {
	return &streamFailureError{cause: cause, errorType: "invalid_state"}
}

func streamSinkFailure(cause error) error {
	return &streamFailureError{cause: cause, errorType: "sink_error"}
}

type streamEventSink interface {
	Emit(context.Context, anthropicStreamEvent) error
}

type streamEncoderOptions struct {
	Model                 string
	MessageID             string
	Mode                  streamMode
	RelayIneligibleReason string
}

type streamMode string

const (
	streamModeNormalized  streamMode = "normalized"
	streamModeNativeRelay streamMode = "native_relay"
)

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
	mu        sync.Mutex
	ctx       context.Context
	sink      streamEventSink
	lifecycle responseLifecycle
	options   streamEncoderOptions

	opened                      bool
	started                     bool
	ended                       bool
	relayComplete               bool
	relayGenericContentObserved bool
	relayNativeContentObserved  bool

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

	stopReason       string
	uncachedTokens   int
	cacheWriteTokens int
	inputTokens      int
	outputTokens     int
	cachedTokens     int
	reasoningTokens  int
	usageObserved    bool
	usageFinalized   bool
	toolNames        map[string]struct{}
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
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.opened || e.ended {
		return fmt.Errorf("open encoder: %w", errResponseLifecycleFinalized)
	}
	e.opened = true
	messageIDSource := "gateway"
	usageSource := "provider_projection"
	if e.options.Mode == streamModeNativeRelay {
		messageIDSource = "upstream"
		usageSource = "native_stream"
	}
	e.lifecycle.ObserveResponse(responseObservation{Mode: string(e.options.Mode), RelayIneligibleReason: e.options.RelayIneligibleReason, MessageIDSource: messageIDSource, UsageSource: usageSource})
	return nil
}

func (e *anthropicStreamEncoder) Accept(event providerStreamEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.opened || e.ended {
		return invalidStreamState(fmt.Errorf("accept stream event in invalid encoder state"))
	}
	if (event.Generic == nil) == (event.Native == nil) {
		return invalidStreamState(fmt.Errorf("provider stream event must contain exactly one payload"))
	}
	var err error
	if event.Native != nil {
		err = e.acceptNative(*event.Native)
	} else if e.options.Mode != streamModeNativeRelay {
		err = e.acceptGeneric(event.Generic)
	} else if hasGenericStreamContent(event.Generic) {
		e.relayGenericContentObserved = true
	}
	if err == nil || e.ended {
		return err
	}
	var classified *streamFailureError
	if errors.As(err, &classified) {
		return err
	}
	return invalidStreamState(err)
}

func (e *anthropicStreamEncoder) Finish() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ended {
		return errResponseLifecycleFinalized
	}
	if !e.opened || !e.started {
		return e.fail(fmt.Errorf("provider stream ended before message_start"), "invalid_state")
	}
	if e.options.Mode == streamModeNativeRelay {
		if !e.started || !e.relayComplete || e.activeKind != "" {
			return e.fail(fmt.Errorf("native relay ended before a complete message lifecycle"), "invalid_state")
		}
		if e.relayGenericContentObserved && !e.relayNativeContentObserved {
			return e.fail(fmt.Errorf("native relay omitted content carried by the generic projection"), "invalid_state")
		}
		e.ended = true
		e.observeUsage()
		return e.lifecycle.Finish(responseFinish{StatusCode: 200, Outcome: "completed"})
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
	e.mu.Lock()
	defer e.mu.Unlock()
	var classified *streamFailureError
	if errors.As(err, &classified) {
		return e.fail(classified.cause, classified.errorType)
	}
	return e.fail(err, "provider_stream_failed")
}

func (e *anthropicStreamEncoder) Cancel(err error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
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

func (e *anthropicStreamEncoder) acceptNative(event anthropicbase.AnthropicStreamEvent) error {
	if e.options.Mode == streamModeNativeRelay {
		return e.acceptRelayNative(event)
	}
	switch event.Event {
	case "ping":
		if !e.started {
			return nil
		}
		return e.emitEvent(anthropicStreamEvent{Event: event.Event, Data: append(json.RawMessage(nil), event.Data...)})
	case "error":
		return e.fail(fmt.Errorf("upstream anthropic error event"), "provider_stream_failed")
	case "message_start":
		// Message-level native production becomes authoritative in phase 3. The
		// phase-1 adapter recognizes the type but normalized mode owns identity.
		if e.started {
			return fmt.Errorf("native message_start repeated")
		}
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
		return e.emitEvent(anthropicStreamEvent{Event: event.Event, Data: rewriteAnthropicSSEEventIndex(event.Data, target)})
	case "content_block_delta":
		target, ok := e.nativeBlockIndices[sourceIndex]
		if !ok && e.activeKind == "text" {
			target, ok = e.activeIndex, true
		}
		if !hasIndex || !ok || target != e.activeIndex {
			return fmt.Errorf("native delta references unopened block index %d", sourceIndex)
		}
		return e.emitEvent(anthropicStreamEvent{Event: event.Event, Data: rewriteAnthropicSSEEventIndex(event.Data, target)})
	case "content_block_stop":
		target, ok := e.nativeBlockIndices[sourceIndex]
		if !hasIndex || !ok || target != e.activeIndex {
			return fmt.Errorf("native stop references unopened block index %d", sourceIndex)
		}
		if err := e.emitEvent(anthropicStreamEvent{Event: event.Event, Data: rewriteAnthropicSSEEventIndex(event.Data, target)}); err != nil {
			return err
		}
		delete(e.nativeBlockIndices, sourceIndex)
		e.activeKind, e.activeIndex = "", -1
		return nil
	default:
		if !e.started {
			return fmt.Errorf("native event %q before message_start", event.Event)
		}
		return e.emitEvent(anthropicStreamEvent{Event: event.Event, Data: append(json.RawMessage(nil), event.Data...)})
	}
}

func (e *anthropicStreamEncoder) acceptRelayNative(event anthropicbase.AnthropicStreamEvent) error {
	if e.relayComplete {
		return fmt.Errorf("native event %q arrived after message_stop", event.Event)
	}
	switch event.Event {
	case "ping":
		if !e.started {
			return nil
		}
		return e.emitEvent(anthropicStreamEvent{Event: event.Event, Data: append(json.RawMessage(nil), event.Data...)})
	case "error":
		return e.fail(fmt.Errorf("upstream anthropic error event"), "provider_stream_failed")
	case "message_start":
		if e.started {
			return fmt.Errorf("native message_start repeated")
		}
		data, err := rewriteNativeMessageStart(event.Data, e.options.Model)
		if err != nil {
			return err
		}
		if err := e.observeNativeMessageStart(data); err != nil {
			return err
		}
		if err := e.emitEvent(anthropicStreamEvent{Event: event.Event, Data: data}); err != nil {
			return err
		}
		e.started = true
		return nil
	case "content_block_start":
		if !e.started || e.activeKind != "" {
			return fmt.Errorf("native relay block start overlaps active block")
		}
		index, ok := anthropicSSEEventIndex(event.Data)
		if !ok {
			return fmt.Errorf("native relay block start has no index")
		}
		var payload struct {
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return err
		}
		if payload.ContentBlock.Type == "tool_use" && (payload.ContentBlock.ID == "" || payload.ContentBlock.Name == "") {
			return fmt.Errorf("native relay tool_use block is missing id or name")
		}
		if payload.ContentBlock.Type == "tool_use" {
			e.toolNames[payload.ContentBlock.Name] = struct{}{}
		}
		e.activeKind, e.activeIndex = "native_block", index
		e.relayNativeContentObserved = true
		return e.emitEvent(anthropicStreamEvent{Event: event.Event, Data: append(json.RawMessage(nil), event.Data...)})
	case "content_block_delta", "content_block_stop":
		index, ok := anthropicSSEEventIndex(event.Data)
		if !ok || e.activeKind == "" || index != e.activeIndex {
			return fmt.Errorf("native relay %s references inactive block %d", event.Event, index)
		}
		if err := e.emitEvent(anthropicStreamEvent{Event: event.Event, Data: append(json.RawMessage(nil), event.Data...)}); err != nil {
			return err
		}
		if event.Event == "content_block_stop" {
			e.activeKind, e.activeIndex = "", -1
		}
		return nil
	case "message_delta":
		if !e.started || e.activeKind != "" {
			return fmt.Errorf("native relay message_delta before blocks close")
		}
		if err := e.acceptNativeMessageDelta(event.Data); err != nil {
			return err
		}
		return e.emitEvent(anthropicStreamEvent{Event: event.Event, Data: append(json.RawMessage(nil), event.Data...)})
	case "message_stop":
		if !e.started || e.activeKind != "" {
			return fmt.Errorf("native relay message_stop before blocks close")
		}
		if err := e.emitEvent(anthropicStreamEvent{Event: event.Event, Data: append(json.RawMessage(nil), event.Data...)}); err != nil {
			return err
		}
		e.relayComplete = true
		return nil
	default:
		if !e.started {
			return fmt.Errorf("native relay event %q before message_start", event.Event)
		}
		return e.emitEvent(anthropicStreamEvent{Event: event.Event, Data: append(json.RawMessage(nil), event.Data...)})
	}
}

func hasGenericStreamContent(chunk *schema.Message) bool {
	if chunk == nil {
		return false
	}
	return chunk.Content != "" || chunk.ReasoningContent != "" || len(chunk.ToolCalls) > 0 || len(provider.ReasoningPartsFromMessage(chunk)) > 0
}

func rewriteNativeMessageStart(data json.RawMessage, model string) (json.RawMessage, error) {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode native message_start: %w", err)
	}
	message, ok := payload["message"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("native message_start has no message")
	}
	id, _ := message["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("native message_start has empty id")
	}
	message["model"] = model
	return json.Marshal(payload)
}

func (e *anthropicStreamEncoder) observeNativeMessageStart(data json.RawMessage) error {
	var payload struct {
		Message struct {
			Usage struct {
				InputTokens   int `json:"input_tokens"`
				OutputTokens  int `json:"output_tokens"`
				CacheCreation int `json:"cache_creation_input_tokens"`
				CacheRead     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	e.uncachedTokens = payload.Message.Usage.InputTokens
	e.cacheWriteTokens = payload.Message.Usage.CacheCreation
	e.inputTokens = e.uncachedTokens + e.cacheWriteTokens + payload.Message.Usage.CacheRead
	e.outputTokens = payload.Message.Usage.OutputTokens
	e.cachedTokens = payload.Message.Usage.CacheRead
	e.usageObserved = true
	return nil
}

func (e *anthropicStreamEncoder) acceptNativeMessageDelta(data json.RawMessage) error {
	var payload struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			InputTokens   *int `json:"input_tokens"`
			OutputTokens  *int `json:"output_tokens"`
			CacheCreation *int `json:"cache_creation_input_tokens"`
			CacheRead     *int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("decode native message_delta: %w", err)
	}
	if payload.Delta.StopReason != "" {
		e.stopReason = mapAnthropicStopReason(payload.Delta.StopReason)
	}
	if payload.Usage.InputTokens != nil || payload.Usage.OutputTokens != nil || payload.Usage.CacheCreation != nil || payload.Usage.CacheRead != nil {
		if payload.Usage.InputTokens != nil {
			e.uncachedTokens = *payload.Usage.InputTokens
		}
		if payload.Usage.CacheCreation != nil {
			e.cacheWriteTokens = *payload.Usage.CacheCreation
		}
		if payload.Usage.CacheRead != nil {
			e.cachedTokens = *payload.Usage.CacheRead
		}
		if payload.Usage.OutputTokens != nil {
			e.outputTokens = *payload.Usage.OutputTokens
		}
		e.inputTokens = e.uncachedTokens + e.cacheWriteTokens + e.cachedTokens
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
	for _, item := range responseItemsFromMessage(chunk) {
		switch item.Block.Type {
		case "text":
			text := item.Block.Text
			if text == "" {
				continue
			}
			if err := e.closeReasoning(); err != nil {
				return err
			}
			if e.toolBlocksHaveCompleteInput() {
				if err := e.closeToolBlocks(false); err != nil {
					return err
				}
			}
			if e.hasOpenToolBlock() {
				byteCount := e.deferredText.Len() + len(text)
				if byteCount > maxDeferredTextBytes {
					return fmt.Errorf("deferred_text buffer would grow to %d bytes, limit %d", byteCount, maxDeferredTextBytes)
				}
				e.deferredText.WriteString(text)
			} else if err := e.writeText(text); err != nil {
				return err
			}
		case "tool_use":
			if item.SourceToolCall != nil {
				if err := e.acceptToolCall(*item.SourceToolCall); err != nil {
					return err
				}
			}
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
		byteCount := block.arguments.Len() + len(call.Function.Arguments)
		if byteCount > maxToolArgumentBytes {
			return fmt.Errorf("tool_arguments buffer would grow to %d bytes, limit %d", byteCount, maxToolArgumentBytes)
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
		e.uncachedTokens = observed.InputTokens - observed.CachedTokens
		if e.uncachedTokens < 0 {
			e.uncachedTokens = 0
		}
		e.cacheWriteTokens = 0
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
	return e.emitEvent(anthropicStreamEvent{Event: event, Data: data})
}

func (e *anthropicStreamEncoder) emitEvent(event anthropicStreamEvent) error {
	if err := e.sink.Emit(e.ctx, event); err != nil {
		return streamSinkFailure(err)
	}
	return nil
}

func (e *anthropicStreamEncoder) emitError(cause error) error {
	message := "stream failed"
	if cause != nil {
		message = cause.Error()
	}
	return e.emit("error", anthropicErrorResponse{Type: "error", Error: anthropicErrorBody{Type: "api_error", Message: message}})
}
