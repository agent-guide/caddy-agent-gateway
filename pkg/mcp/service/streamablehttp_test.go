package service_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/mcp/service"
	"github.com/agent-guide/agent-gateway/pkg/mcp/transport"
)

// streamableHTTPTestServer is a full fake Streamable HTTP MCP server.
// It routes by JSON-RPC method, assigns MCP-Session-Id on initialize,
// and records each incoming request for assertions.
type streamableHTTPTestServer struct {
	mu       sync.Mutex
	received []httpTestRequest
	// sseOnToolsCall, when true, makes tools/call respond with text/event-stream
	// instead of application/json.
	sseOnToolsCall bool
	// initializedStatus overrides the status returned for notifications/initialized.
	// When zero, the fake server returns 202 Accepted.
	initializedStatus int
}

type httpTestRequest struct {
	Method    string
	SessionID string // MCP-Session-Id header value
	AuthZ     string // Authorization header value
}

func (s *streamableHTTPTestServer) record(r *http.Request, method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = append(s.received, httpTestRequest{
		Method:    method,
		SessionID: r.Header.Get("MCP-Session-Id"),
		AuthZ:     r.Header.Get("Authorization"),
	})
}

func (s *streamableHTTPTestServer) snapshot() []httpTestRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]httpTestRequest, len(s.received))
	copy(out, s.received)
	return out
}

func (s *streamableHTTPTestServer) handle(w http.ResponseWriter, r *http.Request) {
	var msg transport.Message
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.record(r, msg.Method)

	switch msg.Method {
	case "initialize":
		s.mu.Lock()
		sid := fmt.Sprintf("upstream-sid-%d", len(s.received))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("MCP-Session-Id", sid)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(transport.Message{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Result: map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake-http-mcp", "version": "0.1"},
			},
		})

	case "notifications/initialized":
		s.mu.Lock()
		status := s.initializedStatus
		s.mu.Unlock()
		if status == 0 {
			status = http.StatusAccepted
		}
		w.WriteHeader(status)

	case "tools/list":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(transport.Message{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Result: map[string]any{
				"tools": []any{
					map[string]any{
						"name":        "echo",
						"description": "echoes input",
						"inputSchema": map[string]any{"type": "object"},
					},
				},
			},
		})

	case "tools/call":
		result := map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "ok"},
			},
		}
		s.mu.Lock()
		useSSE := s.sseOnToolsCall
		s.mu.Unlock()

		if useSSE {
			payload, _ := json.Marshal(transport.Message{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Result:  result,
			})
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(transport.Message{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Result:  result,
			})
		}

	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// newStreamableHTTPTestConfig starts a fake Streamable HTTP MCP server and returns
// its service config and a pointer to the server for inspection.
func newStreamableHTTPTestConfig(t *testing.T, id string) (service.MCPServiceConfig, *streamableHTTPTestServer) {
	t.Helper()
	srv := &streamableHTTPTestServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", srv.handle)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	cfg := service.MCPServiceConfig{
		ID:        id,
		Name:      "fake-http-" + id,
		Transport: service.TransportStreamableHTTP,
		URL:       ts.URL + "/mcp",
	}
	return cfg, srv
}

