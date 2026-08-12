package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/httpclient"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
)

func TestChatMapsExpandedFieldsToMessagesRequest(t *testing.T) {
	var path string
	var apiKeyHeader string
	var versionHeader string
	var reqBody anthropicbase.MessagesRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.RequestURI()
		apiKeyHeader = r.Header.Get("x-api-key")
		versionHeader = r.Header.Get("anthropic-version")
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-ant-test",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	parallel := false
	store := true
	resp, err := prov.Chat(context.Background(), &provider.ChatRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []*schema.Message{
			schema.SystemMessage("system prompt"),
			schema.UserMessage("hello"),
		},
		Options: []model.Option{
			model.WithMaxTokens(20000),
			model.WithTemperature(0.25),
			model.WithTopP(0.75),
			model.WithStop([]string{"stop"}),
			model.WithTools([]*schema.ToolInfo{{Name: "lookup", Desc: "Lookup data"}}),
			model.WithToolChoice(schema.ToolChoiceAllowed),
			provider.WithTopK(17),
			provider.WithChatExtraFields(&provider.ChatExtraFields{
				ReasoningEffort:   "medium",
				User:              "chat-user",
				Metadata:          map[string]any{"user_id": "metadata-user"},
				ParallelToolCalls: &parallel,
				Store:             &store,
			}),
			provider.WithResponsesRequestContext(&provider.ResponsesRequestContext{
				User:              "responses-user",
				Reasoning:         map[string]any{"effort": "high"},
				ParallelToolCalls: &store,
			}),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp == nil || resp.Message == nil || resp.Message.Content != "hello" {
		t.Fatalf("response = %+v, want hello", resp)
	}

	if path != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", path)
	}
	if apiKeyHeader != "sk-ant-test" {
		t.Fatalf("x-api-key = %q, want sk-ant-test", apiKeyHeader)
	}
	if versionHeader != anthropicVersion {
		t.Fatalf("anthropic-version = %q, want %q", versionHeader, anthropicVersion)
	}
	if reqBody.Model != "claude-sonnet-4-20250514" || reqBody.MaxTokens != 20000 {
		t.Fatalf("model/max_tokens = %q/%d, want claude-sonnet-4-20250514/20000", reqBody.Model, reqBody.MaxTokens)
	}
	// Extended thinking is enabled (reasoning_effort medium), so temperature,
	// top_p, and top_k must be dropped to satisfy Anthropic's constraints.
	if reqBody.Temperature != 0 || reqBody.TopP != 0 || reqBody.TopK != 0 {
		t.Fatalf("sampling = %v/%v/%d, want dropped under extended thinking", reqBody.Temperature, reqBody.TopP, reqBody.TopK)
	}
	if len(reqBody.StopSequences) != 1 || reqBody.StopSequences[0] != "stop" {
		t.Fatalf("stop_sequences = %+v, want stop", reqBody.StopSequences)
	}
	if len(reqBody.Tools) != 1 || reqBody.Tools[0].Name != "lookup" {
		t.Fatalf("tools = %+v, want lookup", reqBody.Tools)
	}
	if reqBody.Metadata == nil || reqBody.Metadata.UserID != "chat-user" {
		t.Fatalf("metadata = %+v, want user_id=chat-user", reqBody.Metadata)
	}
	if reqBody.Thinking == nil || reqBody.Thinking.Type != "enabled" || reqBody.Thinking.BudgetTokens != 4096 {
		t.Fatalf("thinking = %+v, want enabled budget 4096", reqBody.Thinking)
	}
	var toolChoice map[string]any
	if err := json.Unmarshal(reqBody.ToolChoice, &toolChoice); err != nil {
		t.Fatalf("decode tool_choice: %v", err)
	}
	if toolChoice["type"] != "auto" || toolChoice["disable_parallel_tool_use"] != true {
		t.Fatalf("tool_choice = %+v, want auto with disable_parallel_tool_use", toolChoice)
	}
	if len(reqBody.System) != 1 || reqBody.System[0].Text != "system prompt" {
		t.Fatalf("system = %+v, want user system prompt", reqBody.System)
	}
	if len(reqBody.Messages) != 1 || reqBody.Messages[0].Content[0].CacheControl != nil {
		t.Fatalf("messages = %+v, want uncached user text", reqBody.Messages)
	}
}

func TestChatReplaysGatewayReasoningWithoutUsingEinoMultimodalParts(t *testing.T) {
	var reqBody anthropicbase.MessagesRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"thinking","thinking":"first step","signature":"response-signature-1"},{"type":"redacted_thinking","data":"response-redacted"},{"type":"thinking","thinking":"next step","signature":"response-signature-2"},{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov := newCaptureProvider(t, server.URL)
	assistant := provider.AttachReasoningParts(&schema.Message{
		Role: schema.Assistant,
		AssistantGenMultiContent: []schema.MessageOutputPart{{
			Type: schema.ChatMessagePartTypeText,
			Text: "visible assistant text",
		}},
		Extra: map[string]any{
			einoClaudeBreakpointKey:    true,
			einoClaudeBreakpointTTLKey: "1h",
		},
		ToolCalls: []schema.ToolCall{{
			ID: "tool-1", Type: "function",
			Function: schema.FunctionCall{Name: "lookup", Arguments: `{"q":"x"}`},
		}},
	},
		provider.NewReasoningOutputPart("inspect", "request-signature", nil),
		provider.NewEncryptedReasoningOutputPart("opaque-redacted", nil),
	)
	resp, err := prov.Chat(context.Background(), &provider.ChatRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []*schema.Message{
			schema.UserMessage("inspect"),
			assistant,
			{Role: schema.Tool, ToolCallID: "tool-1", Content: "result"},
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if len(reqBody.Messages) != 3 || len(reqBody.Messages[1].Content) != 4 {
		t.Fatalf("messages = %+v, want signed thinking, redacted thinking, multimodal text, and tool use", reqBody.Messages)
	}
	if got := reqBody.Messages[1].Content[0]; got.Type != "thinking" || got.Thinking != "inspect" || got.Signature != "request-signature" {
		t.Fatalf("replayed thinking = %+v", got)
	}
	if got := reqBody.Messages[1].Content[1]; got.Type != "redacted_thinking" || got.Data != "opaque-redacted" {
		t.Fatalf("replayed redacted thinking = %+v", got)
	}
	if got := reqBody.Messages[1].Content[2]; got.Type != "text" || got.Text != "visible assistant text" {
		t.Fatalf("replayed assistant multimodal text = %+v", got)
	}
	if got := reqBody.Messages[1].Content[3]; got.Type != "tool_use" || got.CacheControl == nil || got.CacheControl.Type != "ephemeral" || got.CacheControl.TTL != "1h" {
		t.Fatalf("replayed tool/cache breakpoint = %+v", got)
	}
	parts := provider.ReasoningPartsFromMessage(resp.Message)
	if len(parts) != 3 {
		t.Fatalf("response reasoning = %+v, want all three wire blocks", parts)
	}
	if parts[0].Reasoning == nil || parts[0].Reasoning.Text != "first step" || parts[0].Reasoning.Signature != "response-signature-1" {
		t.Fatalf("first response reasoning = %+v", parts[0])
	}
	if provider.EncryptedReasoningData(parts[1]) != "response-redacted" {
		t.Fatalf("redacted response reasoning = %+v", parts[1])
	}
	if parts[2].Reasoning == nil || parts[2].Reasoning.Text != "next step" || parts[2].Reasoning.Signature != "response-signature-2" {
		t.Fatalf("second response reasoning = %+v", parts[2])
	}
	if resp.Message.ReasoningContent != "first step\nnext step" {
		t.Fatalf("ReasoningContent = %q, want both visible thinking blocks", resp.Message.ReasoningContent)
	}
}

func TestChatReplaysOmittedThinkingWithAuthenticSignature(t *testing.T) {
	var reqBody anthropicbase.MessagesRequest
	server := newCaptureServer(t, &reqBody)
	defer server.Close()

	prov := newCaptureProvider(t, server.URL)
	assistant := provider.AttachReasoningParts(&schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID: "tool-1", Type: "function",
			Function: schema.FunctionCall{Name: "lookup", Arguments: `{}`},
		}},
	}, provider.NewReasoningOutputPart("", "omitted-signature", nil))

	_, err := prov.Chat(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-5",
		Messages: []*schema.Message{
			schema.UserMessage("inspect"),
			assistant,
			{Role: schema.Tool, ToolCallID: "tool-1", Content: "result"},
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if len(reqBody.Messages) != 3 || len(reqBody.Messages[1].Content) != 2 {
		t.Fatalf("messages = %+v, want omitted thinking and tool use", reqBody.Messages)
	}
	if got := reqBody.Messages[1].Content[0]; got.Type != "thinking" || got.Thinking != "" || got.Signature != "omitted-signature" {
		t.Fatalf("omitted thinking replay = %+v", got)
	}
}

func TestChatUsesNativeEinoPathForSingleSignedThinking(t *testing.T) {
	var reqBody anthropicbase.MessagesRequest
	server := newCaptureServer(t, &reqBody)
	defer server.Close()

	prov := newCaptureProvider(t, server.URL)
	assistant := provider.AttachReasoningParts(&schema.Message{
		Role: schema.Assistant,
		AssistantGenMultiContent: []schema.MessageOutputPart{{
			Type: schema.ChatMessagePartTypeText,
			Text: "visible assistant text",
		}},
		Extra: map[string]any{"_eino_claude_breakpoint": true},
	}, provider.NewReasoningOutputPart("inspect", "request-signature", nil))

	_, err := prov.Chat(context.Background(), &provider.ChatRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []*schema.Message{
			schema.UserMessage("inspect"),
			assistant,
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if len(reqBody.Messages) != 2 || len(reqBody.Messages[1].Content) != 2 {
		t.Fatalf("messages = %+v, want native thinking and multimodal text blocks", reqBody.Messages)
	}
	if got := reqBody.Messages[1].Content[0]; got.Type != "thinking" || got.Thinking != "inspect" || got.Signature != "request-signature" {
		t.Fatalf("native thinking = %+v", got)
	}
	if got := reqBody.Messages[1].Content[1]; got.Type != "text" || got.Text != "visible assistant text" || got.CacheControl == nil || got.CacheControl.Type != "ephemeral" {
		t.Fatalf("native assistant content = %+v, want text and cache breakpoint preserved", got)
	}
}

func TestEinoReasoningStreamStatePreservesBlocksAndSignatureFragments(t *testing.T) {
	state := &einoReasoningStreamState{}
	chunks := []*schema.Message{
		{Role: schema.Assistant, Extra: map[string]any{einoClaudeThinkingKey: "", einoClaudeThinkingSignatureKey: ""}},
		{Role: schema.Assistant, Extra: map[string]any{einoClaudeThinkingKey: "inspect "}},
		{Role: schema.Assistant, Extra: map[string]any{einoClaudeThinkingKey: "repo"}},
		{Role: schema.Assistant, Extra: map[string]any{einoClaudeThinkingSignatureKey: "sig-"}},
		{Role: schema.Assistant, Extra: map[string]any{einoClaudeThinkingSignatureKey: "one"}},
		{Role: schema.Assistant, Extra: map[string]any{einoClaudeThinkingKey: "", einoClaudeThinkingSignatureKey: ""}},
		{Role: schema.Assistant, Extra: map[string]any{einoClaudeThinkingSignatureKey: "sig-two"}},
	}
	for _, chunk := range chunks {
		state.attach(chunk)
	}

	merged, err := schema.ConcatMessages(chunks)
	if err != nil {
		t.Fatalf("ConcatMessages() error = %v", err)
	}
	parts := provider.ReasoningPartsFromMessage(merged)
	if len(parts) != 2 {
		t.Fatalf("reasoning parts = %+v, want two streamed blocks", parts)
	}
	if got := parts[0]; got.StreamingMeta == nil || got.StreamingMeta.Index != 0 || got.Reasoning == nil || got.Reasoning.Text != "inspect repo" || got.Reasoning.Signature != "sig-one" {
		t.Fatalf("first reasoning block = %+v", got)
	}
	if got := parts[1]; got.StreamingMeta == nil || got.StreamingMeta.Index != 1 || got.Reasoning == nil || got.Reasoning.Text != "" || got.Reasoning.Signature != "sig-two" {
		t.Fatalf("second reasoning block = %+v", got)
	}
}

func TestStreamChatPinsEinoThinkingBlockBoundaryContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-20250514\",\"usage\":{\"input_tokens\":4,\"output_tokens\":1}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"inspect\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"one\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, event := range events {
			_, _ = w.Write([]byte(event))
		}
	}))
	defer server.Close()

	prov := newCaptureProvider(t, server.URL)
	stream, err := prov.StreamChat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("inspect")},
	})
	if err != nil {
		t.Fatalf("StreamChat() error = %v", err)
	}
	defer stream.Close()

	var chunks []*schema.Message
	boundaryFound := false
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		chunks = append(chunks, chunk)
		for _, part := range provider.ReasoningPartsFromMessage(chunk) {
			if part.StreamingMeta != nil && part.StreamingMeta.Index == 0 && part.Reasoning != nil && part.Reasoning.Text == "" && part.Reasoning.Signature == "" {
				boundaryFound = true
			}
		}
	}
	if !boundaryFound {
		t.Fatal("real eino stream parser did not expose the empty thinking/signature start markers")
	}
	merged, err := schema.ConcatMessages(chunks)
	if err != nil {
		t.Fatalf("ConcatMessages() error = %v", err)
	}
	parts := provider.ReasoningPartsFromMessage(merged)
	if len(parts) != 1 {
		t.Fatalf("reasoning parts = %+v, want one block from real eino stream parser", parts)
	}
	if got := parts[0]; got.StreamingMeta == nil || got.StreamingMeta.Index != 0 || got.Reasoning == nil || got.Reasoning.Text != "inspect" || got.Reasoning.Signature != "sig-one" {
		t.Fatalf("first reasoning block = index=%v reasoning=%+v", got.StreamingMeta, got.Reasoning)
	}
}

