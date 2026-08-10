package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/credential"
	sched "github.com/agent-guide/agent-gateway/pkg/credential/scheduler"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/cloudwego/eino/schema"
)

type testProvider struct {
	streamErr error
	cfg       provider.ProviderConfig
}

func (p *testProvider) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, nil
}

func (p *testProvider) StreamChat(context.Context, *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	return nil, p.streamErr
}

func (p *testProvider) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return nil, nil
}

func (p *testProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{Streaming: true}
}

func (p *testProvider) Config() provider.ProviderConfig {
	return p.cfg
}

type testStreamingProvider struct {
	cfg    provider.ProviderConfig
	chunks []*schema.Message
}

func (p *testStreamingProvider) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, nil
}

func (p *testStreamingProvider) StreamChat(context.Context, *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	sr, sw := schema.Pipe[*schema.Message](8)
	go func() {
		defer sw.Close()
		for _, chunk := range p.chunks {
			sw.Send(chunk, nil)
		}
	}()
	return sr, nil
}

func (p *testStreamingProvider) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return nil, nil
}

func (p *testStreamingProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{Streaming: true}
}

func (p *testStreamingProvider) Config() provider.ProviderConfig {
	return p.cfg
}

type testStatusError struct {
	msg    string
	status int
}

func (e testStatusError) Error() string   { return e.msg }
func (e testStatusError) StatusCode() int { return e.status }

type testCredentialMarkingProvider struct {
	base      provider.Provider
	scheduler sched.CredentialScheduler
	credID    string
}

func (p *testCredentialMarkingProvider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	resp, err := p.base.Chat(ctx, req)
	p.mark(req.Model, err)
	return resp, err
}

func (p *testCredentialMarkingProvider) StreamChat(ctx context.Context, req *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	stream, err := p.base.StreamChat(ctx, req)
	p.mark(req.Model, err)
	return stream, err
}

func (p *testCredentialMarkingProvider) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	return p.base.ListModels(ctx)
}

func (p *testCredentialMarkingProvider) Capabilities() provider.ProviderCapabilities {
	return p.base.Capabilities()
}

func (p *testCredentialMarkingProvider) Config() provider.ProviderConfig {
	return p.base.Config()
}

func (p *testCredentialMarkingProvider) mark(model string, err error) {
	if p.scheduler == nil || p.credID == "" {
		return
	}
	result := sched.Result{CredentialID: p.credID, Model: model, Success: err == nil}
	if err != nil {
		status := http.StatusBadGateway
		var sc interface{ StatusCode() int }
		if errors.As(err, &sc) {
			status = sc.StatusCode()
		}
		result.Error = &sched.Error{
			Code:       http.StatusText(status),
			Message:    err.Error(),
			HTTPStatus: status,
			Retryable:  status == http.StatusTooManyRequests || status >= 500,
		}
	}
	p.scheduler.MarkResult(context.Background(), result)
}

func newTestCredentialScheduler(t *testing.T, mgr *credential.Manager) sched.CredentialScheduler {
	t.Helper()
	scheduler := sched.NewScheduler(nil)
	listener, ok := scheduler.(credential.CredentialLifecycleListener)
	if !ok {
		t.Fatal("scheduler does not implement CredentialLifecycleListener")
	}
	mgr.AddListener(listener)
	scheduler.Rebuild(mgr.ListCredentials(credential.Filter{}))
	return scheduler
}

func TestMatchLLMApiIncludesCountTokens(t *testing.T) {
	handler := NewHandler(nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)

	if !handler.MatchLLMApi(req) {
		t.Fatal("MatchLLMApi returned false for /v1/messages/count_tokens")
	}
}

