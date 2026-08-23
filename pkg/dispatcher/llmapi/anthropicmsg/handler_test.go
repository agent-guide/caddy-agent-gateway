package anthropicmsg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	"github.com/agent-guide/agent-gateway/pkg/credential"
	sched "github.com/agent-guide/agent-gateway/pkg/credential/scheduler"
	"github.com/agent-guide/agent-gateway/pkg/dispatcher"
	"github.com/agent-guide/agent-gateway/pkg/httpclient"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/anthropicbase"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider/claudecode"
	"github.com/cloudwego/eino/schema"
)

func requiresFeature(requirements provider.ProtocolRequirementSet, feature provider.ProtocolFeature) bool {
	for _, item := range requirements.Features() {
		if item == feature {
			return true
		}
	}
	return false
}

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
	cfg     provider.ProviderConfig
	chunks  []*schema.Message
	recvErr error
}

type testRoutedExecutor struct {
	chat   *provider.ChatExecution
	stream *provider.StreamExecution
}

type testResolvedProvider struct {
	provider.Provider
	candidate provider.ServedCandidate
}

func (p *testResolvedProvider) ExecuteChat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatExecution, error) {
	resp, err := p.Provider.Chat(ctx, req)
	return &provider.ChatExecution{Response: resp, Resolved: provider.ResolvedExecution{Candidate: p.candidate}}, err
}

func (p *testResolvedProvider) ExecuteStreamChat(ctx context.Context, req *provider.ChatRequest) (*provider.StreamExecution, error) {
	stream, err := p.Provider.StreamChat(ctx, req)
	return &provider.StreamExecution{Stream: stream, Resolved: provider.ResolvedExecution{Candidate: p.candidate}}, err
}

func (p *testRoutedExecutor) ExecuteChat(context.Context, *provider.ChatRequest) (*provider.ChatExecution, error) {
	return p.chat, nil
}

func (p *testRoutedExecutor) ExecuteStreamChat(context.Context, *provider.ChatRequest) (*provider.StreamExecution, error) {
	return p.stream, nil
}

func (p *testRoutedExecutor) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("ordinary Chat adapter must not select response mode")
}

func (p *testRoutedExecutor) StreamChat(context.Context, *provider.ChatRequest) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("ordinary StreamChat adapter must not select response mode")
}

func (p *testRoutedExecutor) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return nil, nil
}
func (p *testRoutedExecutor) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{Streaming: true}
}
func (p *testRoutedExecutor) Config() provider.ProviderConfig {
	return provider.ProviderConfig{ProviderType: "deliberately-not-the-served-candidate"}
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
		if p.recvErr != nil {
			sw.Send(nil, p.recvErr)
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
	handler := NewHandler(StandardProfile())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)

	if !handler.MatchLLMApi(req) {
		t.Fatal("MatchLLMApi returned false for /v1/messages/count_tokens")
	}
}