func TestManagerStreamableHTTPInitialize(t *testing.T) {
	cfg, _ := newStreamableHTTPTestConfig(t, "http1")
	mgr := newInMemoryManager(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	payload, err := mgr.Initialize(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if payload["protocolVersion"] != "2025-11-25" {
		t.Errorf("unexpected protocolVersion: %v", payload["protocolVersion"])
	}
}

func TestManagerStreamableHTTPInitializeAcceptsEmptyOKNotification(t *testing.T) {
	cfg, srv := newStreamableHTTPTestConfig(t, "http1-empty-ok")
	srv.mu.Lock()
	srv.initializedStatus = http.StatusOK
	srv.mu.Unlock()
	mgr := newInMemoryManager(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	payload, err := mgr.Initialize(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if payload["protocolVersion"] != "2025-11-25" {
		t.Errorf("unexpected protocolVersion: %v", payload["protocolVersion"])
	}
}

func TestManagerStreamableHTTPListTools(t *testing.T) {
	cfg, _ := newStreamableHTTPTestConfig(t, "http2")
	mgr := newInMemoryManager(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tools, err := mgr.ListTools(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "echo" {
		t.Errorf("unexpected tool name: %q", tools[0].Name)
	}
	if tools[0].InputSchema["type"] != "object" {
		t.Errorf("unexpected input schema: %#v", tools[0].InputSchema)
	}
}

func TestManagerStreamableHTTPCallTool(t *testing.T) {
	cfg, _ := newStreamableHTTPTestConfig(t, "http3")
	mgr := newInMemoryManager(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := mgr.CallTool(ctx, cfg.ID, "echo", map[string]any{"input": "hello"}, nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result == nil || result.Content == nil {
		t.Fatal("expected non-nil content")
	}
}

// TestManagerStreamableHTTPCallToolSSEResponse verifies that the transport
// correctly decodes a tools/call result delivered as a text/event-stream response.
func TestManagerStreamableHTTPCallToolSSEResponse(t *testing.T) {
	cfg, srv := newStreamableHTTPTestConfig(t, "http4")
	srv.mu.Lock()
	srv.sseOnToolsCall = true
	srv.mu.Unlock()

	mgr := newInMemoryManager(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := mgr.CallTool(ctx, cfg.ID, "echo", nil, nil)
	if err != nil {
		t.Fatalf("CallTool via SSE response: %v", err)
	}
	if result == nil || result.Content == nil {
		t.Fatal("expected non-nil content in SSE-delivered result")
	}
}

func TestManagerStreamableHTTPSessionReuse(t *testing.T) {
	cfg, srv := newStreamableHTTPTestConfig(t, "http5")
	mgr := newInMemoryManager(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := mgr.ListTools(ctx, cfg.ID); err != nil {
		t.Fatalf("first ListTools: %v", err)
	}
	if _, err := mgr.ListTools(ctx, cfg.ID); err != nil {
		t.Fatalf("second ListTools: %v", err)
	}

	// initialize must only be called once across both ListTools invocations.
	reqs := srv.snapshot()
	initCount := 0
	for _, req := range reqs {
		if req.Method == "initialize" {
			initCount++
		}
	}
	if initCount != 1 {
		t.Errorf("expected initialize to be called once, got %d", initCount)
	}
}

// TestManagerStreamableHTTPSessionIDCarried verifies that after the initialize
// handshake the transport sends MCP-Session-Id on every subsequent request.
func TestManagerStreamableHTTPSessionIDCarried(t *testing.T) {
	cfg, srv := newStreamableHTTPTestConfig(t, "http6")
	mgr := newInMemoryManager(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := mgr.ListTools(ctx, cfg.ID); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	reqs := srv.snapshot()

	// All requests after initialize should carry a non-empty MCP-Session-Id.
	seenInit := false
	for _, req := range reqs {
		if req.Method == "initialize" {
			seenInit = true
			continue
		}
		if !seenInit {
			continue
		}
		if req.SessionID == "" {
			t.Errorf("request %q sent after initialize without MCP-Session-Id header", req.Method)
		}
	}
	if !seenInit {
		t.Fatal("initialize was never called")
	}
}

// TestManagerStreamableHTTPAuthHeader verifies that a configured API key is
// forwarded as an Authorization header on every request, including initialize.
func TestManagerStreamableHTTPAuthHeader(t *testing.T) {
	cfg, srv := newStreamableHTTPTestConfig(t, "http7")
	cfg.AuthConfig = &service.AuthConfig{
		Type:   "bearer",
		APIKey: "test-secret-key",
	}
	mgr := newInMemoryManager(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := mgr.ListTools(ctx, cfg.ID); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	reqs := srv.snapshot()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, req := range reqs {
		if req.AuthZ != "Bearer test-secret-key" {
			t.Errorf("request %q: expected Authorization 'Bearer test-secret-key', got %q", req.Method, req.AuthZ)
		}
	}
}
