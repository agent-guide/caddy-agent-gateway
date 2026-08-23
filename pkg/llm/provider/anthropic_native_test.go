package provider_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
	"github.com/cloudwego/eino/schema"
)

func TestAnthropicNativeExtrasConcat(t *testing.T) {
	first := anthropicbase.AttachAnthropicStreamEvent(nil, "content_block_start", json.RawMessage(`{"index":0}`))
	anthropicbase.AttachAnthropicContentBlocks(first, []json.RawMessage{
		json.RawMessage(`{"type":"server_tool_use"}`),
		json.RawMessage(`{"type":"text","text":"done"}`),
	})
	second := anthropicbase.AttachAnthropicStreamEvent(nil, "content_block_stop", json.RawMessage(`{"index":0}`))

	merged, err := schema.ConcatMessages([]*schema.Message{first, second})
	if err != nil {
		t.Fatalf("ConcatMessages() error = %v", err)
	}
	if events := anthropicbase.AnthropicStreamEventsFromMessage(merged); len(events) != 0 {
		t.Fatalf("transport-only stream events remained after concat: %+v", events)
	}
	if blocks := anthropicbase.AnthropicContentBlocksFromMessage(merged); len(blocks) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(blocks))
	}
}

func TestProtocolStateRejectsUnregisteredDialect(t *testing.T) {
	msg := &schema.Message{Role: schema.Assistant}
	provider.AttachMessageProtocolState(msg, &provider.ProtocolState{Envelopes: []provider.NativeEnvelope{{
		Dialect: provider.ProtocolDialect("unregistered-fixture"), Scope: provider.NativeScopeMessageHistory,
		Kind: provider.NativeKindContentBlock, Raw: json.RawMessage(`{"type":"fixture"}`),
	}}})
	err := provider.FoldMessageProtocolState(msg)
	if err == nil || !strings.Contains(err.Error(), "no registered codec") {
		t.Fatalf("error = %v", err)
	}
}

func TestMessageCarrierRejectsRequestScope(t *testing.T) {
	msg := &schema.Message{Role: schema.Assistant}
	provider.AttachMessageProtocolState(msg, &provider.ProtocolState{Envelopes: []provider.NativeEnvelope{{
		Dialect: provider.ProtocolDialectAnthropic, Scope: provider.NativeScopeRequest,
		Kind: provider.NativeKindToolDefinition, Raw: json.RawMessage(`{"type":"web_search_20250305","name":"web_search"}`),
	}}})
	if provider.ProtocolStateFromMessage(msg) == nil {
		t.Fatal("fixture request state was not attached")
	}
	_, err := provider.MergeMessageProtocolStates(provider.ProtocolStateFromMessage(msg))
	if err == nil || !strings.Contains(err.Error(), "request envelope") {
		t.Fatalf("concat error = %v", err)
	}
}

func TestAnthropicRelayStreamConcatFoldsTransportState(t *testing.T) {
	fixtures := []struct {
		event string
		data  string
	}{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1"}}`},
		{"content_block_start", `{"type":"content_block_start","index":4,"content_block":{"type":"text","text":"","future":"preserved"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":4,"delta":{"type":"text_delta","text":"hel"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":4,"delta":{"type":"text_delta","text":"lo"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":4}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}
	chunks := make([]*schema.Message, 0, len(fixtures))
	for _, fixture := range fixtures {
		chunks = append(chunks, anthropicbase.AttachAnthropicRelayStreamEvent(nil, fixture.event, json.RawMessage(fixture.data)))
	}
	merged, err := schema.ConcatMessages(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if events := anthropicbase.AnthropicRelayStreamEventsFromMessage(merged); len(events) != 0 {
		t.Fatalf("transport events retained: %+v", events)
	}
	blocks := anthropicbase.AnthropicContentBlocksFromMessage(merged)
	if len(blocks) != 1 {
		t.Fatalf("history blocks = %d, want 1", len(blocks))
	}
	var block map[string]any
	if err := json.Unmarshal(blocks[0], &block); err != nil {
		t.Fatal(err)
	}
	if block["text"] != "hello" || block["future"] != "preserved" {
		t.Fatalf("folded block = %#v", block)
	}
}

func TestPlainTextRelayConcatDoesNotAttachNativeHistory(t *testing.T) {
	chunks := []*schema.Message{
		anthropicbase.AttachAnthropicRelayStreamEvent(nil, "message_start", json.RawMessage(`{"type":"message_start","message":{"id":"msg_1"}}`)),
		anthropicbase.AttachAnthropicRelayStreamEvent(nil, "content_block_start", json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)),
		schema.AssistantMessage("hello", nil),
		anthropicbase.AttachAnthropicRelayStreamEvent(nil, "content_block_delta", json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`)),
		anthropicbase.AttachAnthropicRelayStreamEvent(nil, "content_block_stop", json.RawMessage(`{"type":"content_block_stop","index":0}`)),
		anthropicbase.AttachAnthropicRelayStreamEvent(nil, "message_stop", json.RawMessage(`{"type":"message_stop"}`)),
	}
	merged, err := schema.ConcatMessages(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Content != "hello" {
		t.Fatalf("content = %q, want hello", merged.Content)
	}
	if state := provider.ProtocolStateFromMessage(merged); state != nil {
		t.Fatalf("plain text concat retained native state: %+v", state)
	}
}

func TestHasAnthropicNativeContentDoesNotExposeStoredSlice(t *testing.T) {
	msg := anthropicbase.AttachAnthropicContentBlocks(schema.UserMessage("history"), []json.RawMessage{json.RawMessage(`{"type":"document"}`)})
	if !anthropicbase.HasAnthropicNativeContent([]*schema.Message{msg}) {
		t.Fatal("HasAnthropicNativeContent() = false, want true")
	}
	blocks := anthropicbase.AnthropicContentBlocksFromMessage(msg)
	blocks[0][0] = '['
	if got := string(anthropicbase.AnthropicContentBlocksFromMessage(msg)[0]); got != `{"type":"document"}` {
		t.Fatalf("stored block mutated through returned slice: %s", got)
	}
}

func TestHasAnthropicNativeReasoning(t *testing.T) {
	authentic := provider.AttachReasoningParts(&schema.Message{Role: schema.Assistant},
		provider.NewReasoningOutputPart("inspect", "authentic-signature", nil))
	if !anthropicbase.HasAnthropicNativeReasoning([]*schema.Message{authentic}) {
		t.Fatal("authentic signature was not detected")
	}

	gateway := provider.AttachReasoningParts(&schema.Message{Role: schema.Assistant},
		provider.NewReasoningOutputPart("inspect", provider.GatewayThinkingSignature("inspect"), nil))
	if anthropicbase.HasAnthropicNativeReasoning([]*schema.Message{gateway}) {
		t.Fatal("gateway placeholder signature was treated as native state")
	}

	encrypted := provider.AttachReasoningParts(&schema.Message{Role: schema.Assistant},
		provider.NewEncryptedReasoningOutputPart("opaque", nil))
	if !anthropicbase.HasAnthropicNativeReasoning([]*schema.Message{encrypted}) {
		t.Fatal("encrypted reasoning was not detected")
	}
}
