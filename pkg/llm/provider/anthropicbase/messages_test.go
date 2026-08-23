package anthropicbase

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

func TestBuildMessagesRequestUsesObjectSchemaForParameterlessTool(t *testing.T) {
	chatReq := &provider.ChatRequest{
		Model:    "model",
		Messages: []*schema.Message{schema.UserMessage("hello")},
		Options: []model.Option{
			model.WithTools([]*schema.ToolInfo{{Name: "heartbeat", Desc: "Check health"}}),
		},
	}
	state, err := provider.ResolveChatRequest(context.Background(), provider.ProviderConfig{}, chatReq)
	if err != nil {
		t.Fatalf("ResolveChatRequest: %v", err)
	}
	req := BuildMessagesRequest(state, BuildMessagesOptions{})
	if len(req.Tools) != 1 {
		t.Fatalf("tools = %+v, want heartbeat", req.Tools)
	}
	var inputSchema map[string]any
	if err := json.Unmarshal(req.Tools[0].InputSchema, &inputSchema); err != nil {
		t.Fatalf("input_schema = %s: %v", req.Tools[0].InputSchema, err)
	}
	if inputSchema["type"] != "object" {
		t.Fatalf("input_schema = %#v, want object", inputSchema)
	}
}

func TestBuildMessagesRequestReplaysNativeAnthropicTools(t *testing.T) {
	tool := json.RawMessage(`{"type":"web_search_20260209","name":"web_search","max_uses":3}`)
	choice := json.RawMessage(`{"type":"auto","disable_parallel_tool_use":true}`)
	chatReq := &provider.ChatRequest{
		Model:         "model",
		Messages:      []*schema.Message{schema.UserMessage("hello")},
		ProtocolState: NewAnthropicRequestProtocolState([]json.RawMessage{tool}, choice),
	}
	state, err := provider.ResolveChatRequest(context.Background(), provider.ProviderConfig{}, chatReq)
	if err != nil {
		t.Fatalf("ResolveChatRequest: %v", err)
	}
	req := BuildMessagesRequest(state, BuildMessagesOptions{})
	wire, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if string(req.ToolChoice) != string(choice) {
		t.Fatalf("tool_choice = %s, want %s", req.ToolChoice, choice)
	}
	if !bytes.Contains(wire, []byte(`"type":"web_search_20260209"`)) || !bytes.Contains(wire, []byte(`"max_uses":3`)) {
		t.Fatalf("request = %s, want native server tool fields", wire)
	}
	if bytes.Contains(wire, []byte(`"input_schema":null`)) {
		t.Fatalf("request contains null input_schema: %s", wire)
	}
}

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

func TestMessagesResponsePreservesNativeServerToolBlocksForReplay(t *testing.T) {
	payload := []byte(`{
		"content":[
			{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"latest"}},
			{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[{"type":"web_search_result","url":"https://example.com","title":"Example","encrypted_content":"opaque"}]},
			{"type":"text","text":"Result","citations":[{"type":"web_search_result_location","url":"https://example.com","title":"Example","cited_text":"result"}]}
		],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":2,"output_tokens":3}
	}`)
	var response MessagesResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	msg := response.ToChatResponse().Message
	blocks := AnthropicContentBlocksFromMessage(msg)
	if len(blocks) != 3 {
		t.Fatalf("native blocks = %d, want 3", len(blocks))
	}
	wire := ConvertMessages([]*schema.Message{msg}, &MessagesRequest{}, false, nil)
	if len(wire) != 1 || len(wire[0].Content) != 3 {
		t.Fatalf("replayed messages = %+v, want exact native content", wire)
	}
	replayed, err := json.Marshal(wire[0].Content)
	if err != nil {
		t.Fatalf("marshal replayed content: %v", err)
	}
	for _, want := range []string{`"type":"server_tool_use"`, `"type":"web_search_tool_result"`, `"citations"`} {
		if !bytes.Contains(replayed, []byte(want)) {
			t.Fatalf("replayed content = %s, missing %s", replayed, want)
		}
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

func TestConvertMessagesOmitsEmptyToolResultContent(t *testing.T) {
	// Anthropic rejects "content": "" on a tool_result; an empty result must
	// omit the field entirely.
	items := ConvertMessages([]*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "t1", Function: schema.FunctionCall{Name: "Bash", Arguments: "{}"},
		}}},
		{Role: schema.Tool, ToolCallID: "t1", Content: ""},
	}, &MessagesRequest{}, false, nil)
	wire, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	if bytes.Contains(wire, []byte(`"content":""`)) {
		t.Fatalf("empty tool_result content serialized: %s", wire)
	}
	if !bytes.Contains(wire, []byte(`"type":"tool_result"`)) {
		t.Fatalf("tool_result block missing: %s", wire)
	}
}

func TestModeledResponseOnlyPinsAuthenticReasoningToNativeProvider(t *testing.T) {
	// Text, thinking, and tool_use survive the generic message model, so they do
	// not need raw blocks. The authentic thinking signature still requires
	// Anthropic-native routing even though its structured representation survives.
	payload := []byte(`{
		"content":[
			{"type":"thinking","thinking":"hmm","signature":"sig"},
			{"type":"text","text":"hello"},
			{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":1,"output_tokens":2}
	}`)
	var response MessagesResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	msg := response.ToChatResponse().Message
	if blocks := AnthropicContentBlocksFromMessage(msg); len(blocks) != 0 {
		t.Fatalf("native blocks = %d, want none for portable content", len(blocks))
	}
	if HasAnthropicNativeContent([]*schema.Message{msg}) {
		t.Fatal("modeled response unexpectedly retained raw native blocks")
	}
	if !HasAnthropicNativeReasoning([]*schema.Message{msg}) {
		t.Fatal("authentic reasoning did not require Anthropic-native routing")
	}
	if msg.Content != "hello" || len(msg.ToolCalls) != 1 {
		t.Fatalf("generic message = %+v, want text and one tool call", msg)
	}
}

