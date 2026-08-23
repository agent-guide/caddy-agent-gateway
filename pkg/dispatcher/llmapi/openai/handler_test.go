package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/internal/statuserr"
	"github.com/agent-guide/agent-gateway/pkg/credential"
	sched "github.com/agent-guide/agent-gateway/pkg/credential/scheduler"
	dispatcher "github.com/agent-guide/agent-gateway/pkg/dispatcher"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type testProvider struct {
	chatResp    *provider.ChatResponse
	generateErr error
	streamResp  *schema.StreamReader[*schema.Message]
	streamErr   error
	models      []provider.ModelInfo
	cfg         provider.ProviderConfig

	lastChatReq   *provider.ChatRequest
	lastStreamReq *provider.ChatRequest
}

func (p *testProvider) Chat(_ context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	p.lastChatReq = req
	return p.chatResp, p.generateErr
}

func (p *testProvider) StreamChat(_ context.Context, req *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	p.lastStreamReq = req
	return p.streamResp, p.streamErr
}

func (p *testProvider) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return p.models, nil
}

func (p *testProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{Streaming: true}
}

func (p *testProvider) Config() provider.ProviderConfig {
	if p.cfg.ProviderType == "" {
		p.cfg.ProviderType = "openai"
	}
	return p.cfg
}

type testResponsesProvider struct {
	*testProvider
	responseResp      *provider.ResponsesResponse
	responseErr       error
	responseStream    *schema.StreamReader[*provider.ResponsesStreamEvent]
	responseStreamErr error
	lastResponseReq   *provider.ResponsesRequest
}

func (p *testResponsesProvider) CreateResponses(_ context.Context, req *provider.ResponsesRequest) (*provider.ResponsesResponse, error) {
	p.lastResponseReq = req
	return p.responseResp, p.responseErr
}

func (p *testResponsesProvider) StreamResponses(_ context.Context, req *provider.ResponsesRequest) (*schema.StreamReader[*provider.ResponsesStreamEvent], error) {
	p.lastResponseReq = req
	return p.responseStream, p.responseStreamErr
}

type testCompatResponsesProvider struct {
	*testProvider
	lastResponseReq *provider.ResponsesRequest
}

func (p *testCompatResponsesProvider) CreateResponses(ctx context.Context, req *provider.ResponsesRequest) (*provider.ResponsesResponse, error) {
	p.lastResponseReq = req
	return provider.CreateResponsesViaChat(ctx, p.testProvider, req)
}

func (p *testCompatResponsesProvider) StreamResponses(ctx context.Context, req *provider.ResponsesRequest) (*schema.StreamReader[*provider.ResponsesStreamEvent], error) {
	p.lastResponseReq = req
	return provider.StreamResponsesViaChat(ctx, p.testProvider, req)
}

type testStatusError struct {
	msg    string
	status int
}

func (e testStatusError) Error() string   { return e.msg }
func (e testStatusError) StatusCode() int { return e.status }

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (r *flushRecorder) Flush() {
	r.flushes++
	r.ResponseRecorder.Flush()
}

type wrappedResponseWriter struct {
	inner *flushRecorder
}

func (w *wrappedResponseWriter) Header() http.Header {
	return w.inner.Header()
}

func (w *wrappedResponseWriter) Write(p []byte) (int, error) {
	return w.inner.Write(p)
}

func (w *wrappedResponseWriter) WriteHeader(status int) {
	w.inner.WriteHeader(status)
}

func (w *wrappedResponseWriter) Unwrap() http.ResponseWriter {
	return w.inner
}

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

func newHandler() *Handler {
	return NewHandler()
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

func TestMatchLLMApiRequiresVersionedOpenAIPath(t *testing.T) {
	handler := newHandler()
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/models", "/v1/embeddings"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if !handler.MatchLLMApi(req) {
			t.Fatalf("MatchLLMApi(%q) = false, want true", path)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/tenant/chat/completions", nil)
	if handler.MatchLLMApi(req) {
		t.Fatal("MatchLLMApi matched unversioned suffix path")
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/models/extra", nil)
	if handler.MatchLLMApi(req) {
		t.Fatal("MatchLLMApi matched non-endpoint path")
	}
}

func TestServeLLMApiListsModels(t *testing.T) {
	prov := &testProvider{
		models: []provider.ModelInfo{{
			ID:          "claude-sonnet-4-6",
			DisplayName: "Claude Sonnet 4.6",
		}},
		cfg: provider.ProviderConfig{ProviderType: "claudecode"},
	}
	handler := newHandler()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"object":"list"`, `"id":"claude-sonnet-4-6"`, `"owned_by":"claudecode"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in models response, got %q", want, body)
		}
	}
}

