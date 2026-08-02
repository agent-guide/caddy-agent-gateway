package deepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	einodeepseek "github.com/cloudwego/eino-ext/components/model/deepseek"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/credential"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

func TestGenerateUsesDeepSeekAPI(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("unexpected authorization: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"object": "chat.completion",
			"created": 1710000000,
			"model": "deepseek-chat",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "四"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 4, "completion_tokens": 1, "total_tokens": 5}
		}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		ProviderType: "deepseek",
		APIKey:       "test-key",
		BaseURL:      server.URL,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	p := prov.(*Provider)

	ctx := provider.WithCredential(context.Background(), &credential.Credential{
		Attributes: map[string]string{"api_key": "test-key"},
	})
	resp, err := p.Chat(ctx, &provider.ChatRequest{
		Model: "deepseek-chat",
		Messages: []*schema.Message{
			{Role: schema.System, Content: "用中文回答"},
			{Role: schema.User, Content: "2 + 2 等于几？"},
		},
		Options: []einomodel.Option{
			einomodel.WithMaxTokens(128),
			einomodel.WithTemperature(0.2),
		},
	})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if resp == nil || resp.Message == nil || resp.Message.Content != "四" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	messages, ok := captured["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("unexpected messages: %#v", captured["messages"])
	}
	if got := messages[0].(map[string]any)["role"]; got != "system" {
		t.Fatalf("first role = %#v, want system", got)
	}
	if got := messages[1].(map[string]any)["content"]; got != "2 + 2 等于几？" {
		t.Fatalf("content = %#v, want string content", got)
	}
	if got := captured["model"]; got != "deepseek-chat" {
		t.Fatalf("model = %#v", got)
	}
	if got := captured["max_tokens"]; got != float64(128) {
		t.Fatalf("max_tokens = %#v", got)
	}
	if got := captured["temperature"]; got != 0.2 {
		t.Fatalf("temperature = %#v", got)
	}
}

func TestNewDefaults(t *testing.T) {
	prov, err := New(provider.ProviderConfig{})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	p, ok := prov.(*Provider)
	if !ok {
		t.Fatalf("unexpected provider type %T", prov)
	}
	if p.ProviderType != "" {
		t.Fatalf("provider type should not be changed by New: %q", p.ProviderType)
	}
	if p.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("BaseURL = %q", p.BaseURL)
	}
}

func TestGenerateCarriesResponsesContextToDeepSeekPayload(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"object": "chat.completion",
			"created": 1710000000,
			"model": "deepseek-chat",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "四"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 4, "completion_tokens": 1, "total_tokens": 5}
		}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		ProviderType: "deepseek",
		APIKey:       "test-key",
		BaseURL:      server.URL,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	p := prov.(*Provider)

	chatReq, err := provider.ResponsesToChatRequest(&provider.ResponsesRequest{
		Model: "deepseek-chat",
		Input: "2 + 2 等于几？",
		Text: map[string]any{
			"format": map[string]any{"type": "json_schema"},
		},
		User: "user-1",
		Metadata: map[string]any{
			"trace_id": "abc123",
		},
		Reasoning: map[string]any{
			"effort": "high",
		},
		ParallelToolCalls: boolPtr(true),
		Store:             boolPtr(false),
	})
	if err != nil {
		t.Fatalf("ResponsesToChatRequest returned error: %v", err)
	}

	ctx := provider.WithCredential(context.Background(), &credential.Credential{
		Attributes: map[string]string{"api_key": "test-key"},
	})
	if _, err := p.Chat(ctx, chatReq); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}

	responseFormat, ok := captured["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format = %#v, want object", captured["response_format"])
	}
	if responseFormat["type"] != "json_schema" {
		t.Fatalf("response_format.type = %#v, want json_schema", responseFormat["type"])
	}
	if captured["user"] != "user-1" {
		t.Fatalf("user = %#v, want user-1", captured["user"])
	}
	metadata, ok := captured["metadata"].(map[string]any)
	if !ok || metadata["trace_id"] != "abc123" {
		t.Fatalf("metadata = %#v, want trace_id", captured["metadata"])
	}
	if captured["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", captured["reasoning_effort"])
	}
	if captured["parallel_tool_calls"] != true || captured["store"] != false {
		t.Fatalf("captured = %+v, want parallel_tool_calls/store preserved", captured)
	}
}

