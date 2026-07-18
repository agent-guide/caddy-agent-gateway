package qwen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/llm/credentialmgr"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

func TestGenerateUsesOpenAICompatibleAPI(t *testing.T) {
	resp, captured := generateAndCapture(t, nil)
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
	if got := captured["model"]; got != "qwen-plus" {
		t.Fatalf("model = %#v", got)
	}
	if got := captured["max_tokens"]; got != float64(128) {
		t.Fatalf("max_tokens = %#v", got)
	}
	if got := captured["temperature"]; got != 0.2 {
		t.Fatalf("temperature = %#v", got)
	}
	if _, ok := captured["enable_thinking"]; ok {
		t.Fatalf("enable_thinking should be omitted by default: %#v", captured["enable_thinking"])
	}
}

func TestGenerateUsesConfiguredEnableThinking(t *testing.T) {
	_, captured := generateAndCapture(t, map[string]any{"enable_thinking": false})
	if got := captured["enable_thinking"]; got != false {
		t.Fatalf("enable_thinking = %#v, want false", got)
	}
	kwargs, ok := captured["chat_template_kwargs"].(map[string]any)
	if !ok || kwargs["enable_thinking"] != false {
		t.Fatalf("chat_template_kwargs = %#v, want enable_thinking false", captured["chat_template_kwargs"])
	}
}

func TestRequestReasoningOverridesConfiguredThinking(t *testing.T) {
	req := &provider.ChatRequest{
		Model:    "qwen-plus",
		Messages: []*schema.Message{schema.UserMessage("2 + 2 等于几？")},
		Options: []einomodel.Option{
			provider.WithChatExtraFields(&provider.ChatExtraFields{
				Reasoning: map[string]any{"type": "enabled"},
			}),
		},
	}
	_, captured := generateAndCaptureRequest(t, map[string]any{"enable_thinking": false}, req)
	if got := captured["enable_thinking"]; got != true {
		t.Fatalf("enable_thinking = %#v, want true from request reasoning", got)
	}
}

func TestReasoningEffortEnablesThinking(t *testing.T) {
	req, err := provider.ResponsesToChatRequest(&provider.ResponsesRequest{
		Model:     "qwen-plus",
		Input:     "2 + 2 等于几？",
		Reasoning: map[string]any{"effort": "high"},
	})
	if err != nil {
		t.Fatalf("ResponsesToChatRequest returned error: %v", err)
	}

	_, captured := generateAndCaptureRequest(t, nil, req)
	if got := captured["enable_thinking"]; got != true {
		t.Fatalf("enable_thinking = %#v, want true from reasoning effort", got)
	}
	if captured["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", captured["reasoning_effort"])
	}
}

func TestCompactCCDropsUnsupportedMetadataAndUser(t *testing.T) {
	req, err := provider.ResponsesToChatRequest(&provider.ResponsesRequest{
		Model:    "qwen-plus",
		Input:    "2 + 2 等于几？",
		Metadata: map[string]any{"user_id": "abc123"},
		User:     "user-1",
	})
	if err != nil {
		t.Fatalf("ResponsesToChatRequest returned error: %v", err)
	}

	_, captured := generateAndCaptureRequest(t, map[string]any{"compact": "cc"}, req)
	if _, ok := captured["metadata"]; ok {
		t.Fatalf("metadata should be dropped in compact=cc mode: %#v", captured["metadata"])
	}
	if _, ok := captured["user"]; ok {
		t.Fatalf("user should be dropped in compact=cc mode: %#v", captured["user"])
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
	if p.BaseURL != "https://dashscope.aliyuncs.com/compatible-mode/v1" {
		t.Fatalf("BaseURL = %q", p.BaseURL)
	}
}

func generateAndCapture(t *testing.T, options map[string]any) (*provider.ChatResponse, map[string]any) {
	t.Helper()
	return generateAndCaptureRequest(t, options, &provider.ChatRequest{
		Model: "qwen-plus",
		Messages: []*schema.Message{
			{Role: schema.System, Content: "用中文回答"},
			{Role: schema.User, Content: "2 + 2 等于几？"},
		},
		Options: []einomodel.Option{
			einomodel.WithMaxTokens(128),
			einomodel.WithTemperature(0.2),
		},
	})
}

func generateAndCaptureRequest(t *testing.T, options map[string]any, req *provider.ChatRequest) (*provider.ChatResponse, map[string]any) {
	t.Helper()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/compatible-mode/v1/chat/completions" {
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
			"model": "qwen-plus",
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
		ProviderType: "qwen",
		APIKey:       "test-key",
		BaseURL:      server.URL + "/compatible-mode/v1",
		Options:      options,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	p := prov.(*Provider)

	ctx := provider.WithCredential(context.Background(), &credentialmgr.Credential{
		Attributes: map[string]string{"api_key": "test-key"},
	})
	resp, err := p.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	return resp, captured
}