func TestBatchRelayModeReadsServedCandidate(t *testing.T) {
	handler := NewHandler(StandardProfile())
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"client-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}]
	}`))
	prepared, _, err := handler.PrepareLLMApiRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	message := schema.AssistantMessage("native answer", nil)
	anthropicbase.AttachAnthropicResponseBody(message, json.RawMessage(`{
		"id":"msg_served_candidate","type":"message","role":"assistant","model":"upstream-model",
		"content":[{"type":"text","text":"native answer"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":2,"output_tokens":3},"future":"preserve"
	}`))
	executor := &testRoutedExecutor{chat: &provider.ChatExecution{
		Response: &provider.ChatResponse{Message: message},
		Resolved: provider.ResolvedExecution{Candidate: provider.ServedCandidate{
			Dialect:  provider.ProtocolDialectAnthropic,
			Features: map[provider.ProtocolFeature]struct{}{provider.FeatureAnthropicBodyRelay: {}},
		}},
	}}
	recorder := httptest.NewRecorder()
	if err := handler.ServeLLMApi(recorder, request, executor, prepared); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["id"] != "msg_served_candidate" || response["model"] != "client-model" || response["future"] != "preserve" {
		t.Fatalf("response = %#v", response)
	}
}

func TestRequestProtocolStateIsIndependentOfMessageOrdering(t *testing.T) {
	request := &MessagesRequest{
		Model: "client-model", MaxTokens: 64,
		Tools: []ToolDefinition{{Type: "web_search_20250305", Name: "web_search"}},
		Messages: []MessageItem{
			{Role: "user", Content: MessageContent{{Type: "text", Text: "first"}}},
			{Role: "assistant", Content: MessageContent{{Type: "text", Text: "second"}}},
		},
	}
	first := (&Converter{}).ToInternal(request)
	request.Messages[0], request.Messages[1] = request.Messages[1], request.Messages[0]
	second := (&Converter{}).ToInternal(request)
	firstTools, _ := anthropicbase.AnthropicRequestTools(first.ProtocolState)
	secondTools, _ := anthropicbase.AnthropicRequestTools(second.ProtocolState)
	if len(firstTools) != 1 || len(secondTools) != 1 || !bytes.Equal(firstTools[0], secondTools[0]) {
		t.Fatalf("request state changed with message order: %q / %q", firstTools, secondTools)
	}
	for _, message := range append(first.Messages, second.Messages...) {
		if state := provider.ProtocolStateFromMessage(message); state != nil && len(state.Envelopes) > 0 && state.Envelopes[0].Scope == provider.NativeScopeRequest {
			t.Fatal("request state was attached to a conversation message")
		}
	}
}

func TestPrepareLLMApiRequestRejectsInvalidClientToolSchema(t *testing.T) {
	handler := NewHandler(StandardProfile())
	for _, inputSchema := range []string{`"not-an-object"`, `{"type":"string"}`, `{"type":"object","properties":123}`} {
		body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"broken","input_schema":` + inputSchema + `}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		if _, _, err := handler.PrepareLLMApiRequest(req); err == nil || !strings.Contains(err.Error(), "input_schema") {
			t.Fatalf("input_schema %s error = %v, want validation failure", inputSchema, err)
		}
	}
}

func TestToInternalDoesNotSilentlyDropInvalidClientToolSchema(t *testing.T) {
	req := &MessagesRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "hi"}}}},
		Tools: []ToolDefinition{{
			Name: "broken", Description: "still visible for direct converter callers",
			InputSchema: json.RawMessage(`{"type":"object","properties":123}`),
		}},
	}
	internal := (&Converter{}).ToInternal(req)
	state, err := provider.ResolveChatRequest(context.Background(), provider.ProviderConfig{}, internal)
	if err != nil {
		t.Fatalf("ResolveChatRequest: %v", err)
	}
	if state.CommonOptions == nil || len(state.CommonOptions.Tools) != 1 || state.CommonOptions.Tools[0].Name != "broken" {
		t.Fatalf("converted options = %+v, want degraded broken tool", state.CommonOptions)
	}
}

func TestServeLLMApiCountTokensReturnsNotImplemented(t *testing.T) {
	handler := NewHandler(StandardProfile())
	body, err := json.Marshal(MessagesRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "hello world"}}}},
		Tools: []ToolDefinition{{
			Type: "web_search_20250305",
			Name: "web_search",
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens?beta=true", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	prepared, requirements, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Disposition != dispatcher.ExecutionLocal || !requirements.ProtocolRequirements.Empty() {
		t.Fatalf("local preparation = %+v, requirements = %+v", prepared, requirements)
	}
	if err := handler.ServeLLMApi(rec, req, nil, prepared); err != nil {
		t.Fatalf("ServeLLMApi returned error: %v", err)
	}

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCountTokensLocalExecutionUsesSharedLifecycle(t *testing.T) {
	handler := NewHandler(ClaudeCodeProfile())
	span := &recordingInteractionSpan{}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{
		"model":"client-model","messages":[{"role":"user","content":"hello world"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}]
	}`))
	request = request.WithContext(usage.ContextWithSpan(request.Context(), span))
	prepared, requirements, err := handler.PrepareLLMApiRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Disposition != dispatcher.ExecutionLocal || !requirements.ProtocolRequirements.Empty() {
		t.Fatalf("prepared=%+v requirements=%+v", prepared, requirements)
	}
	recorder := httptest.NewRecorder()
	if err := handler.ServeLLMApi(recorder, request, nil, prepared); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || len(span.finishes) != 1 || !span.finishes[0].Success {
		t.Fatalf("status=%d finishes=%+v", recorder.Code, span.finishes)
	}
	if len(span.exts) < 2 {
		t.Fatalf("extensions = %#v", span.exts)
	}
	final, ok := span.exts[len(span.exts)-1].(usage.LLMExtension)
	if !ok || final.UsageSource != "estimated" || final.InputTokens == nil || final.ProviderID != "" {
		t.Fatalf("final extension = %#v", span.exts[len(span.exts)-1])
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
	handler := NewHandler(StandardProfile())

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
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("unexpected status code: got %d want %d", rec.Code, http.StatusTooManyRequests)
	}
	if body := rec.Body.String(); strings.Contains(body, "event: message_start") || !strings.Contains(body, "rate limit") {
		t.Fatalf("stream body = %s, want uncommitted Anthropic HTTP error", body)
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

func TestServeLLMApiReturnsHTTPErrorForPreCommitReceiveFailure(t *testing.T) {
	handler := NewHandler(StandardProfile())
	prov := &testStreamingProvider{
		cfg:     provider.ProviderConfig{Id: "fixture", ProviderType: "fixture"},
		recvErr: errors.New("upstream disconnected before message start"),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"client-model","max_tokens":16,"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if !strings.Contains(rec.Body.String(), "upstream disconnected before message start") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestServeLLMApiReturnsHTTPErrorForInvalidFirstRelayEvent(t *testing.T) {
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(anthropicbase.AttachAnthropicRelayStreamEvent(nil, "content_block_start", json.RawMessage(`{
		"type":"content_block_start","index":0,
		"content_block":{"type":"text","text":""}
	}`)), nil)
	sw.Close()
	prov := &testRoutedExecutor{stream: &provider.StreamExecution{
		Stream: sr,
		Resolved: provider.ResolvedExecution{Candidate: provider.ServedCandidate{
			Dialect:  provider.ProtocolDialectAnthropic,
			Features: map[provider.ProtocolFeature]struct{}{provider.FeatureAnthropicStreamRelay: {}},
		}},
	}}
	handler := NewHandler(StandardProfile())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"client-model","max_tokens":16,"stream":true,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadGateway || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d content-type=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "overlaps active block") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestServeLLMApiRestoresClaudeCodeToolNameInNativeRelay(t *testing.T) {
	var upstreamToolName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode upstream request: %v", err)
			return
		}
		if len(request.Tools) > 0 {
			upstreamToolName = request.Tools[0].Name
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_upstream\",\"model\":\"upstream-model\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"Bash\",\"input\":{}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"cmd\\\":\\\"pwd\\\"}\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	base, err := claudecode.New(provider.ProviderConfig{
		Id: "claudecode", ProviderType: "claudecode", BaseURL: server.URL, APIKey: "fixture",
		Options: map[string]any{"compact": "codex"},
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	prov := &testResolvedProvider{Provider: base, candidate: provider.ServedCandidate{
		Dialect:  provider.ProtocolDialectAnthropic,
		Features: map[provider.ProtocolFeature]struct{}{provider.FeatureAnthropicStreamRelay: {}},
	}}
	handler := NewHandler(ClaudeCodeProfile())
	span := &recordingInteractionSpan{}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"client-model","max_tokens":16,"stream":true,
		"messages":[{"role":"user","content":"run pwd"}],
		"tools":[{"name":"exec_command","description":"run command","input_schema":{"type":"object"}}]
	}`))
	req = req.WithContext(usage.ContextWithSpan(req.Context(), span))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if upstreamToolName != "Bash" {
		t.Fatalf("upstream tool name = %q, want Bash", upstreamToolName)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"name":"exec_command"`) || strings.Contains(body, `"name":"Bash"`) {
		t.Fatalf("relayed body did not restore tool name: %s", body)
	}
	if len(span.exts) == 0 {
		t.Fatal("relay tool metrics were not recorded")
	}
	toolExtension, ok := span.exts[len(span.exts)-1].(usage.LLMExtension)
	if !ok || toolExtension.ToolCallCount == nil || *toolExtension.ToolCallCount != 1 || !slices.Contains(toolExtension.ToolNames, "exec_command") {
		t.Fatalf("relay tool extension = %#v", span.exts[len(span.exts)-1])
	}
}

