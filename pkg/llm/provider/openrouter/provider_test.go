package openrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/credential"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

func TestChatCarriesResponsesContextToOpenRouterPayload(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":1710000000,
			"model":"openrouter/test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}
		}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		ProviderType: "openrouter",
		APIKey:       "test-key",
		BaseURL:      server.URL,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	p := prov.(*Provider)

	chatReq, err := provider.ResponsesToChatRequest(&provider.ResponsesRequest{
		Model: "openrouter/test",
		Input: "hello",
		Text: map[string]any{
			"format": map[string]any{"type": "json_schema"},
		},
		Metadata: map[string]any{"trace_id": "abc123"},
		User:     "user-1",
		Reasoning: map[string]any{
			"effort":  "medium",
			"exclude": true,
		},
		ParallelToolCalls: boolPtr(true),
		Store:             boolPtr(true),
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

	if captured["user"] != "user-1" {
		t.Fatalf("user = %#v, want user-1", captured["user"])
	}
	if captured["parallel_tool_calls"] != true || captured["store"] != true {
		t.Fatalf("captured = %+v, want parallel_tool_calls/store", captured)
	}
	metadata, _ := captured["metadata"].(map[string]any)
	if metadata["trace_id"] != "abc123" {
		t.Fatalf("metadata = %+v, want trace_id", metadata)
	}
	reasoning, _ := captured["reasoning"].(map[string]any)
	if reasoning["effort"] != "medium" || reasoning["exclude"] != true {
		t.Fatalf("reasoning = %+v, want preserved reasoning", reasoning)
	}
	responseFormat, _ := captured["response_format"].(map[string]any)
	if responseFormat["type"] != "json_schema" {
		t.Fatalf("response_format = %+v, want json_schema", responseFormat)
	}
}

func boolPtr(v bool) *bool {
	return &v
}
