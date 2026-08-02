package openaibase

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/credential"
	"github.com/agent-guide/agent-gateway/pkg/httpclient"
	"github.com/agent-guide/agent-gateway/pkg/llm/provider"
)

func TestBaseUsesEmbeddedProviderConfig(t *testing.T) {
	var hit bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		if r.URL.Path != "/models" {
			t.Fatalf("request path = %q, want /models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	defer server.Close()

	cfg := provider.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: "http://127.0.0.1:1",
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	}
	base := NewBase(cfg)

	base.BaseURL = server.URL
	models, err := base.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if !hit {
		t.Fatal("server was not called")
	}
	if len(models) != 1 || models[0].ID != "test-model" {
		t.Fatalf("models = %#v, want test-model", models)
	}
}

func TestBaseEmbeddingUsesCredentialOverrideForAPIKey(t *testing.T) {
	var authHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Fatalf("request path = %q, want /embeddings", r.URL.Path)
		}
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2]}],"model":"text-embedding-3-large","usage":{"prompt_tokens":1,"completion_tokens":0}}`))
	}))
	defer server.Close()

	base := NewBase(provider.ProviderConfig{
		APIKey:  "static-key",
		BaseURL: server.URL,
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})

	ctx := provider.WithCredential(context.Background(), providerCredential{
		apiKey: "managed-key",
	}.toCredential())
	resp, err := base.Embedding(ctx, &provider.EmbeddingRequest{
		Model: "text-embedding-3-large",
		Texts: []string{"hello"},
	})
	if err != nil {
		t.Fatalf("Embedding() error = %v", err)
	}
	if resp == nil || resp.Model != "text-embedding-3-large" || len(resp.Embeddings) != 1 {
		t.Fatalf("unexpected embedding response: %+v", resp)
	}
	if authHeader != "Bearer managed-key" {
		t.Fatalf("authorization = %q, want Bearer managed-key", authHeader)
	}
}

func TestBaseCreateResponseCallsResponsesEndpoint(t *testing.T) {
	var body string
	var authHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %q, want /responses", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("request method = %q, want POST", r.Method)
		}
		authHeader = r.Header.Get("Authorization")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"model":"gpt-4.1","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	base := NewBase(provider.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})

	ctx := provider.WithCredential(context.Background(), providerCredential{
		apiKey: "managed-key",
	}.toCredential())
	resp, err := base.DoCreateResponses(ctx, &provider.ResponsesRequest{
		Model: "gpt-4.1",
		Input: "hello",
	})
	if err != nil {
		t.Fatalf("CreateResponse() error = %v", err)
	}
	if resp == nil || resp.ID != "resp_1" || resp.Model != "gpt-4.1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if body == "" {
		t.Fatal("expected request body to be sent")
	}
	if authHeader != "Bearer managed-key" {
		t.Fatalf("authorization = %q, want Bearer managed-key", authHeader)
	}
}

func TestBaseCreateResponsePreservesRawStructuredContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %q, want /responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"model":"gpt-4.1","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"no","severity":"high"}]}]}`))
	}))
	defer server.Close()

	base := NewBase(provider.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})

	resp, err := base.DoCreateResponses(context.Background(), &provider.ResponsesRequest{
		Model: "gpt-4.1",
		Input: "hello",
	})
	if err != nil {
		t.Fatalf("CreateResponse() error = %v", err)
	}
	if resp == nil || len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	part := resp.Output[0].Content[0]
	if part.Type != "refusal" || part.Refusal != "no" {
		t.Fatalf("content part = %+v, want refusal=no", part)
	}
	raw := string(resp.RawJSON)
	for _, want := range []string{`"type":"refusal"`, `"severity":"high"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("expected %q in raw response, got %q", want, raw)
		}
	}
}

func TestBaseStreamResponseParsesSSEEvents(t *testing.T) {
	var authHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %q, want /responses", r.URL.Path)
		}
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"model\":\"gpt-4.1\",\"output\":[]}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"model\":\"gpt-4.1\",\"output\":[]}}\n\n"))
	}))
	defer server.Close()

	base := NewBase(provider.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})

	ctx := provider.WithCredential(context.Background(), providerCredential{
		apiKey: "managed-key",
	}.toCredential())
	stream, err := base.DoStreamResponses(ctx, &provider.ResponsesRequest{
		Model:  "gpt-4.1",
		Input:  "hello",
		Stream: true,
	})
	if err != nil {
		t.Fatalf("StreamResponse() error = %v", err)
	}
	defer stream.Close()

	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if first.Type != "response.created" || first.Response == nil || first.Response.ID != "resp_1" {
		t.Fatalf("unexpected first event: %+v", first)
	}
	second, err := stream.Recv()
	if err != nil {
		t.Fatalf("second event: %v", err)
	}
	if second.Type != "response.output_text.delta" || second.Delta != "hello" {
		t.Fatalf("unexpected second event: %+v", second)
	}
	third, err := stream.Recv()
	if err != nil {
		t.Fatalf("third event: %v", err)
	}
	if third.Type != "response.completed" || third.Response == nil || third.Response.ID != "resp_1" {
		t.Fatalf("unexpected third event: %+v", third)
	}
	if authHeader != "Bearer managed-key" {
		t.Fatalf("authorization = %q, want Bearer managed-key", authHeader)
	}
}

func TestBaseStreamResponseStopsOnCompletedEvent(t *testing.T) {
	requestDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %q, want /responses", r.URL.Path)
		}
		defer close(requestDone)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"model\":\"gpt-4.1\",\"output\":[]}}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	base := NewBase(provider.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})

	stream, err := base.DoStreamResponses(context.Background(), &provider.ResponsesRequest{
		Model:  "gpt-4.1",
		Input:  "hello",
		Stream: true,
	})
	if err != nil {
		t.Fatalf("StreamResponse() error = %v", err)
	}
	defer stream.Close()

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if event.Type != "response.completed" {
		t.Fatalf("event type = %q, want response.completed", event.Type)
	}

	_, err = stream.Recv()
	if err != io.EOF {
		t.Fatalf("next recv error = %v, want io.EOF", err)
	}

	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not close after response.completed")
	}
}

func TestBaseStreamResponseParsesStructuredFunctionCallEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %q, want /responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_item.added\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item_id\":\"fc_1\",\"output_index\":1,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"cat\\\"}\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.function_call_arguments.delta\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"output_index\":1,\"delta\":\"{\\\"q\\\":\\\"cat\\\"}\"}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.done\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item_id\":\"fc_1\",\"output_index\":1,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"cat\\\"}\"}}\n\n"))
	}))
	defer server.Close()

	base := NewBase(provider.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})

	stream, err := base.DoStreamResponses(context.Background(), &provider.ResponsesRequest{
		Model:  "gpt-4.1",
		Input:  "hello",
		Stream: true,
	})
	if err != nil {
		t.Fatalf("StreamResponse() error = %v", err)
	}
	defer stream.Close()

	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if first.Type != "response.output_item.added" || first.Item == nil || first.Item.Type != "function_call" || first.Item.Name != "lookup" {
		t.Fatalf("unexpected first event: %+v", first)
	}
	second, err := stream.Recv()
	if err != nil {
		t.Fatalf("second event: %v", err)
	}
	if second.Type != "response.function_call_arguments.delta" || second.Delta != `{"q":"cat"}` {
		t.Fatalf("unexpected second event: %+v", second)
	}
	third, err := stream.Recv()
	if err != nil {
		t.Fatalf("third event: %v", err)
	}
	if third.Type != "response.output_item.done" || third.Item == nil || third.Item.CallID != "call_1" {
		t.Fatalf("unexpected third event: %+v", third)
	}
}

func TestBaseStreamResponseHandlesLargeDataLine(t *testing.T) {
	largeDelta := strings.Repeat("x", 70*1024)
	payload, err := json.Marshal(map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       "msg_1",
		"output_index":  0,
		"content_index": 0,
		"delta":         largeDelta,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %q, want /responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(payload)
		_, _ = w.Write([]byte("\n\n"))
	}))
	defer server.Close()

	base := NewBase(provider.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})

	stream, err := base.DoStreamResponses(context.Background(), &provider.ResponsesRequest{
		Model:  "gpt-4.1",
		Input:  "hello",
		Stream: true,
	})
	if err != nil {
		t.Fatalf("StreamResponse() error = %v", err)
	}
	defer stream.Close()

	ev, err := stream.Recv()
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if ev.Type != "response.output_text.delta" || ev.Delta != largeDelta {
		t.Fatalf("event type/delta length = %q/%d, want response.output_text.delta/%d", ev.Type, len(ev.Delta), len(largeDelta))
	}
}

func TestBaseStreamResponsePreservesRawStructuredPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %q, want /responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_item.added\n"))
		_, _ = w.Write([]byte("data: {\"item_id\":\"msg_1\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"refusal\",\"refusal\":\"no\"}]},\"sequence_number\":7}\n\n"))
	}))
	defer server.Close()

	base := NewBase(provider.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Network: httpclient.NetworkConfig{RequestTimeoutSeconds: 5},
	})

	stream, err := base.DoStreamResponses(context.Background(), &provider.ResponsesRequest{
		Model:  "gpt-4.1",
		Input:  "hello",
		Stream: true,
	})
	if err != nil {
		t.Fatalf("StreamResponse() error = %v", err)
	}
	defer stream.Close()

	ev, err := stream.Recv()
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if ev.Type != "response.output_item.added" {
		t.Fatalf("type = %q, want response.output_item.added", ev.Type)
	}
	raw := string(ev.RawJSON)
	for _, want := range []string{`"type":"response.output_item.added"`, `"sequence_number":7`, `"refusal":"no"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("expected %q in raw payload, got %q", want, raw)
		}
	}
}

type providerCredential struct {
	apiKey string
}

func (c providerCredential) toCredential() *credential.Credential {
	return &credential.Credential{
		Attributes: map[string]string{
			"api_key": c.apiKey,
		},
	}
}
