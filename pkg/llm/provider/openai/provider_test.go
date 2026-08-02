package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/credential"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

func chatAndCaptureRequest(t *testing.T, options map[string]any, req *provider.ChatRequest) map[string]any {
	t.Helper()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
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
			"model":"gpt-4.1",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}
		}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		ProviderType: "openai",
		APIKey:       "test-key",
		BaseURL:      server.URL + "/v1",
		Options:      options,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	p := prov.(*Provider)

	ctx := provider.WithCredential(context.Background(), &credential.Credential{
		Attributes: map[string]string{"api_key": "test-key"},
	})
	if _, err := p.Chat(ctx, req); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	return captured
}

func TestChatCarriesResponsesContextToOpenAICompatiblePayload(t *testing.T) {
	chatReq, err := provider.ResponsesToChatRequest(&provider.ResponsesRequest{
		Model: "gpt-4.1",
		Input: "hello",
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

	captured := chatAndCaptureRequest(t, nil, chatReq)

	if captured["user"] != "user-1" {
		t.Fatalf("user = %#v, want user-1", captured["user"])
	}
	if captured["parallel_tool_calls"] != true {
		t.Fatalf("parallel_tool_calls = %#v, want true", captured["parallel_tool_calls"])
	}
	if captured["store"] != false {
		t.Fatalf("store = %#v, want false", captured["store"])
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
}

func TestCompactCCDropsUnsupportedMetadataAndUser(t *testing.T) {
	chatReq, err := provider.ResponsesToChatRequest(&provider.ResponsesRequest{
		Model:     "gpt-4.1",
		Input:     "hello",
		Metadata:  map[string]any{"user_id": "abc123"},
		User:      "user-1",
		Reasoning: map[string]any{"effort": "high"},
	})
	if err != nil {
		t.Fatalf("ResponsesToChatRequest returned error: %v", err)
	}

	captured := chatAndCaptureRequest(t, map[string]any{"compact": "cc"}, chatReq)

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

func boolPtr(v bool) *bool {
	return &v
}