func TestPrepareLLMApiRequestAcceptsSystemBlockArray(t *testing.T) {
	handler := NewHandler(StandardProfile())

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
	handler := NewHandler(StandardProfile())

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
	handler := NewHandler(StandardProfile())

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
	handler := NewHandler(StandardProfile())
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
	handler := NewHandler(StandardProfile())
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
	handler := NewHandler(StandardProfile())
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
	handler := NewHandler(StandardProfile())
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

func TestServeLLMApiStreamWaitsForToolIdentityBeforeStartingBlock(t *testing.T) {
	handler := NewHandler(StandardProfile())
	idx0 := 0
	prov := &testStreamingProvider{
		cfg: provider.ProviderConfig{Id: "compat", ProviderType: "openai"},
		chunks: []*schema.Message{
			{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				Index: &idx0, Function: schema.FunctionCall{Arguments: `{"city":`},
			}}},
			{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				Index: &idx0, ID: "call_1", Function: schema.FunctionCall{Name: "get_weather", Arguments: `"Paris"}`},
			}}, ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_use"}},
		},
	}
	body, err := json.Marshal(MessagesRequest{
		Model: "claude-sonnet-4-5", MaxTokens: 16, Stream: true,
		Messages: []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "weather"}}}},
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
	if strings.Contains(bodyText, `"id":"","input":{},"name":""`) {
		t.Fatalf("tool block started before identity arrived: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"id":"call_1","input":{},"name":"get_weather","type":"tool_use"`) {
		t.Fatalf("identified tool block missing: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"partial_json":"{\"city\":\"Paris\"}"`) {
		t.Fatalf("buffered arguments missing: %s", bodyText)
	}
}

func TestToInternalCarriesThinkingMetadataAndOutputConfig(t *testing.T) {
	conv := &Converter{}
	req := &MessagesRequest{
		Model:        "claude-sonnet-4-5",
		MaxTokens:    4096,
		Messages:     []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "hi"}}}},
		Thinking:     json.RawMessage(`{"type":"enabled","budget_tokens":2048,"display":"summarized"}`),
		Metadata:     json.RawMessage(`{"user_id":"end-user-1"}`),
		OutputConfig: json.RawMessage(`{"effort":"max","format":{"type":"json_schema","schema":{"type":"object"}}}`),
	}

	chatReq := conv.ToInternal(req)
	extra := provider.ChatExtraFieldsFromOptions(chatReq.Options...)
	if extra == nil {
		t.Fatal("ChatExtraFields = nil, want inbound thinking/metadata/output_config carried through")
	}
	if extra.Thinking["type"] != "enabled" {
		t.Fatalf("thinking type = %v, want enabled", extra.Thinking["type"])
	}
	if budget, _ := extra.Thinking["budget_tokens"].(int); budget != 2048 {
		t.Fatalf("thinking budget_tokens = %v, want 2048", extra.Thinking["budget_tokens"])
	}
	if extra.Thinking["display"] != "summarized" || extra.ReasoningEffort != "max" {
		t.Fatalf("thinking controls = %+v/%q, want display=summarized effort=max", extra.Thinking, extra.ReasoningEffort)
	}
	if extra.Metadata["user_id"] != "end-user-1" {
		t.Fatalf("metadata user_id = %v, want end-user-1", extra.Metadata["user_id"])
	}
	format, ok := extra.ResponseFormat.(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("response_format = %#v, want json_schema format from output_config", extra.ResponseFormat)
	}
}

func TestToInternalPreservesServerToolsAndNormalizesParameterlessClientTools(t *testing.T) {
	var req MessagesRequest
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"search"}],
		"tools":[
			{"type":"web_search_20260209","name":"web_search","max_uses":3,"allowed_domains":["example.com"]},
			{"name":"heartbeat","description":"Check health","input_schema":null}
		],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true}
	}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	chatReq := (&Converter{}).ToInternal(&req)
	state, err := provider.ResolveChatRequest(context.Background(), provider.ProviderConfig{}, chatReq)
	if err != nil {
		t.Fatalf("ResolveChatRequest: %v", err)
	}
	if state.CommonOptions == nil || len(state.CommonOptions.Tools) != 1 || state.CommonOptions.Tools[0].Name != "heartbeat" {
		t.Fatalf("common tools = %+v, want only the client heartbeat tool", state.CommonOptions)
	}
	js, err := state.CommonOptions.Tools[0].ToJSONSchema()
	if err != nil || js == nil || js.Type != "object" {
		t.Fatalf("heartbeat schema = %+v, %v; want empty object schema", js, err)
	}

	nativeTools, nativeChoice := anthropicbase.AnthropicRequestTools(chatReq.ProtocolState)
	if len(nativeTools) != 2 {
		t.Fatalf("native tools = %+v, want both original tool definitions", nativeTools)
	}
	var serverTool map[string]any
	if err := json.Unmarshal(nativeTools[0], &serverTool); err != nil {
		t.Fatalf("decode preserved server tool: %v", err)
	}
	if serverTool["type"] != "web_search_20260209" || serverTool["max_uses"] != float64(3) {
		t.Fatalf("preserved server tool = %#v", serverTool)
	}
	if _, exists := serverTool["input_schema"]; exists {
		t.Fatalf("server tool gained input_schema: %#v", serverTool)
	}
	var clientTool map[string]any
	if err := json.Unmarshal(nativeTools[1], &clientTool); err != nil {
		t.Fatalf("decode preserved client tool: %v", err)
	}
	if schemaValue, ok := clientTool["input_schema"].(map[string]any); !ok || schemaValue["type"] != "object" {
		t.Fatalf("normalized client tool = %#v, want object input_schema", clientTool)
	}
	if string(nativeChoice) != `{"type":"auto","disable_parallel_tool_use":true}` {
		t.Fatalf("tool_choice = %s, want exact native value", nativeChoice)
	}
}

func TestToInternalUsesGenericToolPathForModeledClientTools(t *testing.T) {
	var req MessagesRequest
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"lookup"}],
		"tools":[{"type":"custom","name":"lookup","description":"Lookup data","input_schema":{"type":"object","properties":{}}}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true}
	}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	chatReq := (&Converter{}).ToInternal(&req)
	extra := provider.ChatExtraFieldsFromOptions(chatReq.Options...)
	if extra == nil {
		t.Fatal("ChatExtraFields = nil, want parallel tool setting")
	}
	if nativeTools, nativeChoice := anthropicbase.AnthropicRequestTools(chatReq.ProtocolState); len(nativeTools) != 0 || len(nativeChoice) != 0 {
		t.Fatalf("ordinary client tools unexpectedly used raw Anthropic replay: %s %+v", nativeChoice, nativeTools)
	}
	if extra.ParallelToolCalls == nil || *extra.ParallelToolCalls {
		t.Fatalf("parallel tool setting = %v, want disabled", extra.ParallelToolCalls)
	}
}

func TestToInternalPreservesUnmodeledClientToolFields(t *testing.T) {
	var req MessagesRequest
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"lookup"}],
		"tools":[{"type":"custom","name":"lookup","input_schema":{"type":"object"},"defer_loading":true}]
	}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	chatReq := (&Converter{}).ToInternal(&req)
	nativeTools, _ := anthropicbase.AnthropicRequestTools(chatReq.ProtocolState)
	if len(nativeTools) != 1 || !bytes.Contains(nativeTools[0], []byte(`"defer_loading":true`)) {
		t.Fatalf("unmodeled custom tool field was not retained: %+v", nativeTools)
	}
}

func TestConverterPreservesNativeServerToolContentBothDirections(t *testing.T) {
	var req MessagesRequest
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"assistant","content":[
			{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"latest"}},
			{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[{"type":"web_search_result","url":"https://example.com","title":"Example","encrypted_content":"opaque"}]},
			{"type":"text","text":"Result","citations":[{"url":"https://example.com"}]}
		]}]
	}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	chatReq := (&Converter{}).ToInternal(&req)
	if len(chatReq.Messages) != 1 {
		t.Fatalf("messages = %+v, want one assistant message", chatReq.Messages)
	}
	if blocks := anthropicbase.AnthropicContentBlocksFromMessage(chatReq.Messages[0]); len(blocks) != 3 {
		t.Fatalf("native blocks = %d, want 3", len(blocks))
	}
	response := (&Converter{}).FromInternal(&provider.ChatResponse{Message: chatReq.Messages[0]}, req.Model)
	wire, err := json.Marshal(response.Content)
	if err != nil {
		t.Fatalf("marshal response content: %v", err)
	}
	for _, want := range []string{`"server_tool_use"`, `"web_search_tool_result"`, `"citations"`} {
		if !strings.Contains(string(wire), want) {
			t.Fatalf("response content = %s, missing %s", wire, want)
		}
	}

	var citationOnly MessagesRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"assistant","content":[{"type":"text","text":"Result","citations":[{"url":"https://example.com"}]}]}]}`), &citationOnly); err != nil {
		t.Fatalf("unmarshal citation-only history: %v", err)
	}
	citationReq := (&Converter{}).ToInternal(&citationOnly)
	if len(citationReq.Messages) != 1 || len(anthropicbase.AnthropicContentBlocksFromMessage(citationReq.Messages[0])) != 1 {
		t.Fatalf("citation-only history lost native metadata: %+v", citationReq.Messages)
	}
}

func TestPrepareLLMApiRequestRequiresAnthropicNativeProvider(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"tools":[{"type":"web_search_20260209","name":"web_search","max_uses":2}],
		"messages":[{"role":"assistant","content":[{"type":"text","text":"Result","citations":[{"url":"https://example.com"}]}]}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	_, requirements, err := NewHandler(StandardProfile()).PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest() error = %v", err)
	}
	if requirements.RequireTools || !requiresFeature(requirements.ProtocolRequirements, provider.FeatureAnthropicServerToolRequest) ||
		!requiresFeature(requirements.ProtocolRequirements, provider.FeatureAnthropicNativeResponse) ||
		!requiresFeature(requirements.ProtocolRequirements, provider.FeatureAnthropicNativeHistoryReplay) {
		t.Fatalf("requirements = %+v, want native fidelity without generic client-tool capability", requirements)
	}
	if reasons := requirements.ProtocolRequirements.Reasons(provider.FeatureAnthropicServerToolRequest); !slices.Equal(reasons, []provider.RequirementReason{provider.ReasonAnthropicServerTool}) {
		t.Fatalf("server tool reasons = %v", reasons)
	}
}

func TestPrepareLLMApiRequestIgnoresNullUnmodeledContentFields(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"assistant","content":[{"type":"text","text":"Result","citations":null}]}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, requirements, err := NewHandler(StandardProfile()).PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest() error = %v", err)
	}
	if !requirements.ProtocolRequirements.Empty() {
		t.Fatal("null citations unexpectedly required Anthropic-native replay")
	}
	if anthropicbase.HasAnthropicNativeContent(prepared.ChatRequest.Messages) {
		t.Fatal("null citations unexpectedly attached native content")
	}
}