func TestResponseBlockUsesPerTypeModeledFields(t *testing.T) {
	// data is modeled for redacted_thinking, not for text. Treating all struct
	// fields as one union would silently discard it from this future text shape.
	payload := []byte(`{
		"content":[{"type":"text","text":"hi","data":"opaque-future-field"}],
		"stop_reason":"end_turn",
		"usage":{}
	}`)
	var response MessagesResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	msg := response.ToChatResponse().Message
	blocks := AnthropicContentBlocksFromMessage(msg)
	if len(blocks) != 1 || !bytes.Contains(blocks[0], []byte(`"data":"opaque-future-field"`)) {
		t.Fatalf("cross-type field was not retained for native replay: %s", blocks)
	}
}

func TestCitationTextResponsePinsConversationToNativeProvider(t *testing.T) {
	// citations has no generic representation, so this response must stay on an
	// Anthropic-native provider.
	payload := []byte(`{
		"content":[{"type":"text","text":"hi","citations":[{"type":"web_search_result_location","url":"https://example.com"}]}],
		"stop_reason":"end_turn",
		"usage":{}
	}`)
	var response MessagesResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	msg := response.ToChatResponse().Message
	if !HasAnthropicNativeContent([]*schema.Message{msg}) {
		t.Fatal("citation response must require Anthropic-native routing")
	}
}

func TestNullCitationFieldDoesNotPinConversation(t *testing.T) {
	payload := []byte(`{
		"content":[{"type":"text","text":"hi","citations":null}],
		"stop_reason":"end_turn",
		"usage":{}
	}`)
	var response MessagesResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	msg := response.ToChatResponse().Message
	if HasAnthropicNativeContent([]*schema.Message{msg}) {
		t.Fatal("null citations unexpectedly required Anthropic-native routing")
	}
	if msg.Content != "hi" {
		t.Fatalf("message content = %q, want hi", msg.Content)
	}
}

func TestNativeContentReplayKeepsUndecodableBlockVerbatim(t *testing.T) {
	// "id" is modeled as a string; a future block shape that uses another type
	// must still be replayed instead of silently disappearing from the turn.
	opaque := json.RawMessage(`{"type":"future_block","id":42,"payload":{"keep":"me"}}`)
	msg := AttachAnthropicContentBlocks(&schema.Message{Role: schema.Assistant}, []json.RawMessage{opaque})
	items := ConvertMessages([]*schema.Message{msg}, &MessagesRequest{}, false, nil)
	if len(items) != 1 || len(items[0].Content) != 1 {
		t.Fatalf("replayed messages = %+v, want one opaque block", items)
	}
	wire, err := json.Marshal(items[0].Content)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	if !bytes.Contains(wire, []byte(`"payload":{"keep":"me"}`)) || !bytes.Contains(wire, []byte(`"id":42`)) {
		t.Fatalf("opaque block not replayed verbatim: %s", wire)
	}
	if bytes.Contains(wire, []byte(`"type":""`)) {
		t.Fatalf("opaque block overwritten by zero-valued modeled fields: %s", wire)
	}
}

func TestToolDefMarshalAppliesMutationsAndClears(t *testing.T) {
	var tool ToolDef
	if err := json.Unmarshal([]byte(`{"type":"custom","name":"exec_command","description":"run","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}`), &tool); err != nil {
		t.Fatalf("unmarshal tool: %v", err)
	}
	tool.Name = "Bash"
	tool.Description = ""
	wire, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal tool: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatalf("decode marshaled tool: %v", err)
	}
	if string(fields["name"]) != `"Bash"` {
		t.Fatalf("name = %s, want renamed", fields["name"])
	}
	if _, ok := fields["description"]; ok {
		t.Fatalf("cleared description retained: %s", wire)
	}
	if _, ok := fields["cache_control"]; !ok {
		t.Fatalf("unmodeled field dropped: %s", wire)
	}
	if string(fields["type"]) != `"custom"` {
		t.Fatalf("type = %s, want custom preserved", fields["type"])
	}
}

func TestBuildMessagesRequestKeepsUndecodableNativeToolVerbatim(t *testing.T) {
	tool := json.RawMessage(`{"type":"web_search_20260209","name":"web_search","max_uses":"three"}`)
	chatReq := &provider.ChatRequest{
		Model:         "model",
		Messages:      []*schema.Message{schema.UserMessage("hello")},
		ProtocolState: NewAnthropicRequestProtocolState([]json.RawMessage{json.RawMessage(`{"name":123}`), tool}, nil),
	}
	state, err := provider.ResolveChatRequest(context.Background(), provider.ProviderConfig{}, chatReq)
	if err != nil {
		t.Fatalf("ResolveChatRequest: %v", err)
	}
	req := BuildMessagesRequest(state, BuildMessagesOptions{})
	if len(req.Tools) != 2 {
		t.Fatalf("tools = %d, want both forwarded", len(req.Tools))
	}
	wire, err := json.Marshal(req.Tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	if !bytes.Contains(wire, []byte(`{"name":123}`)) {
		t.Fatalf("undecodable tool dropped: %s", wire)
	}
	if bytes.Contains(wire, []byte(`"input_schema":null`)) {
		t.Fatalf("null input_schema injected into native tool: %s", wire)
	}
}
