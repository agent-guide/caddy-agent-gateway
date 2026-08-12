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
	if _, ok := captured["thinking"]; ok {
		t.Fatalf("thinking should default to the upstream model behavior: %#v", captured["thinking"])
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
	if !ok || thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v, want enabled for a reasoning request", captured["thinking"])
	}
}

func TestGeneratePreservesStandardEndpointReasoningEffort(t *testing.T) {
	req := &provider.ChatRequest{
		Model:    "glm-standard",
		Messages: []*schema.Message{{Role: schema.User, Content: "inspect"}},
		Options: []einomodel.Option{provider.WithChatExtraFields(&provider.ChatExtraFields{
			Thinking: map[string]any{
				"type":          "adaptive",
				"budget_tokens": 8192,
				"display":       "summarized",
			},
			ReasoningEffort: "xhigh",
		})},
	}
	_, captured := generateAndCaptureRequest(t, nil, req)
	thinking := captured["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v, want adaptive normalized to enabled", thinking)
	}
	if len(thinking) != 1 {
		t.Fatalf("thinking = %#v, want only GLM-supported type field", thinking)
	}
	if captured["reasoning_effort"] != "xhigh" {
		t.Fatalf("reasoning_effort = %#v, want xhigh preserved", captured["reasoning_effort"])
	}
}

func TestGeneratePreservesAssistantReasoningForToolReplay(t *testing.T) {
	req := &provider.ChatRequest{
		Model: "glm-5.2",
		Messages: []*schema.Message{
			{
				Role:             schema.Assistant,
				ReasoningContent: "需要先检查仓库",
				ToolCalls: []schema.ToolCall{{
					ID: "call_1", Type: "function",
					Function: schema.FunctionCall{Name: "read_file", Arguments: `{"path":"README.md"}`},
				}},
			},
			{Role: schema.Tool, ToolCallID: "call_1", Content: "contents"},
		},
		Options: []einomodel.Option{provider.WithChatExtraFields(&provider.ChatExtraFields{
			Thinking: map[string]any{"type": "enabled"},
		})},
	}

	_, captured := generateAndCaptureRequest(t, nil, req)
	messages := captured["messages"].([]any)
	assistant := messages[0].(map[string]any)
	if assistant["reasoning_content"] != "需要先检查仓库" {
		t.Fatalf("assistant reasoning_content = %#v", assistant["reasoning_content"])
	}
	thinking := captured["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v", thinking)
	}
}

func TestGenerateNormalizesCodingClientReasoningControls(t *testing.T) {
	req := &provider.ChatRequest{
		Model:    "glm-5.2",
		Messages: []*schema.Message{{Role: schema.User, Content: "inspect"}},
		Options: []einomodel.Option{provider.WithChatExtraFields(&provider.ChatExtraFields{
			Thinking:        map[string]any{"type": "adaptive"},
			ReasoningEffort: "xhigh",
		})},
	}
	_, captured := generateAndCaptureRequest(t, map[string]any{"api_profile": "coding_plan"}, req)
	thinking := captured["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v, want adaptive normalized to enabled", thinking)
	}
	if captured["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %#v, want xhigh normalized to max", captured["reasoning_effort"])
	}
}

func TestStreamEnablesToolStreamAndReturnsReasoning(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"分析\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := prov.StreamChat(context.Background(), &provider.ChatRequest{
		Model:    "glm-5.2",
		Messages: []*schema.Message{{Role: schema.User, Content: "inspect"}},
		Options: []einomodel.Option{einomodel.WithTools([]*schema.ToolInfo{{
			Name: "read_file", Desc: "Read a file",
		}})},
	})
	if err != nil {
		t.Fatalf("StreamChat returned error: %v", err)
	}
	defer stream.Close()
	chunk, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv returned error: %v", err)
	}
	if chunk.ReasoningContent != "分析" {
		t.Fatalf("reasoning_content = %q", chunk.ReasoningContent)
	}
	if captured["tool_stream"] != true {
		t.Fatalf("tool_stream = %#v, want true", captured["tool_stream"])
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

func TestCodingPlanCapabilitiesMatchGLM52(t *testing.T) {
	prov, err := New(provider.ProviderConfig{BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	caps := prov.Capabilities()
	if caps.ContextWindow != 1000000 || caps.MaxOutputTokens != 128000 {
		t.Fatalf("capabilities = %+v, want 1M context and 128K output", caps)
	}
	if caps.Vision || caps.Embeddings {
		t.Fatalf("coding endpoint capabilities = %+v, want text chat only", caps)
	}
}

func TestStandardCapabilitiesRemainConservative(t *testing.T) {
	prov, err := New(provider.ProviderConfig{BaseURL: "https://open.bigmodel.cn/api/paas/v4"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	caps := prov.Capabilities()
	if caps.ContextWindow != 128000 || caps.MaxOutputTokens != 8192 {
		t.Fatalf("capabilities = %+v, want 128K context and 8K output", caps)
	}
	if !caps.Vision || !caps.Embeddings {
		t.Fatalf("standard endpoint capabilities = %+v, want vision and embeddings", caps)
	}
}

func TestExplicitAPIProfileOverridesURLInference(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    string
		profile    string
		wantCtx    int
		wantOut    int
		wantVision bool
	}{
		{
			name: "coding plan through custom proxy path", baseURL: "https://proxy.example/v4",
			profile: "coding_plan", wantCtx: 1000000, wantOut: 128000, wantVision: false,
		},
		{
			name: "standard API mounted under coding-looking path", baseURL: "https://proxy.example/api/coding/v4",
			profile: "standard", wantCtx: 128000, wantOut: 8192, wantVision: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov, err := New(provider.ProviderConfig{
				BaseURL: tt.baseURL,
				Options: map[string]any{"api_profile": tt.profile},
			})
			if err != nil {
				t.Fatalf("New returned error: %v", err)
			}
			caps := prov.Capabilities()
			if caps.ContextWindow != tt.wantCtx || caps.MaxOutputTokens != tt.wantOut || caps.Vision != tt.wantVision {
				t.Fatalf("capabilities = %+v", caps)
			}
		})
	}
}

func TestCapabilityOptionsOverrideProfileDefaults(t *testing.T) {
	prov, err := New(provider.ProviderConfig{
		BaseURL: "https://proxy.example/v4",
		Options: map[string]any{
			"api_profile":       "coding_plan",
			"context_window":    "262144",
			"max_output_tokens": float64(32768),
			"vision":            "true",
			"embeddings":        true,
		},
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	caps := prov.Capabilities()
	if caps.ContextWindow != 262144 || caps.MaxOutputTokens != 32768 || !caps.Vision || !caps.Embeddings {
		t.Fatalf("capabilities = %+v, want explicit overrides", caps)
	}
}

func TestInvalidProfileAndCapabilityOptionsFailAtConstruction(t *testing.T) {
	tests := []map[string]any{
		{"api_profile": "other"},
		{"api_profile": true},
		{"context_window": "many"},
		{"max_output_tokens": 0},
		{"vision": "sometimes"},
		{"embeddings": 1},
	}
	for _, options := range tests {
		if _, err := New(provider.ProviderConfig{Options: options}); err == nil {
			t.Fatalf("New(%#v) succeeded, want error", options)
		}
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
