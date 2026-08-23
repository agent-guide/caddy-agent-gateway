package provider

import (
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestAnthropicNativeExtrasConcat(t *testing.T) {
	first := AttachAnthropicStreamEvent(nil, "content_block_start", json.RawMessage(`{"index":0}`))
	AttachAnthropicContentBlocks(first, []json.RawMessage{json.RawMessage(`{"type":"server_tool_use"}`)})
	second := AttachAnthropicStreamEvent(nil, "content_block_stop", json.RawMessage(`{"index":0}`))
	AttachAnthropicContentBlocks(second, []json.RawMessage{json.RawMessage(`{"type":"text","text":"done"}`)})

	merged, err := schema.ConcatMessages([]*schema.Message{first, second})
	if err != nil {
		t.Fatalf("ConcatMessages() error = %v", err)
	}
	if events := AnthropicStreamEventsFromMessage(merged); len(events) != 2 || events[0].Event != "content_block_start" || events[1].Event != "content_block_stop" {
		t.Fatalf("stream events = %+v, want ordered start/stop", events)
	}
	if blocks := AnthropicContentBlocksFromMessage(merged); len(blocks) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(blocks))
	}
}

func TestHasAnthropicNativeContentDoesNotExposeStoredSlice(t *testing.T) {
	msg := AttachAnthropicContentBlocks(schema.UserMessage("history"), []json.RawMessage{json.RawMessage(`{"type":"document"}`)})
	if !HasAnthropicNativeContent([]*schema.Message{msg}) {
		t.Fatal("HasAnthropicNativeContent() = false, want true")
	}
	blocks := AnthropicContentBlocksFromMessage(msg)
	blocks[0][0] = '['
	if got := string(AnthropicContentBlocksFromMessage(msg)[0]); got != `{"type":"document"}` {
		t.Fatalf("stored block mutated through returned slice: %s", got)
	}
}

func TestHasAnthropicNativeReasoning(t *testing.T) {
	authentic := AttachReasoningParts(&schema.Message{Role: schema.Assistant},
		NewReasoningOutputPart("inspect", "authentic-signature", nil))
	if !HasAnthropicNativeReasoning([]*schema.Message{authentic}) {
		t.Fatal("authentic signature was not detected")
	}

	gateway := AttachReasoningParts(&schema.Message{Role: schema.Assistant},
		NewReasoningOutputPart("inspect", GatewayThinkingSignature("inspect"), nil))
	if HasAnthropicNativeReasoning([]*schema.Message{gateway}) {
		t.Fatal("gateway placeholder signature was treated as native state")
	}

	encrypted := AttachReasoningParts(&schema.Message{Role: schema.Assistant},
		NewEncryptedReasoningOutputPart("opaque", nil))
	if !HasAnthropicNativeReasoning([]*schema.Message{encrypted}) {
		t.Fatal("encrypted reasoning was not detected")
	}
}