func TestGenerateMapsDeveloperMessagesToSystem(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test","object":"chat.completion","created":1710000000,"model":"deepseek-chat",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		ProviderType: "deepseek",
		APIKey:       "test-key",
		BaseURL:      server.URL,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	p := prov.(*Provider)

	req := &provider.ChatRequest{
		Model: "deepseek-chat",
		Messages: []*schema.Message{
			{Role: schema.RoleType("developer"), Content: "follow policy"},
			{Role: schema.User, Content: "hi"},
		},
	}
	ctx := provider.WithCredential(context.Background(), &credential.Credential{
		Attributes: map[string]string{"api_key": "test-key"},
	})
	if _, err := p.Chat(ctx, req); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if req.Messages[0].Role != schema.RoleType("developer") {
		t.Fatalf("request message was mutated: %+v", req.Messages[0])
	}

	messages, ok := captured["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v, want 2 messages", captured["messages"])
	}
	if got := messages[0].(map[string]any)["role"]; got != "system" {
		t.Fatalf("first role = %#v, want system", got)
	}
}

func TestApplyOptions(t *testing.T) {
	cfg := &einodeepseek.ChatModelConfig{}
	applyOptions(cfg, map[string]any{
		"path":                 "/custom/chat",
		"response_format_type": "json_object",
		"max_tokens":           "256",
		"temperature":          "0.3",
		"top_p":                float64(0.9),
		"presence_penalty":     "0.1",
		"frequency_penalty":    "-0.2",
		"log_probs":            "true",
		"top_log_probs":        float64(2),
	})

	if cfg.Path != "/custom/chat" {
		t.Fatalf("Path = %q, want /custom/chat", cfg.Path)
	}
	if cfg.ResponseFormatType != einodeepseek.ResponseFormatTypeJSONObject {
		t.Fatalf("ResponseFormatType = %#v, want json_object", cfg.ResponseFormatType)
	}
	if cfg.MaxTokens != 256 || cfg.Temperature != 0.3 || cfg.TopP != 0.9 {
		t.Fatalf("common options not applied: %+v", cfg)
	}
	if cfg.PresencePenalty != 0.1 || cfg.FrequencyPenalty != -0.2 {
		t.Fatalf("penalties not applied: %+v", cfg)
	}
	if !cfg.LogProbs || cfg.TopLogProbs != 2 {
		t.Fatalf("logprobs not applied: %+v", cfg)
	}
}

func chatCaptureBody(t *testing.T, options map[string]any) map[string]any {
	t.Helper()
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test","object":"chat.completion","created":1710000000,"model":"deepseek-chat",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		ProviderType: "deepseek",
		APIKey:       "test-key",
		BaseURL:      server.URL,
		Options:      options,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	p := prov.(*Provider)
	ctx := provider.WithCredential(context.Background(), &credential.Credential{
		Attributes: map[string]string{"api_key": "test-key"},
	})
	if _, err := p.Chat(ctx, &provider.ChatRequest{
		Model:    "deepseek-chat",
		Messages: []*schema.Message{{Role: schema.User, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	return captured
}

// DeepSeek v4 models run in thinking mode by default and then require the
// reasoning_content of tool-calling assistant turns to be replayed, which the
// cc/anthropic protocol cannot do. The provider must default thinking off.
func TestThinkingDisabledByDefault(t *testing.T) {
	captured := chatCaptureBody(t, nil)
	thinking, ok := captured["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking = %#v, want object", captured["thinking"])
	}
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking.type = %#v, want disabled", thinking["type"])
	}
}

func TestThinkingTypeOptionOverride(t *testing.T) {
	captured := chatCaptureBody(t, map[string]any{"thinking_type": "enabled"})
	thinking, ok := captured["thinking"].(map[string]any)
	if !ok || thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v, want type=enabled", captured["thinking"])
	}
}

func TestThinkingTypeNoneOmitsField(t *testing.T) {
	captured := chatCaptureBody(t, map[string]any{"thinking_type": "none"})
	if _, ok := captured["thinking"]; ok {
		t.Fatalf("thinking should be omitted when thinking_type=none: %#v", captured["thinking"])
	}
}

func boolPtr(v bool) *bool {
	return &v
}
