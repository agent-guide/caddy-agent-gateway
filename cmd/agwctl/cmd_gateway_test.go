package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLocalAndGatewayCLIAuthHelpAreDistinct(t *testing.T) {
	localOut, localErr, err := executeAGWCTL(t, "cliauth", "--help")
	if err != nil {
		t.Fatalf("local cliauth help: %v\nstderr=%s", err, localErr)
	}
	if !strings.Contains(localOut, "gateway CLI auth login flows and local authenticator support") {
		t.Fatalf("local help missing gateway wording:\n%s", localOut)
	}
	if strings.Contains(localOut, "authenticators") {
		t.Fatalf("local help should not expose hidden authenticators command:\n%s", localOut)
	}
	if strings.Contains(localOut, "\n  list") || strings.Contains(localOut, "\n  get") || strings.Contains(localOut, "\n  delete") {
		t.Fatalf("local help still exposes removed credential commands:\n%s", localOut)
	}

	remoteOut, remoteErr, err := executeAGWCTL(t, "gateway", "cliauth", "--help")
	if err != nil {
		t.Fatalf("gateway cliauth help: %v\nstderr=%s", err, remoteErr)
	}
	if !strings.Contains(remoteOut, "remote gateway CLI auth runtime via the admin API") {
		t.Fatalf("gateway help missing remote wording:\n%s", remoteOut)
	}
}

func TestGatewayCredentialListCommandUsesTypeFilterAndDisplaysType(t *testing.T) {
	var gotBasicAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case "/admin/credentials":
			user, pass, ok := r.BasicAuth()
			if ok {
				gotBasicAuth = user + ":" + pass
			}
			if r.URL.Query().Get("type") != "cliauth_token" {
				t.Fatalf("type query = %q, want cliauth_token", r.URL.Query().Get("type"))
			}
			if r.URL.Query().Get("provider_type") != "openai" {
				t.Fatalf("provider_type query = %q, want openai", r.URL.Query().Get("provider_type"))
			}
			if r.URL.Query().Get("provider_id") != "openai-main" {
				t.Fatalf("provider_id query = %q, want openai-main", r.URL.Query().Get("provider_id"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":            "cred-1",
						"provider_type": "openai",
						"provider_id":   "openai-main",
						"type":          "cliauth_token",
					},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"credential", "list",
		"--type", "cliauth_token",
		"--provider-type", "openai",
		"--provider-id", "openai-main",
	)
	if err != nil {
		t.Fatalf("gateway credential list: %v\nstderr=%s", err, stderr)
	}
	if gotBasicAuth != "admin:secret" {
		t.Fatalf("Basic Auth = %q, want admin:secret", gotBasicAuth)
	}
	if !strings.Contains(stdout, "TYPE") || !strings.Contains(stdout, "cliauth_token") {
		t.Fatalf("stdout missing type column or value:\n%s", stdout)
	}
}

func TestGatewayCredentialListCommandSurfacesAdminAuthErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/credentials":
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "invalid credentials",
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:wrong-secret",
		"credential", "list",
		"--type", "cliauth_token",
	)
	if err == nil {
		t.Fatalf("expected admin auth error, got nil\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "admin API error 401: invalid credentials") {
		t.Fatalf("stderr = %q, want 401 auth error", stderr)
	}
	if strings.Contains(stdout, "no credentials found") {
		t.Fatalf("stdout should not report empty credentials on auth error:\n%s", stdout)
	}
}

func TestGatewayProviderTypesListCommand(t *testing.T) {
	var gotBasicAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case "/admin/llm/provider_types":
			user, pass, ok := r.BasicAuth()
			if ok {
				gotBasicAuth = user + ":" + pass
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"provider_type": "openai", "enabled": true},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"provider-type", "list",
	)
	if err != nil {
		t.Fatalf("provider-type list: %v\nstderr=%s", err, stderr)
	}
	if gotBasicAuth != "admin:secret" {
		t.Fatalf("Basic Auth = %q, want admin:secret", gotBasicAuth)
	}
	if !strings.Contains(stdout, `"provider_type": "openai"`) {
		t.Fatalf("stdout missing provider type:\n%s", stdout)
	}
}

func TestGatewayCLIAuthAuthenticatorsListCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case "/admin/cliauth/authenticators":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"name":          "codex",
						"provider_type": "openai",
						"enabled":       true,
						"config": map[string]any{
							"no_browser": true,
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"cliauth", "authenticators", "list",
	)
	if err != nil {
		t.Fatalf("gateway cliauth authenticators list: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, `"name": "codex"`) {
		t.Fatalf("stdout missing authenticator:\n%s", stdout)
	}
}