func TestServeLLMApiCountTokensReturnsNotImplemented(t *testing.T) {
	handler := NewHandler(nil)
	body, err := json.Marshal(MessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "hello world"}}}},
		Tools: []ToolDefinition{{
			Name:        "lookup",
			Description: "Lookup data",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens?beta=true", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, &testProvider{}, nil); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
}

func TestServeLLMApiMarksAnthropicStreamFailures(t *testing.T) {
	credMgr := credential.NewManager(nil)
	if err := credMgr.RegisterCredential(context.Background(), &credential.Credential{
		ID:           "cred-anthropic-1",
		ProviderType: "anthropic",
		ProviderID:   "anthropic",
		Scope:        "id:anthropic",
		Type:         credential.TypeAPIKey,
	}); err != nil {
		t.Fatalf("register credential: %v", err)
	}

	baseProv := &testProvider{
		streamErr: testStatusError{msg: "rate limit", status: http.StatusTooManyRequests},
		cfg: provider.ProviderConfig{
			Id:           "anthropic",
			ProviderType: "anthropic",
		},
	}
	scheduler := newTestCredentialScheduler(t, credMgr)
	prov := &testCredentialMarkingProvider{base: baseProv, scheduler: scheduler, credID: "cred-anthropic-1"}
	handler := NewHandler(nil)

	body, err := json.Marshal(MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Stream:    true,
		Messages: []MessageItem{{
			Role: "user",
			Content: []ContentBlock{{
				Type: "text",
				Text: "hello",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); !strings.Contains(body, "event: error") || !strings.Contains(body, "rate limit") {
		t.Fatalf("stream body = %s, want Anthropic error event", body)
	}

	_, err = scheduler.Pick(context.Background(), sched.Filter{
		Type:            credential.TypeAPIKey,
		CredentialScope: "id:anthropic",
		Model:           "claude-sonnet-4-5",
	}, nil)
	if err == nil {
		t.Fatal("expected scheduler to reject quota-exceeded credential")
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if !errors.As(err, &sc) || sc.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("expected 429 scheduler error, got %v", err)
	}
}

func TestPrepareLLMApiRequestAcceptsSystemBlockArray(t *testing.T) {
	handler := NewHandler(nil)

	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":16,
		"stream":false,
		"system":[
			{"type":"text","text":"You are Claude Code."},
			{"type":"text","text":"Follow the user's instructions."}
		],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hello"}]}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	if prepared == nil || prepared.ChatRequest == nil {
		t.Fatal("prepared chat request is nil")
	}
	if len(prepared.ChatRequest.Messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(prepared.ChatRequest.Messages))
	}
	if prepared.ChatRequest.Messages[0].Role != schema.System {
		t.Fatalf("first role = %q, want %q", prepared.ChatRequest.Messages[0].Role, schema.System)
	}
	wantSystem := "You are Claude Code.\nFollow the user's instructions."
	if prepared.ChatRequest.Messages[0].Content != wantSystem {
		t.Fatalf("system content = %q, want %q", prepared.ChatRequest.Messages[0].Content, wantSystem)
	}
}

func TestPrepareLLMApiRequestAcceptsStringSystemPrompt(t *testing.T) {
	handler := NewHandler(nil)

	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":16,
		"stream":false,
		"system":"You are a helpful assistant.",
		"messages":[
			{"role":"user","content":"hello"}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	if prepared == nil || prepared.ChatRequest == nil {
		t.Fatal("prepared chat request is nil")
	}
	if len(prepared.ChatRequest.Messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(prepared.ChatRequest.Messages))
	}
	if prepared.ChatRequest.Messages[0].Role != schema.System {
		t.Fatalf("first role = %q, want %q", prepared.ChatRequest.Messages[0].Role, schema.System)
	}
	if prepared.ChatRequest.Messages[0].Content != "You are a helpful assistant." {
		t.Fatalf("system content = %q", prepared.ChatRequest.Messages[0].Content)
	}
}

func TestPrepareLLMApiRequestPreservesToolResultOrder(t *testing.T) {
	handler := NewHandler(nil)

	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":16,
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"before"},
				{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"tool output"}]},
				{"type":"text","text":"after"}
			]}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	got := prepared.ChatRequest.Messages
	if len(got) != 3 {
		t.Fatalf("message count = %d, want 3", len(got))
	}
	if got[0].Role != schema.User || got[0].Content != "before" {
		t.Fatalf("first message = %+v, want user before", got[0])
	}
	if got[1].Role != schema.Tool || got[1].ToolCallID != "call_1" || got[1].Content != "tool output" {
		t.Fatalf("second message = %+v, want tool_result for call_1", got[1])
	}
	if got[2].Role != schema.User || got[2].Content != "after" {
		t.Fatalf("third message = %+v, want user after", got[2])
	}
}

