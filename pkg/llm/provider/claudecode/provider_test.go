package claudecode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/agent-guide/agent-gateway/pkg/httpclient"
	"github.com/agent-guide/agent-gateway/pkg/llm/credentialmgr"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

func TestChatUsesCLIAuthTokenBearerHeaders(t *testing.T) {
	var authHeader string
	var betaHeader string
	var acceptHeader string
	var requestPath string
	var sessionHeader string
	var reqBody messagesRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		betaHeader = r.Header.Get("anthropic-beta")
		acceptHeader = r.Header.Get("Accept")
		sessionHeader = r.Header.Get("x-claude-code-session-id")
		requestPath = r.URL.RequestURI()
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := provider.WithCredential(context.Background(), &credentialmgr.Credential{
		Type: credentialmgr.TypeCLIAuthToken,
		Metadata: map[string]any{
			"access_token": "sk-ant-oat-test",
		},
	})
	resp, err := prov.Chat(ctx, &provider.ChatRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []*schema.Message{
			schema.SystemMessage("system prompt"),
			schema.UserMessage("hello"),
		},
		Options: []model.Option{model.WithMaxTokens(512)},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if authHeader != "Bearer sk-ant-oat-test" {
		t.Fatalf("authorization = %q, want Bearer sk-ant-oat-test", authHeader)
	}
	if betaHeader != anthropicBeta {
		t.Fatalf("anthropic-beta = %q, want %q", betaHeader, anthropicBeta)
	}
	if acceptHeader != "application/json" {
		t.Fatalf("accept = %q, want application/json", acceptHeader)
	}
	if sessionHeader == "" {
		t.Fatal("x-claude-code-session-id is empty")
	}
	if requestPath != "/v1/messages?beta=true" {
		t.Fatalf("request path = %q, want /v1/messages?beta=true", requestPath)
	}
	if len(reqBody.System) != 2 {
		t.Fatalf("system = %+v, want CLI preamble block plus user system prompt", reqBody.System)
	}
	if reqBody.System[0].Text != claudeCodeSystemPreamble {
		t.Fatalf("first system block = %q, want %q", reqBody.System[0].Text, claudeCodeSystemPreamble)
	}
	if reqBody.System[0].CacheControl == nil || reqBody.System[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("preamble block cache_control = %+v, want ephemeral", reqBody.System[0].CacheControl)
	}
	if reqBody.System[len(reqBody.System)-1].Text != "system prompt" {
		t.Fatalf("last system block = %q, want system prompt", reqBody.System[len(reqBody.System)-1].Text)
	}
	if len(reqBody.Messages) != 1 || reqBody.Messages[0].Role != "user" || reqBody.Messages[0].Content[0].Text != "hello" {
		t.Fatalf("messages = %+v, want one user message", reqBody.Messages)
	}
	if reqBody.Messages[0].Content[0].CacheControl == nil || reqBody.Messages[0].Content[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("user content cache_control = %+v, want ephemeral", reqBody.Messages[0].Content[0].CacheControl)
	}
	if reqBody.MaxTokens != 512 {
		t.Fatalf("max_tokens = %d, want 512", reqBody.MaxTokens)
	}
	if reqBody.Metadata.UserID == "" {
		t.Fatal("metadata.user_id is empty")
	}
	if reqBody.Thinking == nil || reqBody.Thinking.Type != "adaptive" {
		t.Fatalf("thinking = %+v, want adaptive", reqBody.Thinking)
	}
	if reqBody.ContextManagement == nil || len(reqBody.ContextManagement.Edits) != 1 {
		t.Fatalf("context_management = %+v, want one edit", reqBody.ContextManagement)
	}
	if reqBody.OutputConfig == nil || reqBody.OutputConfig.Effort != "high" {
		t.Fatalf("output_config = %+v, want effort=high", reqBody.OutputConfig)
	}
	if len(reqBody.Tools) != 0 {
		t.Fatalf("tools = %+v, want empty list", reqBody.Tools)
	}
	if resp.Message == nil || resp.Message.Content != "hello" {
		t.Fatalf("unexpected response = %+v", resp)
	}
	if resp.Message.ResponseMeta == nil || resp.Message.ResponseMeta.Usage == nil || resp.Message.ResponseMeta.Usage.PromptTokens != 12 || resp.Message.ResponseMeta.Usage.CompletionTokens != 34 {
		t.Fatalf("unexpected usage = %+v", resp.Message.ResponseMeta)
	}
}

func TestChatAppliesConfiguredExtraHeaders(t *testing.T) {
	var dangerousHeader string
	var xApp string
	var stainlessRuntime string
	var userAgent string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dangerousHeader = r.Header.Get("anthropic-dangerous-direct-browser-access")
		xApp = r.Header.Get("x-app")
		stainlessRuntime = r.Header.Get("x-stainless-runtime")
		userAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{
			RequestTimeoutSeconds: 5,
			ExtraHeaders: map[string]string{
				"Anthropic-Dangerous-Direct-Browser-Access": "true",
				"User-Agent":          "claude-cli/2.1.158 (external, cli)",
				"X-App":               "cli",
				"X-Stainless-Runtime": "node",
			},
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = prov.Chat(context.Background(), &provider.ChatRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []*schema.Message{
			schema.UserMessage("hello"),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if dangerousHeader != "true" {
		t.Fatalf("anthropic-dangerous-direct-browser-access = %q, want true", dangerousHeader)
	}
	if userAgent != "claude-cli/2.1.158 (external, cli)" {
		t.Fatalf("user-agent = %q, want configured value", userAgent)
	}
	if xApp != "cli" {
		t.Fatalf("x-app = %q, want cli", xApp)
	}
	if stainlessRuntime != "node" {
		t.Fatalf("x-stainless-runtime = %q, want node", stainlessRuntime)
	}
}

func TestChatSendsClaudeCodeFingerprintDefaultsWithoutExtraHeaders(t *testing.T) {
	var got http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = prov.Chat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("hello")},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	// With no ExtraHeaders configured, the provider must still present the full
	// Claude Code fingerprint rather than Go's default User-Agent.
	for name, want := range defaultClaudeCodeFingerprintHeaders {
		if got := got.Get(name); got != want {
			t.Fatalf("default header %s = %q, want %q", name, got, want)
		}
	}
	if ua := got.Get("User-Agent"); ua == "Go-http-client/1.1" {
		t.Fatalf("User-Agent leaked Go default %q", ua)
	}
}

func TestChatExtraHeadersOverrideFingerprintDefaults(t *testing.T) {
	var userAgent, anthropicBetaHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.Header.Get("User-Agent")
		anthropicBetaHeader = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{
			RequestTimeoutSeconds: 5,
			ExtraHeaders: map[string]string{
				"User-Agent":     "claude-cli/9.9.9 (external, cli)",
				"anthropic-beta": "override-beta-flag",
			},
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = prov.Chat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("hello")},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if userAgent != "claude-cli/9.9.9 (external, cli)" {
		t.Fatalf("user-agent = %q, want ExtraHeaders override", userAgent)
	}
	if anthropicBetaHeader != "override-beta-flag" {
		t.Fatalf("anthropic-beta = %q, want ExtraHeaders override", anthropicBetaHeader)
	}
}

func TestChatUsesAPIKeyHeaderForManagedAPIKeyCredential(t *testing.T) {
	var authHeader string
	var apiKeyHeader string
	var reqBody messagesRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		apiKeyHeader = r.Header.Get("x-api-key")
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := provider.WithCredential(context.Background(), &credentialmgr.Credential{
		Type: credentialmgr.TypeAPIKey,
		Attributes: map[string]string{
			"api_key": "sk-ant-api-test",
		},
	})
	resp, err := prov.Chat(ctx, &provider.ChatRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []*schema.Message{
			schema.UserMessage("hello"),
		},
		Options: []model.Option{model.WithMaxTokens(512)},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if authHeader != "Bearer sk-ant-api-test" {
		t.Fatalf("authorization = %q, want Bearer sk-ant-api-test", authHeader)
	}
	if apiKeyHeader != "" {
		t.Fatalf("x-api-key = %q, want empty", apiKeyHeader)
	}
	if len(reqBody.Messages) != 1 || reqBody.Messages[0].Role != "user" || reqBody.Messages[0].Content[0].Text != "hello" {
		t.Fatalf("messages = %+v, want one user message", reqBody.Messages)
	}
	if resp.Message == nil || resp.Message.Content != "hello" {
		t.Fatalf("unexpected response = %+v", resp)
	}
}

func TestChatUsesBearerAuthorizationForProviderFallbackAPIKey(t *testing.T) {
	var authHeader string
	var apiKeyHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		apiKeyHeader = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = prov.Chat(context.Background(), &provider.ChatRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []*schema.Message{
			schema.UserMessage("hello"),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if authHeader != "Bearer sk-ant-api-fallback" {
		t.Fatalf("authorization = %q, want Bearer sk-ant-api-fallback", authHeader)
	}
	if apiKeyHeader != "" {
		t.Fatalf("x-api-key = %q, want empty", apiKeyHeader)
	}
}

func TestChatUsesXAPIKeyHeaderWhenAPIKeyHeaderIsXAPIKey(t *testing.T) {
	var authHeader string
	var apiKeyHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		apiKeyHeader = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-ant-api-fallback",
		Options: map[string]any{
			"api_key_header": "x-api-key",
		},
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = prov.Chat(context.Background(), &provider.ChatRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []*schema.Message{
			schema.UserMessage("hello"),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if authHeader != "" {
		t.Fatalf("authorization = %q, want empty", authHeader)
	}
	if apiKeyHeader != "sk-ant-api-fallback" {
		t.Fatalf("x-api-key = %q, want sk-ant-api-fallback", apiKeyHeader)
	}
}

func TestNewRejectsInvalidAPIKeyHeader(t *testing.T) {
	_, err := New(provider.ProviderConfig{
		APIKey: "sk-ant-api-fallback",
		Options: map[string]any{
			"api_key_header": "bearer",
		},
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err == nil {
		t.Fatal("New() error = nil, want invalid api_key_header error")
	}
}

func TestNewRejectsInvalidCompact(t *testing.T) {
	for _, value := range []any{"yes-ish", 1} {
		_, err := New(provider.ProviderConfig{
			APIKey: "sk-ant-api-fallback",
			Options: map[string]any{
				"compact": value,
			},
			Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
		})
		if err == nil {
			t.Fatalf("New() error = nil for compact=%#v, want invalid option error", value)
		}
	}
}

func TestChatBuildsClaudeCodeStyleBody(t *testing.T) {
	var reqBody messagesRequest

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
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = prov.Chat(context.Background(), &provider.ChatRequest{
		Model: "upstream-model",
		Messages: []*schema.Message{
			schema.SystemMessage("system prompt"),
			{Role: schema.Assistant, Content: "prior answer"},
			schema.UserMessage("hello"),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if reqBody.Model != "upstream-model" {
		t.Fatalf("model = %q, want upstream-model", reqBody.Model)
	}
	if reqBody.MaxTokens != defaultClaudeCodeMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", reqBody.MaxTokens, defaultClaudeCodeMaxTokens)
	}
	if len(reqBody.System) != 2 {
		t.Fatalf("system = %+v, want CLI preamble block plus user system prompt", reqBody.System)
	}
	if reqBody.System[0].Text != claudeCodeSystemPreamble {
		t.Fatalf("first system block = %q, want %q", reqBody.System[0].Text, claudeCodeSystemPreamble)
	}
	if reqBody.System[0].CacheControl == nil || reqBody.System[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("preamble block cache_control = %+v, want ephemeral", reqBody.System[0].CacheControl)
	}
	if reqBody.System[len(reqBody.System)-1].Text != "system prompt" {
		t.Fatalf("last system block = %q, want system prompt", reqBody.System[len(reqBody.System)-1].Text)
	}
	if len(reqBody.Messages) != 2 {
		t.Fatalf("messages = %+v, want assistant and user messages", reqBody.Messages)
	}
	if reqBody.Messages[0].Role != "assistant" || reqBody.Messages[0].Content[0].Text != "prior answer" {
		t.Fatalf("assistant message = %+v, want prior answer", reqBody.Messages[0])
	}
	if reqBody.Messages[1].Role != "user" || reqBody.Messages[1].Content[0].Text != "hello" {
		t.Fatalf("user message = %+v, want hello", reqBody.Messages[1])
	}
	if reqBody.Messages[1].Content[0].CacheControl == nil || reqBody.Messages[1].Content[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("user cache_control = %+v, want ephemeral", reqBody.Messages[1].Content[0].CacheControl)
	}
	if reqBody.Metadata.UserID == "" {
		t.Fatal("metadata.user_id is empty")
	}
	if reqBody.Thinking == nil || reqBody.Thinking.Type != "adaptive" {
		t.Fatalf("thinking = %+v, want adaptive", reqBody.Thinking)
	}
	if reqBody.ContextManagement == nil || len(reqBody.ContextManagement.Edits) != 1 {
		t.Fatalf("context_management = %+v, want one edit", reqBody.ContextManagement)
	}
	if reqBody.OutputConfig == nil || reqBody.OutputConfig.Effort != "high" {
		t.Fatalf("output_config = %+v, want effort=high", reqBody.OutputConfig)
	}
	if len(reqBody.Tools) != 0 {
		t.Fatalf("tools = %+v, want empty list", reqBody.Tools)
	}
}

func TestChatPreservesToolChoiceNone(t *testing.T) {
	var reqBody messagesRequest

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
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = prov.Chat(context.Background(), &provider.ChatRequest{
		Model: "upstream-model",
		Messages: []*schema.Message{
			schema.UserMessage("hello"),
		},
		Options: []model.Option{
			model.WithTools([]*schema.ToolInfo{{Name: "lookup"}}),
			model.WithToolChoice(schema.ToolChoiceForbidden),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if string(reqBody.ToolChoice) != `{"type":"none"}` {
		t.Fatalf("tool_choice = %s, want {\"type\":\"none\"}", string(reqBody.ToolChoice))
	}
}

func TestChatDerivesEffortAndKeepsGenuineMetadata(t *testing.T) {
	var reqBody messagesRequest

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
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	store := false
	parallel := true
	_, err = prov.Chat(context.Background(), &provider.ChatRequest{
		Model: "upstream-model",
		Messages: []*schema.Message{
			schema.UserMessage("hello"),
		},
		Options: []model.Option{
			provider.WithChatExtraFields(&provider.ChatExtraFields{
				ResponseFormat:    map[string]any{"type": "json_object"},
				ReasoningEffort:   "low",
				User:              "chat-user",
				Metadata:          map[string]any{"trace_id": "chat-trace"},
				ParallelToolCalls: &parallel,
				Store:             &store,
			}),
			provider.WithResponsesRequestContext(&provider.ResponsesRequestContext{
				PreviousResponseID: "resp_prev",
				Store:              &store,
				Text:               map[string]any{"format": map[string]any{"type": "text"}},
				Metadata:           map[string]any{"trace_id": "responses-trace"},
				User:               "responses-user",
				Reasoning:          map[string]any{"effort": "medium"},
				ParallelToolCalls:  &parallel,
				Truncation:         "auto",
			}),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	// The chat-completions reasoning_effort wins over the Responses reasoning object.
	if reqBody.OutputConfig == nil || reqBody.OutputConfig.Effort != "low" {
		t.Fatalf("output_config = %+v, want effort=low from chat extra", reqBody.OutputConfig)
	}

	// metadata.user_id must stay byte-identical to the genuine CLI shape: no
	// preserved request context is smuggled into it.
	userID := decodeMetadataUserID(t, reqBody.Metadata.UserID)
	for _, key := range []string{"chat_extra", "responses", "request_user"} {
		if _, ok := userID[key]; ok {
			t.Fatalf("metadata user_id must not carry preserved request context, found %q: %#v", key, userID)
		}
	}
	if len(userID) != 3 {
		t.Fatalf("metadata user_id = %#v, want exactly device_id/account_uuid/session_id", userID)
	}
	if userID["device_id"] == "" || userID["session_id"] == "" {
		t.Fatalf("metadata user_id missing genuine CLI fields: %#v", userID)
	}
	if _, ok := userID["account_uuid"]; !ok {
		t.Fatalf("metadata user_id missing account_uuid: %#v", userID)
	}
}

func TestCreateResponsesUsesChatCompatibility(t *testing.T) {
	var reqBody messagesRequest

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
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	responsesProv, ok := prov.(provider.ResponsesProvider)
	if !ok {
		t.Fatal("claudecode provider does not implement ResponsesProvider")
	}

	parallel := true
	resp, err := responsesProv.CreateResponses(context.Background(), &provider.ResponsesRequest{
		Model: "upstream-model",
		Input: "hello",
		Tools: []provider.ResponsesToolDefinition{{
			Type:        "function",
			Name:        "lookup",
			Description: "Lookup data",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
		ToolChoice:         json.RawMessage(`{"type":"function","name":"lookup"}`),
		MaxOutputTokens:    123,
		Temperature:        0.25,
		TopP:               0.75,
		PreviousResponseID: "resp_prev",
		User:               "responses-user",
		Metadata:           map[string]any{"trace_id": "responses-trace"},
		Reasoning:          map[string]any{"effort": "medium"},
		ParallelToolCalls:  &parallel,
		Truncation:         "auto",
	})
	if err != nil {
		t.Fatalf("CreateResponses() error = %v", err)
	}
	if resp == nil || len(resp.Output) == 0 {
		t.Fatalf("responses response = %+v, want output", resp)
	}

	if reqBody.Model != "upstream-model" || reqBody.MaxTokens != 123 {
		t.Fatalf("request model/max_tokens = %q/%d, want upstream-model/123", reqBody.Model, reqBody.MaxTokens)
	}
	if reqBody.Temperature != 0.25 || reqBody.TopP != 0.75 {
		t.Fatalf("sampling = %v/%v, want 0.25/0.75", reqBody.Temperature, reqBody.TopP)
	}
	if len(reqBody.Tools) != 1 || reqBody.Tools[0].Name != "lookup" {
		t.Fatalf("tools = %+v, want lookup tool", reqBody.Tools)
	}
	if string(reqBody.ToolChoice) != `{"name":"lookup","type":"tool"}` {
		t.Fatalf("tool_choice = %s, want named Anthropic tool choice", string(reqBody.ToolChoice))
	}
	if reqBody.OutputConfig == nil || reqBody.OutputConfig.Effort != "medium" {
		t.Fatalf("output_config = %+v, want effort=medium", reqBody.OutputConfig)
	}
	// Responses fields without a Messages API equivalent are dropped rather than
	// smuggled into metadata.user_id, which stays in the genuine CLI shape.
	userID := decodeMetadataUserID(t, reqBody.Metadata.UserID)
	if len(userID) != 3 {
		t.Fatalf("metadata user_id = %#v, want exactly device_id/account_uuid/session_id", userID)
	}
	for _, key := range []string{"chat_extra", "responses", "request_user"} {
		if _, ok := userID[key]; ok {
			t.Fatalf("metadata user_id must not carry preserved request context, found %q: %#v", key, userID)
		}
	}
}

func codexCompatChatRequest() (*provider.ChatRequest, *schema.ToolInfo, *schema.Message) {
	tool := &schema.ToolInfo{Name: "exec_command", Desc: "Runs a command."}
	priorAssistant := &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID: "toolu_prev",
			Function: schema.FunctionCall{
				Name:      "exec_command",
				Arguments: `{"cmd":"pwd"}`,
			},
		}},
	}
	req := &provider.ChatRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []*schema.Message{
			schema.UserMessage("run a command"),
			priorAssistant,
			{
				Role:       schema.Tool,
				ToolCallID: "toolu_prev",
				Content:    "/tmp\n",
			},
		},
		Options: []model.Option{model.WithTools([]*schema.ToolInfo{tool})},
	}
	return req, tool, priorAssistant
}

func TestChatMapsCodexToolNamesForClaudeCode(t *testing.T) {
	var reqBody messagesRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"printf ok"}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
		Options: map[string]any{"compact": "codex"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req, tool, priorAssistant := codexCompatChatRequest()
	resp, err := prov.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if len(reqBody.Tools) != 1 || reqBody.Tools[0].Name != "Bash" {
		t.Fatalf("request tools = %+v, want Bash alias", reqBody.Tools)
	}
	if len(reqBody.Messages) != 3 || len(reqBody.Messages[1].Content) != 1 || reqBody.Messages[1].Content[0].Name != "Bash" {
		t.Fatalf("history messages = %+v, want prior assistant tool_use mapped to Bash", reqBody.Messages)
	}
	if resp == nil || resp.Message == nil || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("response = %+v, want one tool call", resp)
	}
	if got := resp.Message.ToolCalls[0].Function.Name; got != "exec_command" {
		t.Fatalf("response tool name = %q, want exec_command", got)
	}

	// The aliasing happens on the freshly built wire request, so the caller's
	// tool definitions and message history must keep their original Codex names.
	if tool.Name != "exec_command" {
		t.Fatalf("tool definition name mutated to %q, want exec_command", tool.Name)
	}
	if got := priorAssistant.ToolCalls[0].Function.Name; got != "exec_command" {
		t.Fatalf("history tool call name mutated to %q, want exec_command", got)
	}
}

func TestChatKeepsCodexToolNamesWithoutCompat(t *testing.T) {
	var reqBody messagesRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"toolu_1","name":"exec_command","input":{"cmd":"printf ok"}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req, _, _ := codexCompatChatRequest()
	resp, err := prov.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if len(reqBody.Tools) != 1 || reqBody.Tools[0].Name != "exec_command" {
		t.Fatalf("request tools = %+v, want unchanged exec_command", reqBody.Tools)
	}
	if resp == nil || resp.Message == nil || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("response = %+v, want one tool call", resp)
	}
	if got := resp.Message.ToolCalls[0].Function.Name; got != "exec_command" {
		t.Fatalf("response tool name = %q, want exec_command", got)
	}
}

func TestChatRestoresCodexToolNamesAfterRetry(t *testing.T) {
	var attempts int
	var reqBody messagesRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"printf ok"}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5, MaxRetries: 2, RetryDelaySeconds: 0},
		Options: map[string]any{"compact": "codex"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req, _, _ := codexCompatChatRequest()
	resp, err := prov.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if attempts < 2 {
		t.Fatalf("attempts = %d, want a retry after the first failure", attempts)
	}
	// The second attempt must still alias the request and restore the response
	// name; the first attempt must not have left the shared state renamed.
	if len(reqBody.Tools) != 1 || reqBody.Tools[0].Name != "Bash" {
		t.Fatalf("retried request tools = %+v, want Bash alias", reqBody.Tools)
	}
	if resp == nil || resp.Message == nil || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("response = %+v, want one tool call", resp)
	}
	if got := resp.Message.ToolCalls[0].Function.Name; got != "exec_command" {
		t.Fatalf("response tool name = %q, want exec_command", got)
	}
}

func TestChatRejectsCodexCompatToolNameCollision(t *testing.T) {
	prov, err := New(provider.ProviderConfig{
		BaseURL: "http://127.0.0.1:1",
		APIKey:  "sk-ant-api-fallback",
		Options: map[string]any{"compact": "codex"},
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = prov.Chat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("run a command")},
		Options: []model.Option{
			model.WithTools([]*schema.ToolInfo{
				{Name: "exec_command", Desc: "Runs a command."},
				{Name: "Bash", Desc: "Runs a shell command."},
			}),
		},
	})
	if err == nil {
		t.Fatal("Chat() error = nil, want compact=codex tool name collision")
	}
}

func TestNormalizeEffort(t *testing.T) {
	cases := map[string]string{
		"low":     "low",
		"LOW":     "low",
		" medium": "medium",
		"high":    "high",
		"minimal": "low",
		"":        defaultClaudeCodeEffort,
		"bogus":   defaultClaudeCodeEffort,
	}
	for in, want := range cases {
		if got := normalizeEffort(in); got != want {
			t.Errorf("normalizeEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStreamChatParsesAnthropicSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("accept = %q, want text/event-stream", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, "data: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hel\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, "data: {\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n")
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := provider.WithCredential(context.Background(), &credentialmgr.Credential{
		Type: credentialmgr.TypeCLIAuthToken,
		Metadata: map[string]any{
			"access_token": "sk-ant-oat-test",
		},
	})
	stream, err := prov.StreamChat(ctx, &provider.ChatRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []*schema.Message{
			schema.UserMessage("hello"),
		},
	})
	if err != nil {
		t.Fatalf("StreamChat() error = %v", err)
	}
	defer stream.Close()

	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("first recv: %v", err)
	}
	second, err := stream.Recv()
	if err != nil {
		t.Fatalf("second recv: %v", err)
	}
	if first.Content != "hel" || second.Content != "lo" {
		t.Fatalf("unexpected chunks: first=%+v second=%+v", first, second)
	}
}

func TestStreamChatCapturesFinalUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, "data: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\n")
		_, _ = io.WriteString(w, "data: {\"usage\":{\"output_tokens\":34},\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stream, err := prov.StreamChat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("hello")},
	})
	if err != nil {
		t.Fatalf("StreamChat() error = %v", err)
	}
	defer stream.Close()

	var finalUsage *schema.TokenUsage
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
			finalUsage = msg.ResponseMeta.Usage
		}
	}
	if finalUsage == nil {
		t.Fatal("final streamed usage = nil, want token usage on message_delta")
	}
	if finalUsage.PromptTokens != 12 || finalUsage.CompletionTokens != 34 {
		t.Fatalf("usage = %+v, want prompt=12 completion=34", finalUsage)
	}
}

func TestStreamChatMapsCodexToolNamesForClaudeCode(t *testing.T) {
	var reqBody messagesRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: content_block_start\n")
		_, _ = io.WriteString(w, "data: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"Bash\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, "data: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"cmd\\\":\\\"pwd\\\"}\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\n")
		_, _ = io.WriteString(w, "data: {\"index\":0}\n\n")
	}))
	defer server.Close()

	prov, err := New(provider.ProviderConfig{
		BaseURL: server.URL,
		APIKey:  "sk-ant-api-fallback",
		Options: map[string]any{"compact": "codex"},
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stream, err := prov.StreamChat(context.Background(), &provider.ChatRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []*schema.Message{schema.UserMessage("run a command")},
		Options: []model.Option{
			model.WithTools([]*schema.ToolInfo{{Name: "exec_command", Desc: "Runs a command."}}),
		},
	})
	if err != nil {
		t.Fatalf("StreamChat() error = %v", err)
	}
	defer stream.Close()

	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if len(reqBody.Tools) != 1 || reqBody.Tools[0].Name != "Bash" {
		t.Fatalf("request tools = %+v, want Bash alias", reqBody.Tools)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want one tool call", msg.ToolCalls)
	}
	tc := msg.ToolCalls[0]
	if tc.Function.Name != "exec_command" || tc.Function.Arguments != `{"cmd":"pwd"}` {
		t.Fatalf("tool call = %+v, want restored exec_command", tc)
	}
}

func TestChatMapsResponsesTextFormatToOutputConfigFormat(t *testing.T) {
	var reqBody messagesRequest

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
		APIKey:  "sk-ant-api-fallback",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = prov.Chat(context.Background(), &provider.ChatRequest{
		Model:    "upstream-model",
		Messages: []*schema.Message{schema.UserMessage("hello")},
		Options: []model.Option{
			provider.WithResponsesRequestContext(&provider.ResponsesRequestContext{
				Text: map[string]any{
					"format": map[string]any{
						"type":   "json_schema",
						"schema": map[string]any{"type": "object"},
					},
				},
			}),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if reqBody.OutputConfig == nil {
		t.Fatal("output_config = nil, want effort and format")
	}
	if reqBody.OutputConfig.Effort != "high" {
		t.Fatalf("output_config effort = %q, want high preserved alongside format", reqBody.OutputConfig.Effort)
	}
	if reqBody.OutputConfig.Format == nil || reqBody.OutputConfig.Format.Type != "json_schema" {
		t.Fatalf("output_config format = %+v, want json_schema from responses text.format", reqBody.OutputConfig.Format)
	}
}

func decodeMetadataUserID(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode metadata.user_id: %v", err)
	}
	return out
}
