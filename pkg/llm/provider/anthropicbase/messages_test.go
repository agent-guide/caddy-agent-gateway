package anthropicbase

import (
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

// TestConvertMessagesMergesParallelAssistantToolCalls reproduces the Codex
// parallel-tool-call replay: two separate assistant messages each carrying one
// tool_use, followed by the batched tool_results. The Anthropic API rejects an
// assistant tool_use that is not immediately followed by its tool_result, so
// the two assistant messages must merge into one with both tool_use blocks.
func TestConvertMessagesMergesParallelAssistantToolCalls(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "t1", Function: schema.FunctionCall{Name: "Bash", Arguments: `{"cmd":"a"}`}},
		}},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "t2", Function: schema.FunctionCall{Name: "Bash", Arguments: `{"cmd":"b"}`}},
		}},
		{Role: schema.Tool, ToolCallID: "t1", Content: "out1"},
		{Role: schema.Tool, ToolCallID: "t2", Content: "out2"},
	}

	out := ConvertMessages(msgs, &MessagesRequest{}, false, nil)

	if len(out) != 2 {
		t.Fatalf("message count = %d, want 2 (merged assistant + tool_result user)", len(out))
	}
	if out[0].Role != "assistant" || len(out[0].Content) != 2 {
		t.Fatalf("assistant message = %+v, want one message with 2 tool_use blocks", out[0])
	}
	if out[0].Content[0].Type != "tool_use" || out[0].Content[0].ID != "t1" ||
		out[0].Content[1].Type != "tool_use" || out[0].Content[1].ID != "t2" {
		t.Fatalf("assistant tool_use blocks = %+v, want t1 then t2", out[0].Content)
	}
	if out[1].Role != "user" || len(out[1].Content) != 2 {
		t.Fatalf("user message = %+v, want 2 tool_result blocks", out[1])
	}
	if out[1].Content[0].ToolUseID != "t1" || out[1].Content[1].ToolUseID != "t2" {
		t.Fatalf("tool_result ids = %+v, want t1 then t2", out[1].Content)
	}
}

func TestMessagesResponsePreservesReasoningBlocksForReplay(t *testing.T) {
	resp := (&MessagesResponse{
		Content: []ResponseBlock{
			{Type: "thinking", Thinking: "inspect the repository", Signature: "opaque-signature"},
			{Type: "redacted_thinking", Data: "opaque-redacted-data"},
			{Type: "tool_use", ID: "tool-1", Name: "Read", Input: json.RawMessage(`{"path":"README.md"}`)},
		},
		StopReason: "tool_use",
	}).ToChatResponse()

	msg := resp.Message
	if msg == nil || msg.ReasoningContent != "inspect the repository" {
		t.Fatalf("reasoning content = %#v, want preserved", msg)
	}
	parts := provider.ReasoningPartsFromMessage(msg)
	if len(parts) != 2 {
		t.Fatalf("structured reasoning = %+v, want thinking and encrypted blocks", parts)
	}
	if len(msg.AssistantGenMultiContent) != 0 {
		t.Fatalf("AssistantGenMultiContent = %+v, want gateway reasoning isolated in Extra", msg.AssistantGenMultiContent)
	}
	thinking := parts[0]
	if thinking.Type != schema.ChatMessagePartTypeReasoning || thinking.Reasoning == nil ||
		thinking.Reasoning.Signature != "opaque-signature" {
		t.Fatalf("thinking part = %+v, want original signature", thinking)
	}
	redacted := parts[1]
	if redacted.Type != provider.ChatMessagePartTypeEncryptedReasoning ||
		provider.EncryptedReasoningData(redacted) != "opaque-redacted-data" {
		t.Fatalf("encrypted reasoning part = %+v, want original data", redacted)
	}

	wire := ConvertMessages([]*schema.Message{msg}, &MessagesRequest{}, false, nil)
	if len(wire) != 1 || len(wire[0].Content) != 3 {
		t.Fatalf("wire messages = %+v, want one assistant with 3 blocks", wire)
	}
	if got := wire[0].Content[0]; got.Type != "thinking" || got.Thinking != "inspect the repository" || got.Signature != "opaque-signature" {
		t.Fatalf("replayed thinking = %+v", got)
	}
	if got := wire[0].Content[1]; got.Type != "redacted_thinking" || got.Data != "opaque-redacted-data" {
		t.Fatalf("replayed redacted thinking = %+v", got)
	}
}