func TestServeLLMApiStreamToolOnlyOmitsEmptyTextBlock(t *testing.T) {
	handler := NewHandler(nil)
	prov := &testStreamingProvider{
		cfg: provider.ProviderConfig{Id: "claudecode", ProviderType: "claudecode"},
		chunks: []*schema.Message{{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name:      "lookup",
					Arguments: `{"q":"hello"}`,
				},
			}},
			ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_use"},
		}},
	}

	body, err := json.Marshal(MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Stream:    true,
		Messages: []MessageItem{{
			Role:    "user",
			Content: MessageContent{{Type: "text", Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}

	payload, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyText := string(payload)
	if strings.Contains(bodyText, `"content_block":{"type":"text","text":""}`) {
		t.Fatalf("unexpected empty text block in stream: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"content_block":{"id":"call_1","input":{},"name":"lookup","type":"tool_use"}`) {
		t.Fatalf("missing tool_use block in stream: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"stop_reason":"tool_use"`) {
		t.Fatalf("missing tool_use stop_reason in stream: %s", bodyText)
	}
}

func TestServeLLMApiStreamEmitsStatefulToolUse(t *testing.T) {
	handler := NewHandler(nil)
	prov := &testStreamingProvider{
		cfg: provider.ProviderConfig{Id: "codex", ProviderType: "codex"},
		chunks: []*schema.Message{{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name:      "Agent",
					Arguments: `{"name":"researcher"}`,
				},
			}},
			ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_use"},
		}},
	}

	body, err := json.Marshal(MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Stream:    true,
		Messages: []MessageItem{{
			Role:    "user",
			Content: MessageContent{{Type: "text", Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}

	payload, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyText := string(payload)
	if !strings.Contains(bodyText, `"type":"tool_use"`) || !strings.Contains(bodyText, `"name":"Agent"`) {
		t.Fatalf("missing Agent tool_use in stream: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"stop_reason":"tool_use"`) {
		t.Fatalf("missing tool_use stop_reason in stream: %s", bodyText)
	}
}

func TestServeLLMApiStreamTextAfterToolUsesTextBlockIndex(t *testing.T) {
	handler := NewHandler(nil)
	prov := &testStreamingProvider{
		cfg: provider.ProviderConfig{Id: "codex", ProviderType: "codex"},
		chunks: []*schema.Message{
			{
				Role: schema.Assistant,
				ToolCalls: []schema.ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: schema.FunctionCall{
						Name:      "lookup",
						Arguments: `{"q":"hello"}`,
					},
				}},
				ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_use"},
			},
			{Role: schema.Assistant, Content: "done"},
		},
	}

	body, err := json.Marshal(MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Stream:    true,
		Messages: []MessageItem{{
			Role:    "user",
			Content: MessageContent{{Type: "text", Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}

	payload, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyText := string(payload)
	if !strings.Contains(bodyText, `event: content_block_start
data: {"content_block":{"text":"","type":"text"},"index":1`) ||
		!strings.Contains(bodyText, `event: content_block_delta
data: {"delta":{"text":"done","type":"text_delta"},"index":1`) ||
		!strings.Contains(bodyText, `event: content_block_stop
data: {"index":1`) {
		t.Fatalf("text block after tool must consistently use index 1: %s", bodyText)
	}
}

func TestServeLLMApiStreamAccumulatesFragmentedToolCall(t *testing.T) {
	handler := NewHandler(nil)
	idx0 := 0
	// OpenAI-compatible providers (e.g. DeepSeek) stream one tool call as many
	// fragments: id+name first, then argument deltas with only the index set.
	prov := &testStreamingProvider{
		cfg: provider.ProviderConfig{Id: "deepseek", ProviderType: "deepseek"},
		chunks: []*schema.Message{
			{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				Index: &idx0, ID: "call_1", Type: "function",
				Function: schema.FunctionCall{Name: "get_weather"},
			}}},
			{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				Index: &idx0, Function: schema.FunctionCall{Arguments: `{"ci`},
			}}},
			{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				Index: &idx0, Function: schema.FunctionCall{Arguments: `ty":"Paris"}`},
			}}, ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_use"}},
		},
	}

	body, err := json.Marshal(MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Stream:    true,
		Messages: []MessageItem{{
			Role:    "user",
			Content: MessageContent{{Type: "text", Text: "weather in Paris"}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()
	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}

	payload, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyText := string(payload)

	// Exactly one tool_use content block, never a fragment-per-block explosion.
	if n := strings.Count(bodyText, `"type":"tool_use"`); n != 1 {
		t.Fatalf("tool_use blocks = %d, want 1: %s", n, bodyText)
	}
	// The single block must carry the real id and name (not an empty fragment).
	if !strings.Contains(bodyText, `"id":"call_1","input":{},"name":"get_weather","type":"tool_use"`) {
		t.Fatalf("missing accumulated tool_use block: %s", bodyText)
	}
	if strings.Contains(bodyText, `"name":"","type":"tool_use"`) {
		t.Fatalf("phantom empty-name tool_use block emitted: %s", bodyText)
	}
	// Both argument fragments must stream into the same block and reconstruct.
	if !strings.Contains(bodyText, `"partial_json":"{\"ci"`) ||
		!strings.Contains(bodyText, `"partial_json":"ty\":\"Paris\"}"`) {
		t.Fatalf("argument fragments not preserved: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"stop_reason":"tool_use"`) {
		t.Fatalf("missing tool_use stop_reason: %s", bodyText)
	}
}

func TestToInternalCarriesThinkingMetadataAndOutputConfig(t *testing.T) {
	conv := &Converter{}
	req := &MessagesRequest{
		Model:        "claude-sonnet-4-5",
		MaxTokens:    4096,
		Messages:     []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "hi"}}}},
		Thinking:     json.RawMessage(`{"type":"enabled","budget_tokens":2048}`),
		Metadata:     json.RawMessage(`{"user_id":"end-user-1"}`),
		OutputConfig: json.RawMessage(`{"format":{"type":"json_schema","schema":{"type":"object"}}}`),
	}

	chatReq := conv.ToInternal(req)
	extra := provider.ChatExtraFieldsFromOptions(chatReq.Options...)
	if extra == nil {
		t.Fatal("ChatExtraFields = nil, want inbound thinking/metadata/output_config carried through")
	}
	if extra.Reasoning["type"] != "enabled" {
		t.Fatalf("reasoning type = %v, want enabled", extra.Reasoning["type"])
	}
	if budget, _ := extra.Reasoning["budget_tokens"].(int); budget != 2048 {
		t.Fatalf("reasoning budget_tokens = %v, want 2048", extra.Reasoning["budget_tokens"])
	}
	if extra.Metadata["user_id"] != "end-user-1" {
		t.Fatalf("metadata user_id = %v, want end-user-1", extra.Metadata["user_id"])
	}
	format, ok := extra.ResponseFormat.(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("response_format = %#v, want json_schema format from output_config", extra.ResponseFormat)
	}
}

func TestAnthropicConverterRoundTripsProviderReasoning(t *testing.T) {
	conv := &Converter{}
	chatReq := conv.ToInternal(&MessagesRequest{
		Model: "glm-5.2", MaxTokens: 4096,
		Messages: []MessageItem{{
			Role: "assistant",
			Content: MessageContent{
				{Type: "thinking", Thinking: "inspect first", Signature: "opaque"},
				{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"README.md"}`)},
			},
		}},
	})
	if len(chatReq.Messages) != 1 || chatReq.Messages[0].ReasoningContent != "inspect first" || len(chatReq.Messages[0].ToolCalls) != 1 {
		t.Fatalf("chat messages = %+v", chatReq.Messages)
	}

	resp := conv.FromInternal(&provider.ChatResponse{Message: &schema.Message{
		Role: schema.Assistant, Content: "done", ReasoningContent: "inspect first",
	}}, "glm-5.2")
	if len(resp.Content) != 2 || resp.Content[0].Type != "thinking" || resp.Content[0].Thinking != "inspect first" || resp.Content[0].Signature == "" {
		t.Fatalf("response content = %+v", resp.Content)
	}
	if !strings.HasPrefix(resp.Content[0].Signature, "agw-thinking-") {
		t.Fatalf("thinking signature = %q, want provider-neutral gateway prefix", resp.Content[0].Signature)
	}
	if resp.Content[1].Type != "text" || resp.Content[1].Text != "done" {
		t.Fatalf("response text = %+v", resp.Content[1])
	}
}

func TestServeLLMApiStreamsProviderReasoningAsThinkingBlock(t *testing.T) {
	handler := NewHandler(nil)
	prov := &testStreamingProvider{
		cfg: provider.ProviderConfig{Id: "zhipu", ProviderType: "zhipu"},
		chunks: []*schema.Message{
			{Role: schema.Assistant, ReasoningContent: "inspect "},
			{Role: schema.Assistant, ReasoningContent: "first"},
			{Role: schema.Assistant, Content: "done", ResponseMeta: &schema.ResponseMeta{FinishReason: "stop"}},
		},
	}
	body, err := json.Marshal(MessagesRequest{
		Model: "glm-5.2", MaxTokens: 4096, Stream: true,
		Messages: []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "inspect"}}}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()
	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	bodyText := rec.Body.String()
	for _, want := range []string{
		`"content_block":{"signature":"","thinking":"","type":"thinking"}`,
		`"delta":{"thinking":"inspect ","type":"thinking_delta"}`,
		`"delta":{"thinking":"first","type":"thinking_delta"}`,
		`"type":"signature_delta"`,
		`"delta":{"text":"done","type":"text_delta"}`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("stream missing %q: %s", want, bodyText)
		}
	}
}

func TestServeLLMApiDropsReasoningThatArrivesAfterText(t *testing.T) {
	handler := NewHandler(nil)
	prov := &testStreamingProvider{
		cfg: provider.ProviderConfig{Id: "chat-provider", ProviderType: "chat-provider"},
		chunks: []*schema.Message{
			{Role: schema.Assistant, ReasoningContent: "before"},
			{Role: schema.Assistant, Content: "answer"},
			{Role: schema.Assistant, ReasoningContent: "late-secret"},
		},
	}
	body, err := json.Marshal(MessagesRequest{
		Model: "reasoning-model", MaxTokens: 4096, Stream: true,
		Messages: []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "inspect"}}}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()
	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	bodyText := rec.Body.String()
	if !strings.Contains(bodyText, `"thinking":"before"`) || !strings.Contains(bodyText, `"text":"answer"`) {
		t.Fatalf("expected ordered reasoning and text blocks: %s", bodyText)
	}
	if strings.Contains(bodyText, "late-secret") {
		t.Fatalf("late reasoning must not be emitted after text: %s", bodyText)
	}
}

func TestToInternalCarriesDisableParallelToolUse(t *testing.T) {
	conv := &Converter{}
	req := &MessagesRequest{
		Model:      "claude-sonnet-4-5",
		MaxTokens:  4096,
		Messages:   []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "hi"}}}},
		ToolChoice: json.RawMessage(`{"type":"auto","disable_parallel_tool_use":true}`),
	}

	chatReq := conv.ToInternal(req)
	extra := provider.ChatExtraFieldsFromOptions(chatReq.Options...)
	if extra == nil || extra.ParallelToolCalls == nil || *extra.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %+v, want false", extra)
	}
}