func TestServeLLMApiStreamsNativeServerToolEvents(t *testing.T) {
	handler := NewHandler(StandardProfile())
	start := json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}}`)
	delta := json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"latest\"}"}}`)
	stop := json.RawMessage(`{"type":"content_block_stop","index":0}`)
	citation := json.RawMessage(`{"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"url":"https://example.com"}}}`)
	prov := &testStreamingProvider{chunks: []*schema.Message{
		anthropicbase.AttachAnthropicStreamEvent(nil, "content_block_start", start),
		anthropicbase.AttachAnthropicStreamEvent(nil, "content_block_delta", delta),
		anthropicbase.AttachAnthropicStreamEvent(nil, "content_block_stop", stop),
		{Role: schema.Assistant, Content: "Result"},
		anthropicbase.AttachAnthropicStreamEvent(nil, "content_block_delta", citation),
		{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{FinishReason: "end_turn"}},
	}}
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"search"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest: %v", err)
	}
	rec := httptest.NewRecorder()
	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi: %v", err)
	}
	wire := rec.Body.String()
	for _, want := range []string{`"type":"server_tool_use"`, `"type":"citations_delta"`, `"index":1`, `"text":"Result"`} {
		if !strings.Contains(wire, want) {
			t.Fatalf("stream = %s, missing %s", wire, want)
		}
	}
}