func TestGatewayAgentP1ReadCommand(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		switch r.URL.Path {
		case "/admin/agents/coding-agent/health":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agent_id": "coding-agent",
				"runtime":  "acp",
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"agent", "health", "coding-agent",
	)
	if err != nil {
		t.Fatalf("gateway agent health: %v\nstderr=%s", err, stderr)
	}
	if gotPath != "/admin/agents/coding-agent/health" {
		t.Fatalf("path = %q, want /admin/agents/coding-agent/health", gotPath)
	}
	if !strings.Contains(stdout, `"agent_id": "coding-agent"`) {
		t.Fatalf("stdout missing agent health payload:\n%s", stdout)
	}
}

func TestGatewayMCPServiceListCommand(t *testing.T) {
	var gotBasicAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case "/admin/acp/services", "/admin/acp/routes", "/admin/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case "/admin/mcp/services":
			user, pass, ok := r.BasicAuth()
			if ok {
				gotBasicAuth = user + ":" + pass
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":        "svc-1",
						"name":      "My MCP Service",
						"transport": "streamable_http",
						"url":       "https://example.com/mcp",
						"disabled":  false,
						"source":    "config_store",
					},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-service", "list",
	)
	if err != nil {
		t.Fatalf("gateway mcp-service list: %v\nstderr=%s", err, stderr)
	}
	if gotBasicAuth != "admin:secret" {
		t.Fatalf("Basic Auth = %q, want admin:secret", gotBasicAuth)
	}
	if !strings.Contains(stdout, "TRANSPORT") || !strings.Contains(stdout, "My MCP Service") {
		t.Fatalf("stdout missing mcp service table data:\n%s", stdout)
	}
}