func TestServeLLMApiMarksOpenAIStreamFailures(t *testing.T) {
	credMgr := credential.NewManager(nil)
	if err := credMgr.RegisterCredential(context.Background(), &credential.Credential{
		ID:           "cred-openai-1",
		ProviderType: "openai",
		ProviderID:   "openai",
		Scope:        "id:openai",
		Type:         credential.TypeAPIKey,
	}); err != nil {
		t.Fatalf("register credential: %v", err)
	}

	baseProv := &testProvider{
		streamErr: testStatusError{msg: "rate limit", status: http.StatusTooManyRequests},
		cfg: provider.ProviderConfig{
			Id:           "openai",
			ProviderType: "openai",
		},
	}
	scheduler := newTestCredentialScheduler(t, credMgr)
	prov := &testCredentialMarkingProvider{base: baseProv, scheduler: scheduler, credID: "cred-openai-1"}
	handler := newHandler()

	body, err := json.Marshal(ChatCompletionRequest{
		Model:  "gpt-4o-mini",
		Stream: true,
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("unexpected status code: got %d want %d", rec.Code, http.StatusTooManyRequests)
	}

	_, err = scheduler.Pick(context.Background(), sched.Filter{
		Type:            credential.TypeAPIKey,
		CredentialScope: "id:openai",
		Model:           "gpt-4o-mini",
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

func TestServeLLMApiLogsUpstreamStatusAndBody(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	handler := newHandler()
	handler.SetLogger(zap.New(core))
	prov := &testProvider{
		generateErr: &provider.UpstreamError{
			Status:     http.StatusUnauthorized,
			StatusText: "401 Unauthorized",
			Body:       `{"error":"bad token"}`,
		},
	}

	body, err := json.Marshal(ChatCompletionRequest{
		Model: "gpt-4o-mini",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected status code: got %d want %d", rec.Code, http.StatusUnauthorized)
	}
	if logs.Len() != 1 {
		t.Fatalf("log entries = %d, want 1", logs.Len())
	}

	fields := logs.All()[0].ContextMap()
	if got := fields["upstream_status"]; got != int64(http.StatusUnauthorized) {
		t.Fatalf("upstream_status = %#v, want %d", got, http.StatusUnauthorized)
	}
	if got := fields["upstream_error_body"]; got != `{"error":"bad token"}` {
		t.Fatalf("upstream_error_body = %#v, want %q", got, `{"error":"bad token"}`)
	}
}

func TestServeLLMApiLogsOpenAIClientStatusAndMessage(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	handler := newHandler()
	handler.SetLogger(zap.New(core))
	prov := &testProvider{
		generateErr: &provider.UpstreamError{
			Status:     http.StatusInternalServerError,
			StatusText: "500 Internal Server Error",
			Body:       "Internal server error",
		},
	}

	body, err := json.Marshal(ChatCompletionRequest{
		Model: "gpt-5.4",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unexpected status code: got %d want %d", rec.Code, http.StatusInternalServerError)
	}
	if logs.Len() != 1 {
		t.Fatalf("log entries = %d, want 1", logs.Len())
	}

	fields := logs.All()[0].ContextMap()
	if got := fields["upstream_status"]; got != int64(http.StatusInternalServerError) {
		t.Fatalf("upstream_status = %#v, want %d", got, http.StatusInternalServerError)
	}
	if got := fields["upstream_status_text"]; got != "500 Internal Server Error" {
		t.Fatalf("upstream_status_text = %#v, want %q", got, "500 Internal Server Error")
	}
	if got := fields["upstream_error_body"]; got != "Internal server error" {
		t.Fatalf("upstream_error_body = %#v, want %q", got, "Internal server error")
	}
}

func TestServeLLMApiMapsClientCanceledStreamTo499(t *testing.T) {
	prov := &testProvider{
		streamErr: statuserr.New(http.StatusBadGateway, "Post \"https://api.openai.com/v1/chat/completions\": context canceled"),
	}
	handler := newHandler()

	body, err := json.Marshal(ChatCompletionRequest{
		Model:  "gpt-4o-mini",
		Stream: true,
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if rec.Code != dispatcher.StatusClientClosedRequest {
		t.Fatalf("unexpected status code: got %d want %d", rec.Code, dispatcher.StatusClientClosedRequest)
	}
	if !strings.Contains(rec.Body.String(), "client canceled request") {
		t.Fatalf("unexpected error body: %q", rec.Body.String())
	}
}

func TestServeLLMApiReturnsChatCompletionResponse(t *testing.T) {
	prov := &testProvider{
		chatResp: &provider.ChatResponse{
			Message: &schema.Message{
				Role:             schema.RoleType("assistant"),
				Content:          "hello back",
				ReasoningContent: "reasoning back",
				ResponseMeta: &schema.ResponseMeta{
					FinishReason: "stop",
					Usage: &schema.TokenUsage{
						PromptTokens:     3,
						CompletionTokens: 5,
						TotalTokens:      13,
					},
				},
			},
		},
	}
	handler := newHandler()

	body, err := json.Marshal(ChatCompletionRequest{
		Model: "gpt-4o-mini",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
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
	if prov.lastChatReq == nil || prov.lastChatReq.Model != "gpt-4o-mini" {
		t.Fatalf("unexpected chat request: %+v", prov.lastChatReq)
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Object != "chat.completion" || resp.Model != "gpt-4o-mini" {
		t.Fatalf("unexpected response envelope: %+v", resp)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello back" {
		t.Fatalf("unexpected choices: %+v", resp.Choices)
	}
	if resp.Choices[0].Message.ReasoningContent != "reasoning back" {
		t.Fatalf("reasoning_content = %q", resp.Choices[0].Message.ReasoningContent)
	}
	if resp.Usage.PromptTokens != 3 || resp.Usage.CompletionTokens != 5 || resp.Usage.TotalTokens != 13 {
		t.Fatalf("usage = %+v, want upstream total 13 preserved", resp.Usage)
	}
}

func TestPrepareLLMApiRequestPreservesChatCompletionToolingAndMultimodal(t *testing.T) {
	handler := newHandler()
	body := []byte(`{
		"model":"gpt-4o-mini",
		"messages":[
			{"role":"system","content":"follow policy"},
			{"role":"user","content":[
				{"type":"text","text":"what is in this image?"},
				{"type":"image_url","image_url":{"url":"https://example.com/cat.png","detail":"high"}}
			]},
			{"role":"assistant","content":"calling tool","reasoning_content":"need lookup","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"cat\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"tool result"}
		],
		"tools":[
			{"type":"function","function":{
				"name":"lookup",
				"description":"Look up a fact",
				"parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}
			}}
		],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"max_tokens":128,
		"temperature":0.2,
		"top_p":0.9,
		"stop":["END"]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	prepared, requirements, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	if prepared == nil || prepared.ChatRequest == nil {
		t.Fatal("prepared chat request is nil")
	}
	if !requirements.RequireTools {
		t.Fatal("chat completion tools did not set RequireTools")
	}
	chatReq := prepared.ChatRequest
	if len(chatReq.Messages) != 4 {
		t.Fatalf("message count = %d, want 4", len(chatReq.Messages))
	}
	if chatReq.Messages[1].Role != schema.User || len(chatReq.Messages[1].UserInputMultiContent) != 2 {
		t.Fatalf("user message = %+v, want multimodal content", chatReq.Messages[1])
	}
	if got := chatReq.Messages[1].UserInputMultiContent[1].Image.URL; got == nil || *got != "https://example.com/cat.png" {
		t.Fatalf("user image url = %#v, want https://example.com/cat.png", got)
	}
	if chatReq.Messages[2].Role != schema.Assistant || len(chatReq.Messages[2].ToolCalls) != 1 {
		t.Fatalf("assistant message = %+v, want tool call history", chatReq.Messages[2])
	}
	if chatReq.Messages[2].ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("assistant tool call = %+v, want lookup", chatReq.Messages[2].ToolCalls[0])
	}
	if chatReq.Messages[2].ReasoningContent != "need lookup" {
		t.Fatalf("assistant reasoning_content = %q", chatReq.Messages[2].ReasoningContent)
	}
	if chatReq.Messages[3].Role != schema.Tool || chatReq.Messages[3].ToolCallID != "call_1" || chatReq.Messages[3].Content != "tool result" {
		t.Fatalf("tool message = %+v, want tool_call_id + content", chatReq.Messages[3])
	}

	opts := einomodel.GetCommonOptions(nil, chatReq.Options...)
	if opts.MaxTokens == nil || *opts.MaxTokens != 128 {
		t.Fatalf("max_tokens = %#v, want 128", opts.MaxTokens)
	}
	if opts.Temperature == nil || *opts.Temperature != 0.2 {
		t.Fatalf("temperature = %#v, want 0.2", opts.Temperature)
	}
	if opts.TopP == nil || *opts.TopP != 0.9 {
		t.Fatalf("top_p = %#v, want 0.9", opts.TopP)
	}
	if len(opts.Stop) != 1 || opts.Stop[0] != "END" {
		t.Fatalf("stop = %#v, want END", opts.Stop)
	}
	if len(opts.Tools) != 1 || opts.Tools[0].Name != "lookup" {
		t.Fatalf("tools = %+v, want lookup", opts.Tools)
	}
	if opts.ToolChoice == nil || *opts.ToolChoice != schema.ToolChoiceForced {
		t.Fatalf("tool_choice = %#v, want forced", opts.ToolChoice)
	}
	if len(opts.AllowedToolNames) != 1 || opts.AllowedToolNames[0] != "lookup" {
		t.Fatalf("allowed tool names = %#v, want [lookup]", opts.AllowedToolNames)
	}
}

func TestPrepareLLMApiRequestPreservesChatCompletionResponseFormatAndReasoning(t *testing.T) {
	handler := newHandler()
	body := []byte(`{
		"model":"gpt-4o-mini",
		"messages":[{"role":"user","content":"hi"}],
		"user":"user-1",
		"metadata":{"trace_id":"abc123"},
		"parallel_tool_calls":false,
		"store":true,
		"reasoning":{"effort":"medium"},
		"thinking":{"type":"enabled"},
		"reasoning_effort":"high",
		"tool_stream":true,
		"stream_options":{"include_usage":true},
		"response_format":{"type":"json_schema","json_schema":{"name":"weather","schema":{"type":"object"},"strict":true}}
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	if prepared == nil || prepared.ChatRequest == nil {
		t.Fatal("prepared chat request is nil")
	}

	fields := provider.ChatCompletionsExtraFieldsFromOptions(provider.ZhipuChatCompletionsFields, prepared.ChatRequest.Options...)
	if fields["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", fields["reasoning_effort"])
	}
	thinking, _ := fields["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || fields["tool_stream"] != true {
		t.Fatalf("thinking/tool_stream = %#v/%#v", fields["thinking"], fields["tool_stream"])
	}
	streamOptions, _ := fields["stream_options"].(map[string]any)
	if streamOptions["include_usage"] != true {
		t.Fatalf("stream_options = %#v", fields["stream_options"])
	}
	if fields["user"] != "user-1" {
		t.Fatalf("user = %#v, want user-1", fields["user"])
	}
	metadata, _ := fields["metadata"].(map[string]any)
	if metadata["trace_id"] != "abc123" {
		t.Fatalf("metadata = %#v, want trace_id", fields["metadata"])
	}
	if fields["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v, want false", fields["parallel_tool_calls"])
	}
	if fields["store"] != true {
		t.Fatalf("store = %#v, want true", fields["store"])
	}
	format, ok := fields["response_format"].(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("response_format = %#v, want chat-shaped json_schema passthrough", fields["response_format"])
	}
	jsonSchema, ok := format["json_schema"].(map[string]any)
	if !ok || jsonSchema["name"] != "weather" {
		t.Fatalf("response_format json_schema = %#v, want nested name", format["json_schema"])
	}
}

func TestServeLLMApiReturnsChatCompletionToolCalls(t *testing.T) {
	prov := &testProvider{
		chatResp: &provider.ChatResponse{
			Message: &schema.Message{
				Role:    schema.Assistant,
				Content: "I will call a tool",
				ToolCalls: []schema.ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: schema.FunctionCall{
						Name:      "lookup",
						Arguments: `{"q":"cat"}`,
					},
				}},
				ResponseMeta: &schema.ResponseMeta{
					FinishReason: "tool_calls",
				},
			},
		},
	}
	handler := newHandler()

	body, err := json.Marshal(ChatCompletionRequest{
		Model: "gpt-4o-mini",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
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

	var resp ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("unexpected tool call response: %+v", resp.Choices)
	}
	if resp.Choices[0].Message.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("tool call function = %+v, want lookup", resp.Choices[0].Message.ToolCalls[0])
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
}

func TestServeLLMApiStreamsOpenAIChunks(t *testing.T) {
	prov := &testProvider{
		streamResp: schema.StreamReaderFromArray([]*schema.Message{{
			Role:             schema.RoleType("assistant"),
			Content:          "hello stream",
			ReasoningContent: "stream reasoning",
			ResponseMeta: &schema.ResponseMeta{
				FinishReason: "stop",
			},
		}}),
	}
	handler := newHandler()

	body, err := json.Marshal(ChatCompletionRequest{
		Model:  "gpt-4o-mini",
		Stream: true,
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	inner := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	rec := &wrappedResponseWriter{inner: inner}
	if _, ok := any(rec).(http.Flusher); ok {
		t.Fatal("wrapped response writer must not directly implement http.Flusher")
	}

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if inner.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d", inner.Code, http.StatusOK)
	}
	if prov.lastStreamReq == nil {
		t.Fatal("expected provider Stream to be called")
	}
	if inner.flushes == 0 {
		t.Fatal("expected stream to flush through wrapped response writer Unwrap chain")
	}

	bodyText := inner.Body.String()
	if !strings.Contains(bodyText, "data: [DONE]") {
		t.Fatalf("expected done marker in stream body, got %q", bodyText)
	}

	firstLine := firstDataLine(bodyText)
	var chunk chatCompletionChunk
	if err := json.Unmarshal([]byte(firstLine), &chunk); err != nil {
		t.Fatalf("unmarshal stream chunk: %v", err)
	}
	if chunk.Model != "gpt-4o-mini" || len(chunk.Choices) != 1 || chunk.Choices[0].Delta.Content != "hello stream" {
		t.Fatalf("unexpected chunk: %+v", chunk)
	}
	if chunk.Choices[0].Delta.ReasoningContent != "stream reasoning" {
		t.Fatalf("stream reasoning_content = %q", chunk.Choices[0].Delta.ReasoningContent)
	}
}

func TestPrepareLLMApiRequestAllowsResponsesStateForProviderLevelHandling(t *testing.T) {
	handler := newHandler()

	body := `{"model":"gpt-4.1","input":"hello","previous_response_id":"resp_123","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))

	prepared, requirements, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prepared == nil || prepared.Type != provider.LLMApiRequestTypeResponses || prepared.ChatRequest != nil || prepared.ResponsesRequest == nil || prepared.ResponsesRequest.Model != "gpt-4.1" {
		t.Fatalf("unexpected prepared request: %+v", prepared)
	}
	if prepared.Model() != "gpt-4.1" {
		t.Fatalf("prepared.Model() = %q, want gpt-4.1", prepared.Model())
	}
	if prepared.Stream() {
		t.Fatal("prepared.Stream() = true, want false")
	}
	if requirements.Model != "gpt-4.1" {
		t.Fatalf("requirements.Model = %q, want gpt-4.1", requirements.Model)
	}
	if requirements.RequireStreaming {
		t.Fatal("requirements.RequireStreaming = true, want false")
	}
	if !requirements.RequireTools {
		t.Fatal("responses tools did not set RequireTools")
	}
}

func TestServeLLMApiAllowsProviderResponsesRequestsThatFallbackCannotExpress(t *testing.T) {
	prov := &testResponsesProvider{
		testProvider: &testProvider{},
		responseResp: &provider.ResponsesResponse{
			ID:        "resp_provider",
			Object:    "response",
			CreatedAt: 1,
			Model:     "gpt-4.1",
			Output: []provider.ResponsesResponseOutput{{
				ID:     "msg_provider",
				Type:   "message",
				Role:   "assistant",
				Status: "completed",
				Content: []provider.ResponsesResponseContentPart{{
					Type:        "output_text",
					Text:        "provider path",
					Annotations: []any{},
				}},
			}},
		},
	}
	handler := newHandler()

	body := `{"model":"gpt-4.1","input":"hello","previous_response_id":"resp_123"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
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
	if prov.lastResponseReq == nil || prov.lastResponseReq.PreviousResponseID != "resp_123" {
		t.Fatalf("unexpected provider responses request: %+v", prov.lastResponseReq)
	}
	if prov.lastChatReq != nil {
		t.Fatalf("expected no chat fallback, got %+v", prov.lastChatReq)
	}
}

func TestServeResponsesRecordsExactUpstreamTotalTokens(t *testing.T) {
	prov := &testResponsesProvider{
		testProvider: &testProvider{},
		responseResp: &provider.ResponsesResponse{
			ID:        "resp_provider",
			Object:    "response",
			CreatedAt: 1,
			Model:     "gpt-4.1",
			Output:    []provider.ResponsesResponseOutput{},
			Usage: &provider.ResponsesResponseUsage{
				InputTokens:  3,
				OutputTokens: 4,
				TotalTokens:  11,
			},
		},
	}
	handler := newHandler()
	sink := &usage.InMemorySink{}
	observer := usage.NewObserver(sink)
	span, ctx := observer.Begin(context.Background(), usage.InteractionDimensions{RouteKind: "llm", RouteID: "responses-route"})

	body := `{"model":"gpt-4.1","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)).WithContext(ctx)
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	span.Finish(usage.InteractionOutcome{Success: true, StatusCode: rec.Code})

	if len(sink.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.Events))
	}
	ev, ok := sink.Events[0].(usage.LLMUsageEvent)
	if !ok {
		t.Fatalf("event type = %T, want LLMUsageEvent", sink.Events[0])
	}
	if ev.InputTokens != 3 || ev.OutputTokens != 4 || ev.TotalTokens != 11 || !ev.UsageFinalized {
		t.Fatalf("usage = input:%d output:%d total:%d finalized:%v, want 3/4/11/finalized", ev.InputTokens, ev.OutputTokens, ev.TotalTokens, ev.UsageFinalized)
	}
}

func TestServeLLMApiPreservesResponsesFieldsForProviderLevelHandling(t *testing.T) {
	prov := &testResponsesProvider{
		testProvider: &testProvider{},
		responseResp: &provider.ResponsesResponse{
			ID:        "resp_provider",
			Object:    "response",
			CreatedAt: 1,
			Model:     "gpt-4.1",
			Output:    []provider.ResponsesResponseOutput{},
		},
	}
	handler := newHandler()

	body := `{
		"model":"gpt-4.1",
		"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}],
		"tools":[{"type":"function","function":{"name":"lookup","description":"Look up facts","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],
		"tool_choice":{"type":"function","name":"lookup"},
		"metadata":{"trace_id":"abc123"},
		"user":"user-1",
		"reasoning":{"effort":"medium"},
		"parallel_tool_calls":true
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
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
	if prov.lastResponseReq == nil {
		t.Fatal("expected provider-level responses request")
	}
	if len(prov.lastResponseReq.Tools) != 1 {
		t.Fatalf("tools = %+v, want one tool", prov.lastResponseReq.Tools)
	}
	if prov.lastResponseReq.Tools[0].Function == nil || prov.lastResponseReq.Tools[0].Function.Name != "lookup" {
		t.Fatalf("tool = %+v, want function lookup", prov.lastResponseReq.Tools[0])
	}
	if string(prov.lastResponseReq.ToolChoice) != `{"type":"function","name":"lookup"}` {
		t.Fatalf("tool_choice = %s, want function lookup", string(prov.lastResponseReq.ToolChoice))
	}
	if prov.lastResponseReq.Metadata["trace_id"] != "abc123" {
		t.Fatalf("metadata = %+v, want trace_id", prov.lastResponseReq.Metadata)
	}
	if prov.lastResponseReq.User != "user-1" {
		t.Fatalf("user = %q, want user-1", prov.lastResponseReq.User)
	}
	if prov.lastResponseReq.Reasoning["effort"] != "medium" {
		t.Fatalf("reasoning = %+v, want effort=medium", prov.lastResponseReq.Reasoning)
	}
	if prov.lastResponseReq.ParallelToolCalls == nil || !*prov.lastResponseReq.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %#v, want true", prov.lastResponseReq.ParallelToolCalls)
	}
}

func TestServeLLMApiReturnsResponsesResponse(t *testing.T) {
	prov := &testCompatResponsesProvider{testProvider: &testProvider{
		chatResp: &provider.ChatResponse{
			Message: &schema.Message{
				Role:    schema.RoleType("assistant"),
				Content: "hello response",
				ResponseMeta: &schema.ResponseMeta{
					FinishReason: "stop",
					Usage: &schema.TokenUsage{
						PromptTokens:     4,
						CompletionTokens: 6,
						TotalTokens:      10,
					},
				},
			},
		},
	}}
	handler := newHandler()

	body := `{"model":"gpt-4.1","input":"hello"}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
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
	if prov.lastResponseReq == nil || prov.lastResponseReq.Model != "gpt-4.1" {
		t.Fatalf("unexpected responses request: %+v", prov.lastResponseReq)
	}
	if prov.lastChatReq == nil || prov.lastChatReq.Model != "gpt-4.1" {
		t.Fatalf("unexpected chat fallback request: %+v", prov.lastChatReq)
	}
	if len(prov.lastChatReq.Messages) != 1 || prov.lastChatReq.Messages[0].Content != "hello" {
		t.Fatalf("unexpected internal messages: %+v", prov.lastChatReq.Messages)
	}

	var resp provider.ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Object != "response" || resp.Model != "gpt-4.1" {
		t.Fatalf("unexpected response envelope: %+v", resp)
	}
	if len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Text != "hello response" {
		t.Fatalf("unexpected output: %+v", resp.Output)
	}
}

func TestServeLLMApiUsesProviderResponsesCompatibilityForToolsAndImages(t *testing.T) {
	prov := &testCompatResponsesProvider{testProvider: &testProvider{
		chatResp: &provider.ChatResponse{
			Message: &schema.Message{
				Role:    schema.Assistant,
				Content: "handled compat",
			},
		},
	}}
	handler := newHandler()

	body := `{
		"model":"gpt-4.1",
		"input":[{"role":"user","content":[
			{"type":"input_text","text":"hello"},
			{"type":"input_image","image_url":{"url":"https://example.com/cat.png","detail":"high"}}
		]}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],
		"tool_choice":"required"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
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
	if prov.lastChatReq == nil || len(prov.lastChatReq.Messages) != 1 {
		t.Fatalf("chat fallback request = %+v, want one message", prov.lastChatReq)
	}
	msg := prov.lastChatReq.Messages[0]
	if len(msg.UserInputMultiContent) != 2 {
		t.Fatalf("user multimodal content = %+v, want text+image", msg.UserInputMultiContent)
	}
	opts := einomodel.GetCommonOptions(nil, prov.lastChatReq.Options...)
	if len(opts.Tools) != 1 || opts.Tools[0].Name != "lookup" {
		t.Fatalf("tools = %+v, want lookup", opts.Tools)
	}
	if opts.ToolChoice == nil || *opts.ToolChoice != schema.ToolChoiceForced {
		t.Fatalf("tool choice = %#v, want forced", opts.ToolChoice)
	}
	respCtx := provider.ResponsesRequestContextFromOptions(prov.lastChatReq.Options...)
	if respCtx != nil {
		t.Fatalf("responses context = %+v, want nil for request without extra fields", respCtx)
	}
}

func TestServeLLMApiUsesProviderResponsesCompatibilityPreservesResponsesContext(t *testing.T) {
	prov := &testCompatResponsesProvider{testProvider: &testProvider{
		chatResp: &provider.ChatResponse{
			Message: &schema.Message{Role: schema.Assistant, Content: "ok"},
		},
	}}
	handler := newHandler()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-4.1",
		"input":"hello",
		"previous_response_id":"resp_prev",
		"store":true,
		"metadata":{"trace_id":"abc123"},
		"user":"user-1",
		"reasoning":{"effort":"medium"},
		"text":{"verbosity":"high"},
		"parallel_tool_calls":true,
		"truncation":"auto"
	}`))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if prov.lastChatReq == nil {
		t.Fatal("expected compatibility path to call chat provider")
	}
	respCtx := provider.ResponsesRequestContextFromOptions(prov.lastChatReq.Options...)
	if respCtx == nil || respCtx.User != "user-1" {
		t.Fatalf("responses context = %+v, want user-1", respCtx)
	}
	if respCtx.PreviousResponseID != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev", respCtx.PreviousResponseID)
	}
	if respCtx.Store == nil || !*respCtx.Store {
		t.Fatalf("store = %#v, want true", respCtx.Store)
	}
	if respCtx.Metadata["trace_id"] != "abc123" || respCtx.Reasoning["effort"] != "medium" {
		t.Fatalf("responses context = %+v, want metadata/reasoning preserved", respCtx)
	}
	if respCtx.Text["verbosity"] != "high" || respCtx.Truncation != "auto" {
		t.Fatalf("responses context = %+v, want text/truncation preserved", respCtx)
	}
	if respCtx.ParallelToolCalls == nil || !*respCtx.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %#v, want true", respCtx.ParallelToolCalls)
	}
}

func TestServeLLMApiResponsesCompatibilityPreservesFunctionCallOutput(t *testing.T) {
	prov := &testCompatResponsesProvider{testProvider: &testProvider{
		chatResp: &provider.ChatResponse{
			Message: &schema.Message{
				Role:    schema.Assistant,
				Content: "calling tool",
				ToolCalls: []schema.ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: schema.FunctionCall{
						Name:      "lookup",
						Arguments: `{"q":"cat"}`,
					},
				}},
			},
		},
	}}
	handler := newHandler()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1","input":"hello"}`))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}

	var resp provider.ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output count = %d, want 2", len(resp.Output))
	}
	if resp.Output[1].Type != "function_call" || resp.Output[1].Name != "lookup" {
		t.Fatalf("function call output = %+v, want lookup", resp.Output[1])
	}
}

func TestServeLLMApiResponsesCompatibilityStreamsFunctionCallEvents(t *testing.T) {
	prov := &testCompatResponsesProvider{testProvider: &testProvider{
		streamResp: schema.StreamReaderFromArray([]*schema.Message{{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name:      "lookup",
					Arguments: `{"q":"cat"}`,
				},
			}},
			ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_calls"},
		}}),
	}}
	handler := newHandler()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1","input":"hello","stream":true}`))
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
		"event: response.created",
		"event: response.output_item.added",
		"event: response.function_call_arguments.delta",
		"event: response.output_item.done",
		"event: response.completed",
		`"name":"lookup"`,
		`"delta":"{\"q\":\"cat\"}"`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("expected %q in stream body, got %q", want, bodyText)
		}
	}
}

func TestServeLLMApiResponsesStreamPreservesRawProviderPayload(t *testing.T) {
	prov := &testResponsesProvider{
		testProvider: &testProvider{},
		responseStream: schema.StreamReaderFromArray([]*provider.ResponsesStreamEvent{{
			Type:    "response.output_text.delta",
			RawJSON: json.RawMessage(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello","logprobs":{"tokens":[1]}}`),
		}}),
	}
	handler := newHandler()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1","input":"hello","stream":true}`))
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
		"event: response.output_text.delta",
		`"delta":"hello"`,
		`"logprobs":{"tokens":[1]}`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("expected %q in stream body, got %q", want, bodyText)
		}
	}
}

func TestServeLLMApiResponsesPreservesRawProviderPayload(t *testing.T) {
	prov := &testResponsesProvider{
		testProvider: &testProvider{},
		responseResp: &provider.ResponsesResponse{
			ID:      "resp_1",
			Object:  "response",
			Model:   "gpt-4.1",
			RawJSON: json.RawMessage(`{"id":"resp_1","object":"response","created_at":1,"model":"gpt-4.1","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"no","severity":"high"}]}]}`),
		},
	}
	handler := newHandler()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1","input":"hello"}`))
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
		`"type":"refusal"`,
		`"severity":"high"`,
		`"refusal":"no"`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("expected %q in response body, got %q", want, bodyText)
		}
	}
}

func TestServeLLMApiStreamsResponsesEvents(t *testing.T) {
	prov := &testCompatResponsesProvider{testProvider: &testProvider{
		streamResp: schema.StreamReaderFromArray([]*schema.Message{{
			Role:    schema.RoleType("assistant"),
			Content: "hello response stream",
		}}),
	}}
	handler := newHandler()

	body := `{"model":"gpt-4.1","input":"hello","stream":true}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
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
	if prov.lastResponseReq == nil || prov.lastResponseReq.Model != "gpt-4.1" {
		t.Fatalf("unexpected responses request: %+v", prov.lastResponseReq)
	}
	if prov.lastStreamReq == nil || prov.lastStreamReq.Model != "gpt-4.1" {
		t.Fatalf("unexpected chat stream fallback request: %+v", prov.lastStreamReq)
	}

	bodyText := rec.Body.String()
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", "event: response.completed"} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("expected %q in stream body, got %q", want, bodyText)
		}
	}
}

func TestServeLLMApiUsesProviderResponsesCompatibilityForCreate(t *testing.T) {
	baseProv := &testCompatResponsesProvider{testProvider: &testProvider{
		chatResp: &provider.ChatResponse{
			Message: &schema.Message{
				Role:    schema.RoleType("assistant"),
				Content: "hello wrapped fallback",
			},
		},
		cfg: provider.ProviderConfig{
			Id:           "zhipu-main",
			ProviderType: "zhipu",
		},
	}}
	handler := newHandler()

	body := `{"model":"glm-4.7","input":"hello"}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, baseProv, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d", rec.Code, http.StatusOK)
	}
	if baseProv.lastResponseReq == nil || baseProv.lastResponseReq.Model != "glm-4.7" {
		t.Fatalf("expected provider-level responses request, got %+v", baseProv.lastResponseReq)
	}
	if baseProv.lastChatReq == nil || baseProv.lastChatReq.Model != "glm-4.7" {
		t.Fatalf("expected provider-level chat compatibility, got %+v", baseProv.lastChatReq)
	}
	if !strings.Contains(rec.Body.String(), "hello wrapped fallback") {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
}

func TestServeLLMApiUsesProviderResponsesCompatibilityForStream(t *testing.T) {
	baseProv := &testCompatResponsesProvider{testProvider: &testProvider{
		streamResp: schema.StreamReaderFromArray([]*schema.Message{{
			Role:    schema.RoleType("assistant"),
			Content: "hello wrapped stream fallback",
		}}),
		cfg: provider.ProviderConfig{
			Id:           "zhipu-main",
			ProviderType: "zhipu",
		},
	}}
	handler := newHandler()

	body := `{"model":"glm-4.7","input":"hello","stream":true}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, baseProv, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d", rec.Code, http.StatusOK)
	}
	if baseProv.lastResponseReq == nil || baseProv.lastResponseReq.Model != "glm-4.7" {
		t.Fatalf("expected provider-level responses request, got %+v", baseProv.lastResponseReq)
	}
	if baseProv.lastStreamReq == nil || baseProv.lastStreamReq.Model != "glm-4.7" {
		t.Fatalf("expected provider-level chat stream compatibility, got %+v", baseProv.lastStreamReq)
	}
	bodyText := rec.Body.String()
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", "hello wrapped stream fallback", "event: response.completed"} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("expected %q in stream body, got %q", want, bodyText)
		}
	}
}

func TestServeLLMApiPrefersProviderResponsesInterface(t *testing.T) {
	prov := &testResponsesProvider{
		testProvider: &testProvider{},
		responseResp: &provider.ResponsesResponse{
			ID:        "resp_provider",
			Object:    "response",
			CreatedAt: 1,
			Model:     "gpt-4.1",
			Output: []provider.ResponsesResponseOutput{{
				ID:     "msg_provider",
				Type:   "message",
				Role:   "assistant",
				Status: "completed",
				Content: []provider.ResponsesResponseContentPart{{
					Type:        "output_text",
					Text:        "provider path",
					Annotations: []any{},
				}},
			}},
		},
	}
	handler := newHandler()

	body := `{"model":"gpt-4.1","input":"hello"}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if prov.lastResponseReq == nil {
		t.Fatal("expected provider Responses interface to be used")
	}
	if prov.lastChatReq != nil {
		t.Fatalf("expected no chat fallback, got %+v", prov.lastChatReq)
	}
	if !strings.Contains(rec.Body.String(), "provider path") {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
}

func TestServeLLMApiPreservesResponsesStateInProviderCompatibility(t *testing.T) {
	prov := &testCompatResponsesProvider{testProvider: &testProvider{}}
	handler := newHandler()

	body := `{"model":"gpt-4.1","input":"hello","previous_response_id":"resp_123","store":false}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
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
	if prov.lastChatReq == nil {
		t.Fatal("expected compatibility path to call chat provider")
	}
	respCtx := provider.ResponsesRequestContextFromOptions(prov.lastChatReq.Options...)
	if respCtx == nil || respCtx.PreviousResponseID != "resp_123" {
		t.Fatalf("responses context = %+v, want previous_response_id preserved", respCtx)
	}
	if respCtx.Store == nil || *respCtx.Store {
		t.Fatalf("store = %#v, want false", respCtx.Store)
	}
}

func TestServeLLMApiReturnsNotImplementedWhenProviderDoesNotExposeResponses(t *testing.T) {
	prov := &testProvider{}
	handler := newHandler()

	body := `{"model":"gpt-4.1","input":"hello"}` + "\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("unexpected status code: got %d want %d", rec.Code, http.StatusNotImplemented)
	}
	if !strings.Contains(rec.Body.String(), "responses api is not supported") {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
}

func TestServeLLMApiWritesOpenAIErrorShape(t *testing.T) {
	handler := newHandler()
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, nil, nil); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected status code: got %d want %d", rec.Code, http.StatusMethodNotAllowed)
	}

	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   any    `json:"param"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if payload.Error.Message != "method not allowed" {
		t.Fatalf("unexpected error message: %q", payload.Error.Message)
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Fatalf("unexpected error type: %q", payload.Error.Type)
	}
}

func TestServeLLMApiProviderErrorsUseOpenAIErrorShape(t *testing.T) {
	prov := &testProvider{
		generateErr: testStatusError{msg: "quota exceeded", status: http.StatusTooManyRequests},
	}
	handler := newHandler()

	body, err := json.Marshal(ChatCompletionRequest{
		Model: "gpt-4.1",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "hello",
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest returned error: %v", err)
	}
	rec := httptest.NewRecorder()

	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("unexpected status code: got %d want %d", rec.Code, http.StatusTooManyRequests)
	}

	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if payload.Error.Message != "quota exceeded" {
		t.Fatalf("unexpected error message: %q", payload.Error.Message)
	}
	if payload.Error.Type != "rate_limit_error" {
		t.Fatalf("unexpected error type: %q", payload.Error.Type)
	}
}

func firstDataLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			return strings.TrimPrefix(line, "data: ")
		}
	}
	return ""
}