func TestServeLLMApiClosesGeneratedTextAndRemapsNativeBlockIndex(t *testing.T) {
	handler := NewHandler(StandardProfile())
	start := json.RawMessage(`{"type":"content_block_start","index":2,"content_block":{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}}`)
	delta := json.RawMessage(`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"latest\"}"}}`)
	stop := json.RawMessage(`{"type":"content_block_stop","index":2}`)
	prov := &testStreamingProvider{chunks: []*schema.Message{
		{Role: schema.Assistant, Content: "before"},
		anthropicbase.AttachAnthropicStreamEvent(nil, "content_block_start", start),
		anthropicbase.AttachAnthropicStreamEvent(nil, "content_block_delta", delta),
		anthropicbase.AttachAnthropicStreamEvent(nil, "content_block_stop", stop),
		{Role: schema.Assistant, Content: "after"},
		{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{FinishReason: "end_turn"}},
	}}
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"search"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, _, err := handler.PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest() error = %v", err)
	}
	rec := httptest.NewRecorder()
	if err := handler.ServeLLMApi(rec, req, prov, prepared); err != nil {
		t.Fatalf("ServeLLMApi() error = %v", err)
	}

	var sequence []string
	for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
		lines := strings.Split(frame, "\n")
		if len(lines) < 2 || !strings.HasPrefix(lines[0], "event: content_block_") || !strings.HasPrefix(lines[1], "data: ") {
			continue
		}
		var event struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type string `json:"type"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &event); err != nil {
			t.Fatalf("decode SSE frame %q: %v", frame, err)
		}
		detail := event.ContentBlock.Type
		if detail == "" {
			detail = event.Delta.Type
		}
		sequence = append(sequence, fmt.Sprintf("%s:%d:%s", event.Type, event.Index, detail))
	}
	want := []string{
		"content_block_start:0:text",
		"content_block_delta:0:text_delta",
		"content_block_stop:0:",
		"content_block_start:1:server_tool_use",
		"content_block_delta:1:input_json_delta",
		"content_block_stop:1:",
		"content_block_start:2:text",
		"content_block_delta:2:text_delta",
		"content_block_stop:2:",
	}
	if !slices.Equal(sequence, want) {
		t.Fatalf("content block sequence = %v, want %v\nstream:\n%s", sequence, want, rec.Body.String())
	}
}

func TestToInternalFiltersUnknownThinkingFieldsAndInvalidBudget(t *testing.T) {
	chatReq := (&Converter{}).ToInternal(&MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 4096,
		Messages:  []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "hi"}}}},
		Thinking:  json.RawMessage(`{"type":"enabled","budget_tokens":-1,"display":"summarized","vendor_secret":"leak"}`),
	})
	extra := provider.ChatExtraFieldsFromOptions(chatReq.Options...)
	if extra == nil || extra.Thinking["type"] != "enabled" || extra.Thinking["display"] != "summarized" {
		t.Fatalf("thinking = %+v, want supported fields", extra)
	}
	if _, ok := extra.Thinking["budget_tokens"]; ok {
		t.Fatalf("thinking = %+v, want invalid budget removed", extra.Thinking)
	}
	if _, ok := extra.Thinking["vendor_secret"]; ok {
		t.Fatalf("thinking = %+v, want unknown field removed", extra.Thinking)
	}
}

func TestAnthropicConverterRoundTripsProviderReasoning(t *testing.T) {
	conv := &Converter{}
	chatReq := conv.ToInternal(&MessagesRequest{
		Model: "glm-5.2", MaxTokens: 4096,
		Messages: []MessageItem{{
			Role: "assistant",
			Content: MessageContent{
				{Type: "thinking"},
				{Type: "thinking", Thinking: "inspect first", Signature: "opaque"},
				{Type: "redacted_thinking"},
				{Type: "redacted_thinking", Data: "opaque-redacted"},
				{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"README.md"}`)},
			},
		}},
	})
	if len(chatReq.Messages) != 1 || chatReq.Messages[0].ReasoningContent != "inspect first" || len(chatReq.Messages[0].ToolCalls) != 1 {
		t.Fatalf("chat messages = %+v", chatReq.Messages)
	}
	if parts := provider.ReasoningPartsFromMessage(chatReq.Messages[0]); len(parts) != 2 ||
		parts[0].Reasoning == nil || parts[0].Reasoning.Signature != "opaque" ||
		provider.EncryptedReasoningData(parts[1]) != "opaque-redacted" {
		t.Fatalf("structured reasoning = %+v, want original thinking metadata", parts)
	}
	if len(chatReq.Messages[0].AssistantGenMultiContent) != 0 {
		t.Fatalf("AssistantGenMultiContent = %+v, want gateway reasoning isolated in Extra", chatReq.Messages[0].AssistantGenMultiContent)
	}

	responseMessage := provider.AttachReasoningParts(&schema.Message{
		Role: schema.Assistant, Content: "done", ReasoningContent: "inspect first",
	},
		provider.NewReasoningOutputPart("inspect first", "opaque", nil),
		provider.NewEncryptedReasoningOutputPart("opaque-redacted", nil),
	)
	resp := conv.FromInternal(&provider.ChatResponse{Message: responseMessage}, "glm-5.2")
	if len(resp.Content) != 3 || resp.Content[0].Type != "thinking" || resp.Content[0].Thinking != "inspect first" || resp.Content[0].Signature != "opaque" {
		t.Fatalf("response content = %+v", resp.Content)
	}
	if resp.Content[1].Type != "redacted_thinking" || resp.Content[1].Data != "opaque-redacted" {
		t.Fatalf("redacted thinking = %+v", resp.Content[1])
	}
	if resp.Content[2].Type != "text" || resp.Content[2].Text != "done" {
		t.Fatalf("response text = %+v", resp.Content[2])
	}
}