func TestGatewayMCPServiceGetAndDeleteCommands(t *testing.T) {
	var deleteCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case "/admin/mcp/services/svc-1":
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":        "svc-1",
					"name":      "My MCP Service",
					"transport": "stdio",
					"command":   "npx",
					"source":    "config_store",
				})
			case http.MethodDelete:
				deleteCalled.Store(true)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "deleted"})
			default:
				t.Fatalf("unexpected method: %s", r.Method)
			}
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-service", "get", "svc-1",
	)
	if err != nil {
		t.Fatalf("gateway mcp-service get: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, `"id": "svc-1"`) {
		t.Fatalf("stdout missing mcp service id:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-service", "delete", "svc-1",
	)
	if err != nil {
		t.Fatalf("gateway mcp-service delete: %v\nstderr=%s", err, stderr)
	}
	if !deleteCalled.Load() {
		t.Fatal("expected mcp service delete request")
	}
	if !strings.Contains(stdout, `"status": "deleted"`) {
		t.Fatalf("stdout missing deleted status:\n%s", stdout)
	}
}

func TestGatewayMCPServiceInteractionCommands(t *testing.T) {
	var toolCallBody []byte
	var promptGetBody []byte
	var resourceReadBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case "/admin/mcp/services/svc-1/sessions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"session": map[string]any{
					"id":                  "sess-1",
					"service_id":          "svc-1",
					"upstream_session_id": "abcdef12****",
					"transport":           "streamable_http",
					"state":               "ready",
					"created_at":          "2026-05-22T10:00:00Z",
					"last_used_at":        "2026-05-22T10:05:00Z",
				},
			})
		case "/admin/mcp/services/svc-1/capabilities":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities": map[string]any{
					"tools": map[string]any{
						"listChanged": true,
					},
				},
				"serverInfo": map[string]any{
					"name":    "filesystem",
					"version": "1.0.0",
				},
				"instructions": "Use tools for filesystem access.",
			})
		case "/admin/mcp/services/svc-1/tools":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"name":        "echo",
						"description": "Echo input",
						"inputSchema": map[string]any{
							"type": "object",
						},
					},
				},
			})
		case "/admin/mcp/services/svc-1/tools/call":
			var err error
			toolCallBody, err = io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(tool call body) error = %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"structuredContent": map[string]any{
					"message": "ok",
				},
				"isError": false,
			})
		case "/admin/mcp/services/svc-1/resources":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"uri":         "file:///tmp/example.txt",
						"name":        "example.txt",
						"mimeType":    "text/plain",
						"description": "Example file",
					},
				},
			})
		case "/admin/mcp/services/svc-1/resource-templates":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"name":        "by-path",
						"title":       "Path Reader",
						"uriTemplate": "file:///{path}",
						"mimeType":    "text/plain",
						"description": "Read a file by path",
					},
				},
			})
		case "/admin/mcp/services/svc-1/resources/read":
			var err error
			resourceReadBody, err = io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(resource read body) error = %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"contents": []map[string]any{
					{
						"uri":      "file:///tmp/example.txt",
						"mimeType": "text/plain",
						"text":     "hello",
					},
				},
			})
		case "/admin/mcp/services/svc-1/prompts":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"name":        "summarize",
						"description": "Summarize content",
					},
				},
			})
		case "/admin/mcp/services/svc-1/prompts/get":
			var err error
			promptGetBody, err = io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll(prompt get body) error = %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"description": "Prompt",
				"messages": []map[string]any{
					{
						"role":    "user",
						"content": "hello",
					},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-service", "session", "svc-1",
	)
	if err != nil {
		t.Fatalf("gateway mcp-service session: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "SESSION-ID") || !strings.Contains(stdout, "sess-1") {
		t.Fatalf("stdout missing mcp session table data:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-service", "capabilities", "svc-1",
	)
	if err != nil {
		t.Fatalf("gateway mcp-service capabilities: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, `"protocolVersion": "2025-11-25"`) || !strings.Contains(stdout, `"name": "filesystem"`) {
		t.Fatalf("stdout missing mcp capabilities payload:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-service", "tools", "svc-1",
	)
	if err != nil {
		t.Fatalf("gateway mcp-service tools: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "NAME") || !strings.Contains(stdout, "echo") {
		t.Fatalf("stdout missing mcp tools table data:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-service", "tool-call", "svc-1", "echo",
		"--arguments", `{"input":"hello"}`,
	)
	if err != nil {
		t.Fatalf("gateway mcp-service tool-call: %v\nstderr=%s", err, stderr)
	}
	if got := strings.TrimSpace(string(toolCallBody)); got != `{"name":"echo","arguments":{"input":"hello"}}` {
		t.Fatalf("tool call body = %s", got)
	}
	if !strings.Contains(stdout, `"message": "ok"`) {
		t.Fatalf("stdout missing tool call result:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-service", "resources", "svc-1",
	)
	if err != nil {
		t.Fatalf("gateway mcp-service resources: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "URI") || !strings.Contains(stdout, "example.txt") {
		t.Fatalf("stdout missing mcp resources table data:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-service", "resource-templates", "svc-1",
	)
	if err != nil {
		t.Fatalf("gateway mcp-service resource-templates: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "URI-TEMPLATE") || !strings.Contains(stdout, "file:///{path}") {
		t.Fatalf("stdout missing mcp resource template table data:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-service", "resource-read", "svc-1", "file:///tmp/example.txt",
	)
	if err != nil {
		t.Fatalf("gateway mcp-service resource-read: %v\nstderr=%s", err, stderr)
	}
	if got := strings.TrimSpace(string(resourceReadBody)); got != `{"uri":"file:///tmp/example.txt"}` {
		t.Fatalf("resource read body = %s", got)
	}
	if !strings.Contains(stdout, `"text": "hello"`) {
		t.Fatalf("stdout missing resource read result:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-service", "prompts", "svc-1",
	)
	if err != nil {
		t.Fatalf("gateway mcp-service prompts: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "NAME") || !strings.Contains(stdout, "summarize") {
		t.Fatalf("stdout missing mcp prompts table data:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-service", "prompt-get", "svc-1", "summarize",
		"--arguments", `{"topic":"logs"}`,
	)
	if err != nil {
		t.Fatalf("gateway mcp-service prompt-get: %v\nstderr=%s", err, stderr)
	}
	if got := strings.TrimSpace(string(promptGetBody)); got != `{"name":"summarize","arguments":{"topic":"logs"}}` {
		t.Fatalf("prompt get body = %s", got)
	}
	if !strings.Contains(stdout, `"description": "Prompt"`) {
		t.Fatalf("stdout missing prompt get result:\n%s", stdout)
	}
}

func TestGatewayMCPRouteListGetAndDeleteCommands(t *testing.T) {
	var deleteCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case "/admin/mcp/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":         "mcp:svc-1:/mcp",
						"kind":       "mcp",
						"protocol":   "mcp",
						"service_id": "svc-1",
						"match_policy": map[string]any{
							"path_prefix": "/mcp",
						},
						"auth_policy": map[string]any{
							"require_virtual_key": true,
						},
						"source": "store",
					},
				},
			})
		case "/admin/mcp/routes/mcp:svc-1:/mcp":
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":         "mcp:svc-1:/mcp",
					"kind":       "mcp",
					"protocol":   "mcp",
					"service_id": "svc-1",
					"match_policy": map[string]any{
						"path_prefix": "/mcp",
					},
					"auth_policy": map[string]any{
						"require_virtual_key": true,
					},
					"source": "store",
				})
			case http.MethodDelete:
				deleteCalled.Store(true)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "deleted"})
			default:
				t.Fatalf("unexpected method: %s", r.Method)
			}
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-route", "list",
	)
	if err != nil {
		t.Fatalf("gateway mcp-route list: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "SERVICE-ID") || !strings.Contains(stdout, "/mcp") {
		t.Fatalf("stdout missing mcp route table data:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-route", "get", "mcp:svc-1:/mcp",
	)
	if err != nil {
		t.Fatalf("gateway mcp-route get: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, `"service_id": "svc-1"`) {
		t.Fatalf("stdout missing mcp route service_id:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-route", "delete", "mcp:svc-1:/mcp",
	)
	if err != nil {
		t.Fatalf("gateway mcp-route delete: %v\nstderr=%s", err, stderr)
	}
	if !deleteCalled.Load() {
		t.Fatal("expected mcp route delete request")
	}
	if !strings.Contains(stdout, `"status": "deleted"`) {
		t.Fatalf("stdout missing deleted status:\n%s", stdout)
	}
}

func TestGatewayMCPRuntimeCommands(t *testing.T) {
	var gotHistoryQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case "/admin/mcp/runtime":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"in_flight": []map[string]any{
					{
						"route_id":       "route-a",
						"request_id":     "req-1",
						"request_key":    "route-a\u0000\"req-1\"",
						"method":         "tools/call",
						"progress_token": "progress-1",
						"started_at":     "2026-05-22T10:00:00Z",
					},
				},
				"progress": []map[string]any{
					{
						"route_id":           "route-a",
						"progress_token":     "progress-1",
						"progress_token_key": "route-a\u0000\"progress-1\"",
						"request_id":         "req-1",
						"request_key":        "route-a\u0000\"req-1\"",
						"progress":           3,
						"total":              10,
						"message":            "working",
						"last_method":        "tools/call",
						"updated_at":         "2026-05-22T10:00:01Z",
					},
				},
			})
		case "/admin/mcp/runtime/inflight":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"route_id":       "route-a",
						"request_id":     "req-1",
						"request_key":    "route-a\u0000\"req-1\"",
						"method":         "tools/call",
						"progress_token": "progress-1",
						"started_at":     "2026-05-22T10:00:00Z",
					},
				},
			})
		case "/admin/mcp/runtime/progress":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"route_id":           "route-a",
						"progress_token":     "progress-1",
						"progress_token_key": "route-a\u0000\"progress-1\"",
						"request_id":         "req-1",
						"request_key":        "route-a\u0000\"req-1\"",
						"progress":           3,
						"total":              10,
						"message":            "working",
						"last_method":        "tools/call",
						"updated_at":         "2026-05-22T10:00:01Z",
					},
				},
			})
		case "/admin/mcp/runtime/history":
			gotHistoryQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"route_id":      "route-a",
						"request_id":    "req-1",
						"request_key":   "route-a\u0000\"req-1\"",
						"method":        "tools/call",
						"started_at":    "2026-05-22T10:00:00Z",
						"completed_at":  "2026-05-22T10:00:03Z",
						"cancelled":     false,
						"cancel_reason": "",
						"error":         "",
					},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-runtime", "get",
	)
	if err != nil {
		t.Fatalf("gateway mcp-runtime get: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "ROUTE-ID") || !strings.Contains(stdout, "working") {
		t.Fatalf("stdout missing runtime overview data:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-runtime", "inflight",
	)
	if err != nil {
		t.Fatalf("gateway mcp-runtime inflight: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "tools/call") {
		t.Fatalf("stdout missing inflight data:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-runtime", "progress",
	)
	if err != nil {
		t.Fatalf("gateway mcp-runtime progress: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "PROGRESS") || !strings.Contains(stdout, "working") {
		t.Fatalf("stdout missing progress data:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"mcp-runtime", "history",
		"--route-id", "route-a",
	)
	if err != nil {
		t.Fatalf("gateway mcp-runtime history: %v\nstderr=%s", err, stderr)
	}
	if gotHistoryQuery != "route_id=route-a" {
		t.Fatalf("history query = %q, want route_id=route-a", gotHistoryQuery)
	}
	if !strings.Contains(stdout, "COMPLETED-AT") || !strings.Contains(stdout, "route-a") {
		t.Fatalf("stdout missing history data:\n%s", stdout)
	}
}

func TestCLIAuthLoginRequiresAuthenticatorWithClearUsage(t *testing.T) {
	stdout, stderr, err := executeAGWCTL(t, "cliauth", "login")
	if err == nil {
		t.Fatalf("expected login error\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	msg := err.Error()
	if !strings.Contains(msg, "--authenticator is required") {
		t.Fatalf("error = %q, want missing authenticator message", msg)
	}
	if !strings.Contains(msg, "supported authenticators: claudecode, codex, gemini") &&
		!strings.Contains(msg, "supported authenticators: codex, claudecode, gemini") &&
		!strings.Contains(msg, "supported authenticators: gemini, codex, claudecode") {
		t.Fatalf("error = %q, want supported authenticators", msg)
	}
	if !strings.Contains(msg, "Usage:\n  agwctl cliauth login") {
		t.Fatalf("error = %q, want login usage", msg)
	}
}

func TestCLIAuthLoginRejectsUnknownAuthenticatorWithClearUsage(t *testing.T) {
	stdout, stderr, err := executeAGWCTL(t, "cliauth", "login", "--authenticator", "bad-auth")
	if err == nil {
		t.Fatalf("expected login error\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	msg := err.Error()
	if !strings.Contains(msg, `unsupported --authenticator "bad-auth"`) {
		t.Fatalf("error = %q, want unsupported authenticator message", msg)
	}
	if !strings.Contains(msg, "supported authenticators:") {
		t.Fatalf("error = %q, want supported authenticators", msg)
	}
	if !strings.Contains(msg, "Usage:\n  agwctl cliauth login") {
		t.Fatalf("error = %q, want login usage", msg)
	}
}

func TestGatewayValidateCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
providers:
  - id: openai-main
    provider_type: openai
llmRoutes:
  - id: chat-prod
    protocol: openai
    match:
      path_prefix: /
      methods:
        - POST
    auth_policy:
      require_virtual_key: true
    target_policy:
      provider_target:
        provider_id: openai-main
virtualKeys:
  - id: vk-local-test
    allowed_route_ids:
      - chat-prod
`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	stdout, stderr, err := executeAGWCTL(t, "gateway", "validate", "-f", path)
	if err != nil {
		t.Fatalf("gateway validate: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "gateway bundle is valid:") {
		t.Fatalf("stdout missing success message:\n%s", stdout)
	}
}

func TestGatewayValidateCommandAcceptsCodexProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway-codex.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
providers:
  - id: codex-test
    provider_type: codex
llmRoutes:
  - id: code-codex
    protocol: openai
    match:
      path_prefix: /codecodex
      methods:
        - POST
    auth_policy:
      require_virtual_key: true
    target_policy:
      provider_target:
        provider_id: codex-test
virtualKeys:
  - id: vk-local-code-codex
    allowed_route_ids:
      - code-codex
`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	stdout, stderr, err := executeAGWCTL(t, "gateway", "validate", "-f", path)
	if err != nil {
		t.Fatalf("gateway validate: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "gateway bundle is valid:") {
		t.Fatalf("stdout missing success message:\n%s", stdout)
	}
}

func TestGatewayValidateCommandReturnsLocalValidationError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
providers:
  - id: dup
    provider_type: openai
  - id: dup
    provider_type: openai
`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	stdout, stderr, err := executeAGWCTL(t, "gateway", "validate", "-f", path)
	if err == nil {
		t.Fatalf("gateway validate error = nil\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if !strings.Contains(err.Error(), `providers["dup"]: duplicate id`) {
		t.Fatalf("validate error = %v, want duplicate provider id", err)
	}
}

func TestGatewayApplyCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
providers:
  - id: openai-main
    provider_type: openai
    default_model: gpt-4.1
virtualKeys:
  - id: vk-local-test
    allowed_route_ids: []
cliAuthAuthenticators:
  - name: claudecode
    enabled: true
    config:
      no_browser: true
      transport_profile: browser_like_tls
`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var providerUpdated atomic.Bool
	var virtualKeyCreated atomic.Bool
	var authenticatorUpdated atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case r.URL.Path == "/admin/llm/provider_types":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"provider_type": "openai", "enabled": true}, {"provider_type": "codex", "enabled": true}}})
		case r.URL.Path == "/admin/llm/providers" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":            "openai-main",
						"provider_type": "openai",
						"default_model": "gpt-4o-mini",
						"source":        "dynamic",
						"read_only":     false,
					},
				},
			})
		case r.URL.Path == "/admin/llm/providers/openai-main" && r.Method == http.MethodPut:
			providerUpdated.Store(true)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":            "openai-main",
				"provider_type": "openai",
				"default_model": "gpt-4.1",
			})
		case r.URL.Path == "/admin/llm/models/managed":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/llm/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/virtual_keys" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/virtual_keys" && r.Method == http.MethodPost:
			virtualKeyCreated.Store(true)
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode virtual key create request: %v", err)
			}
			if _, ok := req["key"]; ok {
				t.Fatalf("virtual key create request unexpectedly carried key: %#v", req)
			}
			if req["id"] != "vk-local-test" {
				t.Fatalf("virtual key create request id = %#v, want %q", req["id"], "vk-local-test")
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":  "vk-local-test",
				"key": "vk-generated",
			})
		case r.URL.Path == "/admin/cliauth/authenticators" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/acp/services" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/acp/routes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/mcp/services" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/mcp/routes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/cliauth/authenticators/claudecode" && r.Method == http.MethodPut:
			authenticatorUpdated.Store(true)
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode cliauth authenticator request: %v", err)
			}
			config, ok := req["config"].(map[string]any)
			if !ok {
				t.Fatalf("cliauth authenticator request config = %#v, want object", req["config"])
			}
			if got := config["transport_profile"]; got != "browser_like_tls" {
				t.Fatalf("transport_profile = %#v, want browser_like_tls", got)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "enabled",
				"authenticator": map[string]any{
					"name":    "claudecode",
					"enabled": true,
					"config": map[string]any{
						"no_browser":        true,
						"transport_profile": "browser_like_tls",
					},
				},
			})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"apply",
		"-f", path,
	)
	if err != nil {
		t.Fatalf("gateway apply: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if !providerUpdated.Load() {
		t.Fatal("expected provider update request")
	}
	if !virtualKeyCreated.Load() {
		t.Fatal("expected virtual key create request")
	}
	if !authenticatorUpdated.Load() {
		t.Fatal("expected cliauth authenticator update request")
	}
	if !strings.Contains(stdout, `"status": "ok"`) {
		t.Fatalf("stdout missing ok status:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"action": "update"`) {
		t.Fatalf("stdout missing update action:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"action": "create"`) {
		t.Fatalf("stdout missing create action:\n%s", stdout)
	}
}

func TestGatewayApplyCommandSkipsUnchangedObjects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
providers:
  - id: openai-main
    provider_type: openai
    default_model: gpt-4.1
virtualKeys:
  - id: vk-local-test
    allowed_route_ids: []
cliAuthAuthenticators:
  - name: codex
    enabled: false
`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var providerWriteCount atomic.Int32
	var virtualKeyWriteCount atomic.Int32
	var authenticatorWriteCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case r.URL.Path == "/admin/llm/provider_types":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"provider_type": "openai", "enabled": true}, {"provider_type": "codex", "enabled": true}}})
		case r.URL.Path == "/admin/llm/providers" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":            "openai-main",
						"provider_type": "openai",
						"default_model": "gpt-4.1",
						"source":        "dynamic",
						"read_only":     false,
					},
				},
			})
		case r.URL.Path == "/admin/llm/providers/openai-main" && r.Method == http.MethodPut:
			providerWriteCount.Add(1)
			t.Fatalf("unexpected provider update request for unchanged object")
		case r.URL.Path == "/admin/llm/models/managed":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/llm/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/virtual_keys" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":                "vk-local-test",
						"key":               "vk-generated",
						"allowed_route_ids": []string{},
						"source":            "dynamic",
						"read_only":         false,
					},
				},
			})
		case r.URL.Path == "/admin/virtual_keys" && r.Method == http.MethodPost:
			virtualKeyWriteCount.Add(1)
			t.Fatalf("unexpected virtual key create request for unchanged object")
		case r.URL.Path == "/admin/virtual_keys/vk-local-test" && r.Method == http.MethodPut:
			virtualKeyWriteCount.Add(1)
			t.Fatalf("unexpected virtual key update request for unchanged object")
		case r.URL.Path == "/admin/cliauth/authenticators" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"name":          "codex",
						"provider_type": "openai",
						"enabled":       false,
						"config":        map[string]any{},
					},
				},
			})
		case r.URL.Path == "/admin/cliauth/authenticators/codex" && r.Method == http.MethodPut:
			authenticatorWriteCount.Add(1)
			t.Fatalf("unexpected cliauth authenticator update request for unchanged object")
		case r.URL.Path == "/admin/acp/services" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/acp/routes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/mcp/services" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/mcp/routes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"apply",
		"-f", path,
	)
	if err != nil {
		t.Fatalf("gateway apply unchanged: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if providerWriteCount.Load() != 0 || virtualKeyWriteCount.Load() != 0 || authenticatorWriteCount.Load() != 0 {
		t.Fatalf("unexpected writes: provider=%d virtualKey=%d authenticator=%d", providerWriteCount.Load(), virtualKeyWriteCount.Load(), authenticatorWriteCount.Load())
	}
	if !strings.Contains(stdout, `"action": "skip"`) {
		t.Fatalf("stdout missing skip action:\n%s", stdout)
	}
}

func TestGatewayApplyCommandFailsOnReadOnlyObjectDrift(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
providers:
  - id: openai-main
    provider_type: openai
    default_model: gpt-4.1
`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case r.URL.Path == "/admin/llm/provider_types":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"provider_type": "openai", "enabled": true}, {"provider_type": "codex", "enabled": true}}})
		case r.URL.Path == "/admin/llm/providers" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":            "openai-main",
						"provider_type": "openai",
						"default_model": "gpt-4o-mini",
						"source":        "static",
						"read_only":     true,
					},
				},
			})
		case r.URL.Path == "/admin/llm/models/managed":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/llm/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/virtual_keys" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/cliauth/authenticators" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/acp/services" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/acp/routes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/mcp/services" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/mcp/routes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"apply",
		"-f", path,
	)
	if err == nil {
		t.Fatalf("gateway apply read-only drift error = nil\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "gateway apply finished with 1 error") {
		t.Fatalf("apply error = %v", err)
	}
	if !strings.Contains(stdout, `"status": "error"`) {
		t.Fatalf("stdout missing error status:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"error": "provider \"openai-main\" is read-only"`) {
		t.Fatalf("stdout missing read-only provider error:\n%s", stdout)
	}
}

func TestGatewayExportCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case "/admin/llm/provider_types":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"provider_type": "openai", "enabled": true},
				},
			})
		case "/admin/llm/providers":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":            "openai-main",
						"provider_type": "openai",
						"default_model": "gpt-4.1",
						"source":        "dynamic",
						"read_only":     false,
					},
				},
			})
		case "/admin/llm/models/managed":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"provider_id":    "openai-main",
						"upstream_model": "gpt-4.1",
						"enabled":        true,
					},
				},
			})
		case "/admin/llm/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":       "chat-prod",
						"protocol": "openai",
						"match": map[string]any{
							"path_prefix": "/",
							"methods":     []string{"POST"},
						},
						"auth_policy": map[string]any{
							"require_virtual_key": true,
						},
						"target_policy": map[string]any{
							"provider_target": map[string]any{
								"provider_id": "openai-main",
							},
						},
						"source":    "dynamic",
						"read_only": false,
					},
				},
			})
		case "/admin/virtual_keys":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":                "vk-local-test",
						"key":               "vk-local-test",
						"allowed_route_ids": []string{"chat-prod"},
						"source":            "dynamic",
						"read_only":         false,
					},
				},
			})
		case "/admin/cliauth/authenticators":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"name":          "codex",
						"provider_type": "openai",
						"enabled":       true,
						"config": map[string]any{
							"no_browser": true,
						},
					},
				},
			})
		case "/admin/acp/services", "/admin/acp/routes", "/admin/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case "/admin/mcp/services":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case "/admin/mcp/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"export",
	)
	if err != nil {
		t.Fatalf("gateway export: %v\nstderr=%s", err, stderr)
	}
	for _, want := range []string{
		"apiVersion: gateway.agw/v1alpha1",
		"kind: GatewayBundle",
		"providers:",
		"llmRoutes:",
		"virtualKeys:",
		"cliAuthAuthenticators:",
		"id: openai-main",
		"id: vk-local-test",
		"name: codex",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "\n    key: ") || strings.Contains(stdout, "\n  key: ") {
		t.Fatalf("stdout unexpectedly included generated virtual key value:\n%s", stdout)
	}
}

func TestGatewayExportThenValidateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	exportPath := filepath.Join(dir, "exported.gateway.yaml")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case "/admin/llm/provider_types":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"provider_type": "openai", "enabled": true},
				},
			})
		case "/admin/llm/providers":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":            "openai-main",
						"provider_type": "openai",
						"default_model": "gpt-4.1",
						"source":        "dynamic",
						"read_only":     false,
					},
				},
			})
		case "/admin/llm/models/managed":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case "/admin/llm/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":       "chat-prod",
						"protocol": "openai",
						"match": map[string]any{
							"path_prefix": "/",
							"methods":     []string{"POST"},
						},
						"auth_policy": map[string]any{
							"require_virtual_key": true,
						},
						"target_policy": map[string]any{
							"provider_target": map[string]any{
								"provider_id": "openai-main",
							},
						},
						"source":    "dynamic",
						"read_only": false,
					},
				},
			})
		case "/admin/virtual_keys":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":                "vk-local-test",
						"key":               "vk-local-test",
						"allowed_route_ids": []string{"chat-prod"},
						"source":            "dynamic",
						"read_only":         false,
					},
				},
			})
		case "/admin/cliauth/authenticators":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"name":          "codex",
						"provider_type": "openai",
						"enabled":       true,
						"config": map[string]any{
							"device_flow": true,
						},
					},
				},
			})
		case "/admin/acp/services", "/admin/acp/routes", "/admin/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case "/admin/mcp/services":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case "/admin/mcp/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	_, stderr, err := executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"export",
		"-f", exportPath,
	)
	if err != nil {
		t.Fatalf("gateway export: %v\nstderr=%s", err, stderr)
	}

	stdout, stderr, err := executeAGWCTL(t, "gateway", "validate", "-f", exportPath)
	if err != nil {
		t.Fatalf("gateway validate exported file: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "gateway bundle is valid:") {
		t.Fatalf("stdout missing validation success:\n%s", stdout)
	}
}

func TestGatewayExportCommandIncludesMCPServicesAndRoutes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token", "username": "admin"})
		case "/admin/llm/provider_types":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"provider_type": "openai", "enabled": true}, {"provider_type": "codex", "enabled": true}}})
		case "/admin/llm/providers":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case "/admin/llm/models/managed":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case "/admin/llm/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case "/admin/virtual_keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case "/admin/cliauth/authenticators":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case "/admin/acp/services", "/admin/acp/routes", "/admin/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case "/admin/mcp/services":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":        "my-mcp-svc",
						"name":      "My MCP Service",
						"transport": "streamable_http",
						"url":       "https://example.com/mcp",
						"source":    "config_store",
						"read_only": false,
					},
				},
			})
		case "/admin/mcp/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":         "mcp:my-mcp-svc:/mcp",
						"kind":       "mcp",
						"protocol":   "mcp",
						"service_id": "my-mcp-svc",
						"match_policy": map[string]any{
							"path_prefix": "/mcp",
						},
						"auth_policy": map[string]any{
							"require_virtual_key": true,
						},
						"source":    "store",
						"read_only": false,
					},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"export",
	)
	if err != nil {
		t.Fatalf("gateway export: %v\nstderr=%s", err, stderr)
	}
	for _, want := range []string{
		"mcpServices:",
		"id: my-mcp-svc",
		"name: My MCP Service",
		"mcpRoutes:",
		"service_id: my-mcp-svc",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestGatewayApplyCommandCreatesMCPServiceAndRoute(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: gateway.agw/v1alpha1
kind: GatewayBundle
mcpServices:
  - id: my-mcp-svc
    name: My MCP Service
    transport: streamable_http
    url: https://example.com/mcp
mcpRoutes:
  - service_id: my-mcp-svc
    match_policy:
      path_prefix: /mcp
    auth_policy:
      require_virtual_key: true
`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var mcpServiceCreated atomic.Bool
	var mcpRouteCreated atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token", "username": "admin"})
		case r.URL.Path == "/admin/llm/provider_types":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"provider_type": "openai", "enabled": true}, {"provider_type": "codex", "enabled": true}}})
		case r.URL.Path == "/admin/llm/providers" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/llm/models/managed":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/llm/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/virtual_keys" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/cliauth/authenticators" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/acp/services" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/acp/routes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/mcp/services" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/mcp/services" && r.Method == http.MethodPost:
			mcpServiceCreated.Store(true)
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode mcp service create request: %v", err)
			}
			if req["id"] != "my-mcp-svc" {
				t.Fatalf("mcp service create id = %#v, want my-mcp-svc", req["id"])
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(req)
		case r.URL.Path == "/admin/mcp/routes" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
		case r.URL.Path == "/admin/mcp/routes" && r.Method == http.MethodPost:
			mcpRouteCreated.Store(true)
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode mcp route create request: %v", err)
			}
			if req["service_id"] != "my-mcp-svc" {
				t.Fatalf("mcp route create service_id = %#v, want my-mcp-svc", req["service_id"])
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(req)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, err := executeAGWCTL(
		t,
		"--output", "json",
		"gateway",
		"--admin-addr", srv.URL,
		"--admin-basic-auth", "admin:secret",
		"apply",
		"-f", path,
	)
	if err != nil {
		t.Fatalf("gateway apply: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if !mcpServiceCreated.Load() {
		t.Fatal("expected mcp service create request")
	}
	if !mcpRouteCreated.Load() {
		t.Fatal("expected mcp route create request")
	}
	if !strings.Contains(stdout, `"status": "ok"`) {
		t.Fatalf("stdout missing ok status:\n%s", stdout)
	}
}

func executeAGWCTL(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	oldArgs := os.Args
	oldStdout := os.Stdout
	oldStderr := os.Stderr
	oldOutputFormat := outputFormat
	oldGatewayExportFile := gatewayExportFile
	oldGatewayBundleFile := gatewayBundleFile
	defer func() {
		os.Args = oldArgs
		os.Stdout = oldStdout
		os.Stderr = oldStderr
		outputFormat = oldOutputFormat
		gatewayExportFile = oldGatewayExportFile
		gatewayBundleFile = oldGatewayBundleFile
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(oldStdout)
		rootCmd.SetErr(oldStderr)
	}()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stdoutBuf, stdoutR)
		close(stdoutDone)
	}()
	go func() {
		_, _ = io.Copy(&stderrBuf, stderrR)
		close(stderrDone)
	}()

	os.Stdout = stdoutW
	os.Stderr = stderrW
	rootCmd.SetOut(stdoutW)
	rootCmd.SetErr(stderrW)
	rootCmd.SetArgs(args)

	runErr := rootCmd.Execute()

	_ = stdoutW.Close()
	_ = stderrW.Close()
	<-stdoutDone
	<-stderrDone

	return stdoutBuf.String(), stderrBuf.String(), runErr
}
