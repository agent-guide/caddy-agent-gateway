package anthropicbase

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

func TestCodecDifferentialOverlayPreservesUnknownFields(t *testing.T) {
	codec := anthropicCodec{}
	envelope, err := codec.Capture(provider.NativeCaptureInput{
		Scope: provider.NativeScopeMessageHistory,
		Kind:  provider.NativeKindContentBlock,
		Raw:   json.RawMessage(`{"type":"tool_use","name":"before","input":{"x":1},"future":{"opaque":true}}`),
		Modeled: ModeledObject{
			"type": "tool_use", "name": "before", "input": map[string]any{"x": float64(1)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := codec.Overlay(provider.NativeOverlayInput{Envelope: envelope, Current: ModeledObject{
		"type": "tool_use", "name": "after", "input": map[string]any{"x": float64(1)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(replayed, &got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "after" {
		t.Fatalf("name = %v, want after", got["name"])
	}
	if future, ok := got["future"].(map[string]any); !ok || future["opaque"] != true {
		t.Fatalf("unknown field was not preserved: %#v", got)
	}
}

func TestCodecRejectsWrongProjectionType(t *testing.T) {
	_, err := (anthropicCodec{}).Capture(provider.NativeCaptureInput{
		Scope: provider.NativeScopeRequest, Kind: provider.NativeKindToolDefinition,
		Raw: json.RawMessage(`{"name":"lookup"}`), Modeled: map[string]any{"name": "lookup"},
	})
	if err == nil || !strings.Contains(err.Error(), "wrong modeled projection type") {
		t.Fatalf("error = %v", err)
	}
}

func TestCodecFoldsCompleteStreamIntoReplayableHistory(t *testing.T) {
	events := []provider.NativeEnvelope{
		streamEnvelope("message_start", 0, `{"type":"message_start","message":{"id":"msg_1"}}`),
		streamEnvelope("content_block_start", 0, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"","future":"keep"}}`),
		streamEnvelope("content_block_delta", 0, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`),
		streamEnvelope("content_block_delta", 0, `{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","start_char_index":0}}}`),
		streamEnvelope("content_block_stop", 0, `{"type":"content_block_stop","index":0}`),
		streamEnvelope("content_block_start", 3, `{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}`),
		streamEnvelope("content_block_delta", 3, `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`),
		streamEnvelope("content_block_delta", 3, `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}`),
		streamEnvelope("content_block_stop", 3, `{"type":"content_block_stop","index":3}`),
		streamEnvelope("message_delta", 0, `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`),
		streamEnvelope("message_stop", 0, `{"type":"message_stop"}`),
	}
	folded, err := (anthropicCodec{}).FoldStreamEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(folded) != 2 {
		t.Fatalf("folded blocks = %d, want 2", len(folded))
	}
	var textBlock, toolBlock map[string]any
	if err := json.Unmarshal(folded[0].Raw, &textBlock); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(folded[1].Raw, &toolBlock); err != nil {
		t.Fatal(err)
	}
	if textBlock["text"] != "hello" || textBlock["future"] != "keep" || len(textBlock["citations"].([]any)) != 1 {
		t.Fatalf("text block = %#v", textBlock)
	}
	if input := toolBlock["input"].(map[string]any); input["q"] != "x" {
		t.Fatalf("tool block = %#v", toolBlock)
	}
}

func TestCodecDoesNotPinPlainTextStreamToAnthropicHistory(t *testing.T) {
	folded, err := (anthropicCodec{}).FoldStreamEvents([]provider.NativeEnvelope{
		streamEnvelope("message_start", 0, `{"type":"message_start","message":{"id":"msg_1"}}`),
		streamEnvelope("content_block_start", 0, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		streamEnvelope("content_block_delta", 0, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`),
		streamEnvelope("content_block_stop", 0, `{"type":"content_block_stop","index":0}`),
		streamEnvelope("message_stop", 0, `{"type":"message_stop"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(folded) != 0 {
		t.Fatalf("plain text folded state = %+v, want no native history", folded)
	}
}

func TestCodecIgnoresUnknownMessageEventInsideValidLifecycle(t *testing.T) {
	folded, err := (anthropicCodec{}).FoldStreamEvents([]provider.NativeEnvelope{
		streamEnvelope("message_start", 0, `{"type":"message_start","message":{"id":"msg_1"}}`),
		streamEnvelope("future_message_metadata", 0, `{"type":"future_message_metadata","opaque":true}`),
		streamEnvelope("message_stop", 0, `{"type":"message_stop"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(folded) != 0 {
		t.Fatalf("unknown message metadata folded into history: %+v", folded)
	}
}

func TestCodecRejectsUnknownEventBeforeMessageStart(t *testing.T) {
	_, err := (anthropicCodec{}).FoldStreamEvents([]provider.NativeEnvelope{
		streamEnvelope("future_message_metadata", 0, `{"type":"future_message_metadata"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "pre-message") {
		t.Fatalf("error = %v, want controlled pre-message rejection", err)
	}
}

func TestCodecRejectsOverlappingBlocks(t *testing.T) {
	_, err := (anthropicCodec{}).FoldStreamEvents([]provider.NativeEnvelope{
		streamEnvelope("message_start", 0, `{"type":"message_start","message":{"id":"msg_1"}}`),
		streamEnvelope("content_block_start", 0, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		streamEnvelope("content_block_start", 1, `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "overlapping") {
		t.Fatalf("error = %v", err)
	}
}

func streamEnvelope(event string, index int, raw string) provider.NativeEnvelope {
	return provider.NativeEnvelope{Dialect: provider.ProtocolDialectAnthropic, Scope: provider.NativeScopeStreamEvent,
		Kind: provider.NativeKindStreamEvent, Location: provider.NativeLocation{Event: event, SourceIndex: index}, Raw: json.RawMessage(raw)}
}