func TestExactThinkingReplayRejectsUnsupportedAssistantContent(t *testing.T) {
	msg := provider.AttachReasoningParts(&schema.Message{
		Role: schema.Assistant,
		AssistantGenMultiContent: []schema.MessageOutputPart{{
			Type:  schema.ChatMessagePartTypeAudioURL,
			Audio: &schema.MessageOutputAudio{},
		}},
	}, provider.NewEncryptedReasoningOutputPart("opaque-redacted", nil))

	_, _, err := adaptSignedReasoningMessages([]*schema.Message{msg})
	if err == nil || !strings.Contains(err.Error(), "cannot preserve assistant part type") {
		t.Fatalf("adaptSignedReasoningMessages() error = %v, want explicit loss-prevention error", err)
	}
}

func TestCreateResponsesMapsResponsesContextToMessagesRequest(t *testing.T) {
	var reqBody anthropicbase.MessagesRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-ant-test",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	responsesProv, ok := prov.(provider.ResponsesProvider)
	if !ok {
		t.Fatal("anthropic provider does not implement ResponsesProvider")
	}

	parallel := false
	resp, err := responsesProv.CreateResponses(context.Background(), &provider.ResponsesRequest{
		Model: "claude-sonnet-4-20250514",
		Input: "hello",
		Tools: []provider.ResponsesToolDefinition{{
			Type:        "function",
			Name:        "lookup",
			Description: "Lookup data",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
		ToolChoice:        json.RawMessage(`{"type":"function","name":"lookup"}`),
		MaxOutputTokens:   4096,
		Temperature:       0.2,
		TopP:              0.8,
		User:              "responses-user",
		Metadata:          map[string]any{"user_id": "metadata-user"},
		Reasoning:         map[string]any{"budget_tokens": float64(2048)},
		ParallelToolCalls: &parallel,
	})
	if err != nil {
		t.Fatalf("CreateResponses() error = %v", err)
	}
	if resp == nil || len(resp.Output) == 0 {
		t.Fatalf("responses response = %+v, want output", resp)
	}

	if reqBody.Model != "claude-sonnet-4-20250514" || reqBody.MaxTokens != 4096 {
		t.Fatalf("model/max_tokens = %q/%d, want claude-sonnet-4-20250514/4096", reqBody.Model, reqBody.MaxTokens)
	}
	if reqBody.Metadata == nil || reqBody.Metadata.UserID != "responses-user" {
		t.Fatalf("metadata = %+v, want user_id=responses-user", reqBody.Metadata)
	}
	if reqBody.Thinking == nil || reqBody.Thinking.Type != "enabled" || reqBody.Thinking.BudgetTokens != 2048 {
		t.Fatalf("thinking = %+v, want enabled budget 2048", reqBody.Thinking)
	}
	var toolChoice map[string]any
	if err := json.Unmarshal(reqBody.ToolChoice, &toolChoice); err != nil {
		t.Fatalf("decode tool_choice: %v", err)
	}
	if toolChoice["type"] != "tool" || toolChoice["name"] != "lookup" || toolChoice["disable_parallel_tool_use"] != true {
		t.Fatalf("tool_choice = %+v, want named tool with disable_parallel_tool_use", toolChoice)
	}
}

// newCaptureServer returns a mock Anthropic endpoint that decodes the request
// body into capture and replies with a minimal successful message.
func newCaptureServer(t *testing.T, capture *anthropicbase.MessagesRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(capture); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
}

func newCaptureProvider(t *testing.T, url string) provider.Provider {
	t.Helper()
	prov, err := New(provider.ProviderConfig{
		BaseURL: url,
		APIKey:  "sk-ant-test",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return prov
}

func TestChatKeepsSamplingWhenThinkingDisabled(t *testing.T) {
	var reqBody anthropicbase.MessagesRequest
	server := newCaptureServer(t, &reqBody)
	defer server.Close()
	prov := newCaptureProvider(t, server.URL)

	_, err := prov.Chat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("hello")},
		Options: []model.Option{
			model.WithMaxTokens(1000),
			model.WithTemperature(0.25),
			model.WithTopP(0.75),
			provider.WithTopK(17),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if reqBody.Thinking != nil {
		t.Fatalf("thinking = %+v, want nil without reasoning", reqBody.Thinking)
	}
	if reqBody.Temperature != 0.25 || reqBody.TopP != 0.75 || reqBody.TopK != 17 {
		t.Fatalf("sampling = %v/%v/%d, want 0.25/0.75/17 without thinking", reqBody.Temperature, reqBody.TopP, reqBody.TopK)
	}
	if reqBody.MaxTokens != 1000 {
		t.Fatalf("max_tokens = %d, want 1000", reqBody.MaxTokens)
	}
}

func TestChatClampsThinkingBudgetBelowMaxTokens(t *testing.T) {
	var reqBody anthropicbase.MessagesRequest
	server := newCaptureServer(t, &reqBody)
	defer server.Close()
	prov := newCaptureProvider(t, server.URL)

	_, err := prov.Chat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("hello")},
		Options: []model.Option{
			model.WithMaxTokens(3000),
			provider.WithChatExtraFields(&provider.ChatExtraFields{ReasoningEffort: "high"}),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if reqBody.Thinking == nil || reqBody.Thinking.Type != "enabled" {
		t.Fatalf("thinking = %+v, want enabled", reqBody.Thinking)
	}
	// effort=high requests 8192 thinking tokens, which exceeds max_tokens=3000.
	// The budget must shrink to stay below the cap with answer headroom.
	if reqBody.Thinking.BudgetTokens >= reqBody.MaxTokens {
		t.Fatalf("budget %d must be < max_tokens %d", reqBody.Thinking.BudgetTokens, reqBody.MaxTokens)
	}
	if reqBody.Thinking.BudgetTokens < 1024 {
		t.Fatalf("budget %d must be >= 1024", reqBody.Thinking.BudgetTokens)
	}
	if reqBody.MaxTokens != 3000 {
		t.Fatalf("max_tokens = %d, want unchanged 3000", reqBody.MaxTokens)
	}
}

func TestChatGrowsMaxTokensForMinimumThinking(t *testing.T) {
	var reqBody anthropicbase.MessagesRequest
	server := newCaptureServer(t, &reqBody)
	defer server.Close()
	prov := newCaptureProvider(t, server.URL)

	_, err := prov.Chat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("hello")},
		Options: []model.Option{
			model.WithMaxTokens(500),
			provider.WithChatExtraFields(&provider.ChatExtraFields{ReasoningEffort: "low"}),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	// max_tokens=500 is too small to host even minimal (1024) thinking, so it
	// must grow above the budget.
	if reqBody.Thinking == nil || reqBody.Thinking.BudgetTokens != 1024 {
		t.Fatalf("thinking = %+v, want budget 1024", reqBody.Thinking)
	}
	if reqBody.MaxTokens <= reqBody.Thinking.BudgetTokens {
		t.Fatalf("max_tokens %d must exceed budget %d", reqBody.MaxTokens, reqBody.Thinking.BudgetTokens)
	}
}

func TestChatDisablesParallelToolUseWithoutExplicitToolChoice(t *testing.T) {
	var reqBody anthropicbase.MessagesRequest
	server := newCaptureServer(t, &reqBody)
	defer server.Close()
	prov := newCaptureProvider(t, server.URL)

	parallel := false
	_, err := prov.Chat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("hello")},
		Options: []model.Option{
			model.WithTools([]*schema.ToolInfo{{Name: "lookup", Desc: "Lookup data"}}),
			provider.WithChatExtraFields(&provider.ChatExtraFields{ParallelToolCalls: &parallel}),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	var toolChoice map[string]any
	if err := json.Unmarshal(reqBody.ToolChoice, &toolChoice); err != nil {
		t.Fatalf("decode tool_choice: %v", err)
	}
	if toolChoice["type"] != "auto" || toolChoice["disable_parallel_tool_use"] != true {
		t.Fatalf("tool_choice = %+v, want auto with disable_parallel_tool_use", toolChoice)
	}
}

func TestChatMapsResponseFormatToOutputConfig(t *testing.T) {
	var reqBody anthropicbase.MessagesRequest
	server := newCaptureServer(t, &reqBody)
	defer server.Close()
	prov := newCaptureProvider(t, server.URL)

	_, err := prov.Chat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("hello")},
		Options: []model.Option{
			provider.WithChatExtraFields(&provider.ChatExtraFields{
				ResponseFormat: map[string]any{
					"type": "json_schema",
					"json_schema": map[string]any{
						"name":   "person",
						"schema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}},
					},
				},
			}),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if reqBody.OutputConfig == nil || reqBody.OutputConfig.Format == nil {
		t.Fatalf("output_config = %+v, want format", reqBody.OutputConfig)
	}
	if reqBody.OutputConfig.Format.Type != "json_schema" {
		t.Fatalf("format type = %q, want json_schema", reqBody.OutputConfig.Format.Type)
	}
	var schemaObj map[string]any
	if err := json.Unmarshal(reqBody.OutputConfig.Format.Schema, &schemaObj); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if schemaObj["type"] != "object" {
		t.Fatalf("schema = %+v, want object schema extracted from json_schema", schemaObj)
	}
}

func TestChatPassesThroughUpstream4xxWithoutRetry(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad input"}}`))
	}))
	defer server.Close()
	prov := newCaptureProvider(t, server.URL)

	_, err := prov.Chat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("hello")},
	})
	if err == nil {
		t.Fatal("Chat() error = nil, want upstream error")
	}
	if status := statuserr.StatusCode(err, 0); status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 passed through", status)
	}
	if !strings.Contains(err.Error(), "bad input") {
		t.Fatalf("error = %q, want upstream body included", err)
	}
	if requests != 1 {
		t.Fatalf("upstream requests = %d, want 1 (4xx must not be retried)", requests)
	}
}

func TestStreamChatCapturesInputTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\n"))
		_, _ = w.Write([]byte(`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-20250514","usage":{"input_tokens":42,"output_tokens":1}}}` + "\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\n"))
		_, _ = w.Write([]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\n"))
		_, _ = w.Write([]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n"))
		_, _ = w.Write([]byte("event: content_block_stop\n"))
		_, _ = w.Write([]byte(`data: {"type":"content_block_stop","index":0}` + "\n\n"))
		_, _ = w.Write([]byte("event: message_delta\n"))
		_, _ = w.Write([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n"))
		_, _ = w.Write([]byte("event: message_stop\n"))
		_, _ = w.Write([]byte(`data: {"type":"message_stop"}` + "\n\n"))
	}))
	defer server.Close()
	prov := newCaptureProvider(t, server.URL)

	stream, err := prov.StreamChat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("hello")},
	})
	if err != nil {
		t.Fatalf("StreamChat() error = %v", err)
	}
	defer stream.Close()

	var prompt, completion int
	var finalUsage *schema.TokenUsage
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
			continue
		}
		if msg.ResponseMeta.Usage.PromptTokens > 0 {
			prompt = msg.ResponseMeta.Usage.PromptTokens
		}
		if msg.ResponseMeta.Usage.CompletionTokens > 0 {
			completion = msg.ResponseMeta.Usage.CompletionTokens
			finalUsage = msg.ResponseMeta.Usage
		}
	}
	if finalUsage == nil {
		t.Fatal("final streamed usage = nil, want token usage on message_delta")
	}
	if prompt != 42 {
		t.Fatalf("prompt tokens = %d, want 42 from message_start", prompt)
	}
	if completion != 7 {
		t.Fatalf("completion tokens = %d, want 7 from message_delta", completion)
	}
}
