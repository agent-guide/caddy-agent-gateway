package zhipu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/credential"
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
	if got := messages[1].(map[string]any)["content"]; got != "2 + 2 等于几？" {
		t.Fatalf("content = %#v, want string content", got)
	}
	if got := captured["model"]; got != "glm-4.7" {
		t.Fatalf("model = %#v", got)
	}
	if got := captured["max_tokens"]; got != float64(128) {
		t.Fatalf("max_tokens = %#v", got)
	}
	if got := captured["temperature"]; got != 0.2 {
		t.Fatalf("temperature = %#v", got)
	}
	thinking, ok := captured["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking = %#v, want object", captured["thinking"])
	}
	if got := thinking["type"]; got != "disabled" {
		t.Fatalf("thinking.type = %#v, want disabled", got)
	}
}

func TestGenerateUsesConfiguredThinkingType(t *testing.T) {
	_, captured := generateAndCapture(t, map[string]any{"thinking_type": "enabled"})
	thinking, ok := captured["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking = %#v, want object", captured["thinking"])
	}
	if got := thinking["type"]; got != "enabled" {
		t.Fatalf("thinking.type = %#v, want enabled", got)
	}
}

func TestGenerateOmitsThinkingWhenConfiguredNone(t *testing.T) {
	_, captured := generateAndCapture(t, map[string]any{"thinking_type": "none"})
	if _, ok := captured["thinking"]; ok {
		t.Fatalf("thinking should be omitted: %#v", captured["thinking"])
	}
}

func TestGenerateCarriesResponsesContextToOpenAICompatiblePayload(t *testing.T) {
	req, err := provider.ResponsesToChatRequest(&provider.ResponsesRequest{
		Model: "glm-4.7",
		Input: "2 + 2 等于几？",
		Text: map[string]any{
			"format": map[string]any{"type": "json_object"},
		},
		Metadata:          map[string]any{"trace_id": "abc123"},
		User:              "user-1",
		Reasoning:         map[string]any{"effort": "high"},
		ParallelToolCalls: boolPtr(true),
		Store:             boolPtr(false),
	})
	if err != nil {
		t.Fatalf("ResponsesToChatRequest returned error: %v", err)
	}

	_, captured := generateAndCaptureRequest(t, nil, req)
	if captured["user"] != "user-1" {
		t.Fatalf("user = %#v, want user-1", captured["user"])
	}
	if captured["parallel_tool_calls"] != true || captured["store"] != false {
		t.Fatalf("captured = %+v, want parallel_tool_calls/store", captured)
	}
	if captured["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", captured["reasoning_effort"])
	}
	metadata, _ := captured["metadata"].(map[string]any)
	if metadata["trace_id"] != "abc123" {
		t.Fatalf("metadata = %+v, want trace_id", metadata)
	}
	responseFormat, _ := captured["response_format"].(map[string]any)
	if responseFormat["type"] != "json_object" {
		t.Fatalf("response_format = %+v, want json_object", responseFormat)
	}
	thinking, ok := captured["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("thinking = %#v, want disabled", captured["thinking"])
	}
}

func TestCompactCCDropsUnsupportedMetadataAndUser(t *testing.T) {
	req, err := provider.ResponsesToChatRequest(&provider.ResponsesRequest{
		Model:     "glm-4.7",
		Input:     "2 + 2 等于几？",
		Metadata:  map[string]any{"user_id": "abc123"},
		User:      "user-1",
		Reasoning: map[string]any{"effort": "high"},
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
	// Unrelated fields must still pass through.
	if captured["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", captured["reasoning_effort"])
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
	if p.BaseURL != "https://open.bigmodel.cn/api/paas/v4" {
		t.Fatalf("BaseURL = %q", p.BaseURL)
	}
}

func generateAndCapture(t *testing.T, options map[string]any) (*provider.ChatResponse, map[string]any) {
	t.Helper()
	return generateAndCaptureRequest(t, options, &provider.ChatRequest{
		Model: "glm-4.7",
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
		if r.URL.Path != "/api/paas/v4/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("unexpected authorization: %q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "" {
			t.Fatalf("x-api-key should not be sent: %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "" {
			t.Fatalf("anthropic-version should not be sent: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"object": "chat.completion",
			"created": 1710000000,
			"model": "glm-4.7",
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
		ProviderType: "zhipu",
		APIKey:       "test-key",
		BaseURL:      server.URL + "/api/paas/v4",
		Options:      options,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	p := prov.(*Provider)

	ctx := provider.WithCredential(context.Background(), &credential.Credential{
		Attributes: map[string]string{"api_key": "test-key"},
	})
	resp, err := p.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	return resp, captured
}

func boolPtr(v bool) *bool {
	return &v
}