func TestConvertMessagesDropsThinkingWithoutAuthenticSignature(t *testing.T) {
	msg := provider.AttachReasoningParts(&schema.Message{
		Role:    schema.Assistant,
		Content: "result",
	},
		provider.NewReasoningOutputPart("synthetic", provider.GatewayThinkingSignature("synthetic"), nil),
		provider.NewReasoningOutputPart("interrupted", "", nil),
		provider.NewReasoningOutputPart("authentic", "opaque-signature", nil),
		provider.NewEncryptedReasoningOutputPart("opaque-redacted-data", nil),
	)

	wire := ConvertMessages([]*schema.Message{msg}, &MessagesRequest{}, false, nil)
	if len(wire) != 1 || len(wire[0].Content) != 3 {
		t.Fatalf("wire messages = %+v, want authentic thinking, redacted thinking, and text", wire)
	}
	if got := wire[0].Content[0]; got.Type != "thinking" || got.Thinking != "authentic" || got.Signature != "opaque-signature" {
		t.Fatalf("thinking block = %+v, want only authentic signed thinking", got)
	}
	if got := wire[0].Content[1]; got.Type != "redacted_thinking" || got.Data != "opaque-redacted-data" {
		t.Fatalf("redacted thinking block = %+v", got)
	}
	if got := wire[0].Content[2]; got.Type != "text" || got.Text != "result" {
		t.Fatalf("text block = %+v", got)
	}
}

func TestConvertAssistantFallsBackToContentWhenMultiContentHasNoVisibleBlocks(t *testing.T) {
	msg := &schema.Message{
		Role:    schema.Assistant,
		Content: "visible fallback",
		AssistantGenMultiContent: []schema.MessageOutputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "   "},
			provider.NewReasoningOutputPart("private", "opaque-signature", nil),
		},
	}
	provider.AttachReasoningParts(msg, provider.NewReasoningOutputPart("private", "opaque-signature", nil))

	wire := ConvertMessages([]*schema.Message{msg}, &MessagesRequest{}, false, nil)
	if len(wire) != 1 || len(wire[0].Content) != 2 {
		t.Fatalf("wire messages = %+v, want thinking and fallback text", wire)
	}
	if got := wire[0].Content[1]; got.Type != "text" || got.Text != "visible fallback" {
		t.Fatalf("fallback block = %+v, want visible Content", got)
	}
}

// TestConvertMessagesMergesConsecutiveUserMessages verifies that leading
// same-role user turns (Codex sends developer + multiple user items) collapse
// into a single alternating user turn.
func TestConvertMessagesMergesConsecutiveUserMessages(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "first"},
		{Role: schema.User, Content: "second"},
		{Role: schema.Assistant, Content: "reply"},
	}

	out := ConvertMessages(msgs, &MessagesRequest{}, false, nil)

	if len(out) != 2 {
		t.Fatalf("message count = %d, want 2", len(out))
	}
	if out[0].Role != "user" || len(out[0].Content) != 2 {
		t.Fatalf("user message = %+v, want 2 merged text blocks", out[0])
	}
	if out[0].Content[0].Text != "first" || out[0].Content[1].Text != "second" {
		t.Fatalf("merged user text = %+v, want first then second", out[0].Content)
	}
	if out[1].Role != "assistant" {
		t.Fatalf("second message role = %q, want assistant", out[1].Role)
	}
}
