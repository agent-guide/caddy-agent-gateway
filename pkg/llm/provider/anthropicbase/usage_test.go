package anthropicbase

import (
	"io"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestMessagesUsageTokenUsageIncludesCacheTokens(t *testing.T) {
	u := MessagesUsage{
		InputTokens:              10,
		OutputTokens:             5,
		CacheReadInputTokens:     100,
		CacheCreationInputTokens: 20,
	}
	got := u.TokenUsage()
	// Prompt tokens follow the eino-ext claude accounting: input + cache read +
	// cache creation, with the cache-read subset broken out as CachedTokens.
	if got.PromptTokens != 130 {
		t.Fatalf("PromptTokens = %d, want 130", got.PromptTokens)
	}
	if got.PromptTokenDetails.CachedTokens != 100 {
		t.Fatalf("CachedTokens = %d, want 100", got.PromptTokenDetails.CachedTokens)
	}
	if got.CompletionTokens != 5 || got.TotalTokens != 135 {
		t.Fatalf("CompletionTokens/TotalTokens = %d/%d, want 5/135", got.CompletionTokens, got.TotalTokens)
	}
}

func TestEmitStreamEventCarriesCacheUsageToFinalChunk(t *testing.T) {
	sr, sw := schema.Pipe[*schema.Message](4)
	state := &StreamState{pendingToolCalls: map[int]*pendingToolCall{}}

	messageStart := `{"message":{"usage":{"input_tokens":3,"cache_read_input_tokens":40,"cache_creation_input_tokens":7}}}`
	if err := EmitStreamEvent("message_start", messageStart, sw, state, "test"); err != nil {
		t.Fatalf("EmitStreamEvent(message_start) error = %v", err)
	}
	messageDelta := `{"usage":{"output_tokens":9},"delta":{"stop_reason":"end_turn"}}`
	if err := EmitStreamEvent("message_delta", messageDelta, sw, state, "test"); err != nil {
		t.Fatalf("EmitStreamEvent(message_delta) error = %v", err)
	}
	sw.Close()

	var msg *schema.Message
	for {
		chunk, err := sr.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		if len(AnthropicRelayStreamEventsFromMessage(chunk)) == 0 {
			msg = chunk
		}
	}
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		t.Fatalf("final chunk has no usage: %+v", msg)
	}
	u := msg.ResponseMeta.Usage
	if u.PromptTokens != 50 || u.CompletionTokens != 9 {
		t.Fatalf("PromptTokens/CompletionTokens = %d/%d, want 50/9", u.PromptTokens, u.CompletionTokens)
	}
	if u.PromptTokenDetails.CachedTokens != 40 {
		t.Fatalf("CachedTokens = %d, want 40", u.PromptTokenDetails.CachedTokens)
	}
}