func TestAnthropicConverterSynthesizesSignatureForFlatReasoning(t *testing.T) {
	resp := (&Converter{}).FromInternal(&provider.ChatResponse{Message: &schema.Message{
		Role: schema.Assistant, ReasoningContent: "inspect first",
	}}, "reasoning-model")
	if len(resp.Content) != 1 || !strings.HasPrefix(resp.Content[0].Signature, "agw-thinking-") {
		t.Fatalf("response content = %+v, want provider-neutral fallback signature", resp.Content)
	}
}

func TestServeLLMApiStreamsProviderReasoningAsThinkingBlock(t *testing.T) {
	handler := NewHandler(StandardProfile())
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

func TestServeLLMApiStreamsStructuredReasoningWithoutChangingOpaqueState(t *testing.T) {
	handler := NewHandler(StandardProfile())
	index := 0
	reasoningChunk := func(msg *schema.Message, parts ...schema.MessageOutputPart) *schema.Message {
		return provider.AttachReasoningParts(msg, parts...)
	}
	prov := &testStreamingProvider{
		cfg: provider.ProviderConfig{Id: "anthropic-compatible", ProviderType: "claudecode"},
		chunks: []*schema.Message{
			reasoningChunk(&schema.Message{Role: schema.Assistant},
				provider.NewReasoningOutputPart("", "", &index),
			),
			reasoningChunk(&schema.Message{Role: schema.Assistant, ReasoningContent: "inspect"},
				provider.NewReasoningOutputPart("inspect", "", &index),
			),
			reasoningChunk(&schema.Message{Role: schema.Assistant},
				provider.NewReasoningOutputPart("", "opaque-", &index),
			),
			reasoningChunk(&schema.Message{Role: schema.Assistant},
				provider.NewReasoningOutputPart("", "signature", &index),
			),
			reasoningChunk(&schema.Message{Role: schema.Assistant},
				provider.NewReasoningEndOutputPart(index),
			),
			reasoningChunk(&schema.Message{Role: schema.Assistant},
				provider.NewEncryptedReasoningOutputPart("opaque-redacted", nil),
			),
			{Role: schema.Assistant, Content: "done", ResponseMeta: &schema.ResponseMeta{FinishReason: "stop"}},
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
	for _, want := range []string{
		`"delta":{"signature":"opaque-signature","type":"signature_delta"}`,
		`"content_block":{"data":"opaque-redacted","type":"redacted_thinking"}`,
		`"delta":{"text":"done","type":"text_delta"}`,
	} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("stream missing %q: %s", want, bodyText)
		}
	}
	if strings.Contains(bodyText, gatewayThinkingSignature("inspect")) {
		t.Fatalf("stream replaced upstream signature: %s", bodyText)
	}
}

func TestServeLLMApiDropsReasoningThatArrivesAfterText(t *testing.T) {
	handler := NewHandler(StandardProfile())
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

// parseSSEBlockEvents returns the (event, index) pairs of every content_block_*
// event in an SSE body.
func parseSSEBlockEvents(t *testing.T, body string) [][2]any {
	t.Helper()
	var events [][2]any
	var eventName string
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			eventName = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if !strings.HasPrefix(eventName, "content_block_") {
				continue
			}
			var payload struct {
				Index *int `json:"index"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				t.Fatalf("decode %s data: %v", eventName, err)
			}
			if payload.Index == nil {
				t.Fatalf("%s event without index: %s", eventName, line)
			}
			events = append(events, [2]any{eventName, *payload.Index})
		}
	}
	return events
}

// assertContentBlockDiscipline enforces the Anthropic streaming contract: a
// block index is started once, only receives deltas while open, and is stopped
// exactly once.
func assertContentBlockDiscipline(t *testing.T, body string) {
	t.Helper()
	open := map[int]bool{}
	seen := map[int]bool{}
	activeIndex := -1
	for _, event := range parseSSEBlockEvents(t, body) {
		name, index := event[0].(string), event[1].(int)
		switch name {
		case "content_block_start":
			if seen[index] {
				t.Fatalf("block %d started twice:\n%s", index, body)
			}
			if activeIndex >= 0 {
				t.Fatalf("block %d started while block %d is still open:\n%s", index, activeIndex, body)
			}
			seen[index] = true
			open[index] = true
			activeIndex = index
		case "content_block_delta":
			if !open[index] {
				t.Fatalf("delta for block %d that is not open:\n%s", index, body)
			}
		case "content_block_stop":
			if !open[index] {
				t.Fatalf("stop for block %d that is not open:\n%s", index, body)
			}
			open[index] = false
			activeIndex = -1
		}
	}
	for index, isOpen := range open {
		if isOpen {
			t.Fatalf("block %d was not stopped:\n%s", index, body)
		}
	}
}

func TestServeLLMApiStreamKeepsInterleavedToolFragmentsInOneBlock(t *testing.T) {
	handler := NewHandler(StandardProfile())
	idx0 := 0
	// An upstream that interleaves text between tool-call fragments must still
	// produce one tool_use block with one complete JSON input value.
	prov := &testStreamingProvider{
		cfg: provider.ProviderConfig{Id: "deepseek", ProviderType: "deepseek"},
		chunks: []*schema.Message{
			{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				Index: &idx0, ID: "call_1", Type: "function",
				Function: schema.FunctionCall{Name: "get_weather", Arguments: `{"ci`},
			}}},
			{Role: schema.Assistant, Content: "checking"},
			{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				Index: &idx0, Function: schema.FunctionCall{Arguments: `ty":"Paris"}`},
			}}, ResponseMeta: &schema.ResponseMeta{FinishReason: "tool_use"}},
		},
	}

	body, err := json.Marshal(MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Stream:    true,
		Messages:  []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "weather"}}}},
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

	assertContentBlockDiscipline(t, bodyText)
	if n := strings.Count(bodyText, `"id":"call_1"`); n != 1 {
		t.Fatalf("tool_use starts carrying call_1 = %d, want exactly one: %s", n, bodyText)
	}
	var toolIndex = -1
	var arguments strings.Builder
	for _, frame := range strings.Split(bodyText, "\n\n") {
		lines := strings.Split(frame, "\n")
		if len(lines) < 2 || !strings.HasPrefix(lines[1], "data: ") {
			continue
		}
		var event struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &event); err != nil {
			t.Fatalf("decode SSE frame: %v", err)
		}
		if event.ContentBlock.Type == "tool_use" {
			toolIndex = event.Index
		}
		if event.Delta.Type == "input_json_delta" && event.Index == toolIndex {
			arguments.WriteString(event.Delta.PartialJSON)
		}
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(arguments.String()), &input); err != nil {
		t.Fatalf("combined tool input %q is invalid JSON: %v\n%s", arguments.String(), err, bodyText)
	}
	if input["city"] != "Paris" {
		t.Fatalf("combined tool input = %+v, want city Paris", input)
	}
	if !strings.Contains(bodyText, `"text":"checking"`) {
		t.Fatalf("interleaved text was lost: %s", bodyText)
	}
}

func TestServeLLMApiStreamSynthesizesMissingToolCallID(t *testing.T) {
	handler := NewHandler(StandardProfile())
	idx0 := 0
	prov := &testStreamingProvider{
		cfg: provider.ProviderConfig{Id: "compatible", ProviderType: "openai"},
		chunks: []*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			Index: &idx0,
			Function: schema.FunctionCall{
				Name:      "get_weather",
				Arguments: `{"city":"Paris"}`,
			},
		}}}},
	}

	body, err := json.Marshal(MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Stream:    true,
		Messages:  []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "weather"}}}},
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

	assertContentBlockDiscipline(t, bodyText)
	if !strings.Contains(bodyText, `"id":"toolu_`) {
		t.Fatalf("synthetic tool id missing: %s", bodyText)
	}
	if !strings.Contains(bodyText, `"stop_reason":"tool_use"`) {
		t.Fatalf("tool stop reason missing: %s", bodyText)
	}
}

func TestServeLLMApiStreamClosesCompleteToolBeforeFollowingText(t *testing.T) {
	handler := NewHandler(StandardProfile())
	idx0 := 0
	prov := &testStreamingProvider{
		cfg: provider.ProviderConfig{Id: "compatible", ProviderType: "openai"},
		chunks: []*schema.Message{
			{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				Index: &idx0, ID: "call_1",
				Function: schema.FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`},
			}}},
			{Role: schema.Assistant, Content: "I will check."},
		},
		recvErr: errors.New("upstream disconnected after text"),
	}

	body, err := json.Marshal(MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Stream:    true,
		Messages:  []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "weather"}}}},
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

	if !strings.Contains(bodyText, `"text":"I will check."`) {
		t.Fatalf("complete-tool follower text was deferred until after the receive error: %s", bodyText)
	}
	toolStop := strings.Index(bodyText, "event: content_block_stop")
	textStart := strings.Index(bodyText, `"content_block":{"text":"","type":"text"}`)
	if toolStop < 0 || textStart < 0 || toolStop > textStart {
		t.Fatalf("tool block was not closed before text block: %s", bodyText)
	}
}

func TestPrepareLLMApiRequestRequiresNativeProviderForSignedReasoning(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"inspect","signature":"authentic-signature"},
			{"type":"redacted_thinking","data":"opaque-redacted"}
		]}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, requirements, err := NewHandler(StandardProfile()).PrepareLLMApiRequest(req)
	if err != nil {
		t.Fatalf("PrepareLLMApiRequest() error = %v", err)
	}
	if !requiresFeature(requirements.ProtocolRequirements, provider.FeatureAnthropicReasoningReplay) {
		t.Fatal("signed reasoning did not require Anthropic reasoning replay")
	}
	if requiresFeature(requirements.ProtocolRequirements, provider.FeatureAnthropicNativeHistoryReplay) {
		t.Fatal("modeled signed reasoning unnecessarily required full native-content support")
	}
	if !anthropicbase.HasAnthropicNativeReasoning(prepared.ChatRequest.Messages) {
		t.Fatal("signed reasoning was not retained in the internal request")
	}
}

func TestServeLLMApiFailsClosedForUnmappableNativeStreamEvents(t *testing.T) {
	handler := NewHandler(StandardProfile())
	// A native delta/stop whose upstream block was never started downstream has
	// no index to point at; forwarding it would break the client's block map.
	prov := &testStreamingProvider{
		cfg: provider.ProviderConfig{Id: "claudecode", ProviderType: "claudecode"},
		chunks: []*schema.Message{
			anthropicbase.AttachAnthropicStreamEvent(nil, "content_block_delta",
				json.RawMessage(`{"type":"content_block_delta","index":7,"delta":{"type":"citations_delta","citation":{"url":"https://example.com"}}}`)),
			anthropicbase.AttachAnthropicStreamEvent(nil, "content_block_stop",
				json.RawMessage(`{"type":"content_block_stop","index":7}`)),
			{Role: schema.Assistant, Content: "answer"},
		},
	}

	body, err := json.Marshal(MessagesRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Stream:    true,
		Messages:  []MessageItem{{Role: "user", Content: MessageContent{{Type: "text", Text: "hi"}}}},
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

	assertContentBlockDiscipline(t, bodyText)
	if !strings.Contains(bodyText, "event: error") || !strings.Contains(bodyText, "unopened block index 7") {
		t.Fatalf("unmappable citation did not produce typed stream error: %s", bodyText)
	}
	if strings.Contains(bodyText, `"text":"answer"`) {
		t.Fatalf("stream continued after invalid native lifecycle: %s", bodyText)
	}
}
