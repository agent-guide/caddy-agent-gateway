package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayBuiltinRouteCommands(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":    "test-token",
				"username": "admin",
			})
		case "/admin/builtin/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{
						"id":           "builtin-helper",
						"kind":         "builtin",
						"protocol":     "builtin",
						"agent_id":     "helper-agent",
						"match_policy": map[string]any{"path_prefix": "/agents/helper"},
						"auth_policy":  map[string]any{"require_virtual_key": false},
						"source":       "store",
					},
				},
			})
		case "/admin/builtin/routes/builtin-helper":
			if r.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":       "builtin-helper",
				"kind":     "builtin",
				"protocol": "builtin",
				"agent_id": "helper-agent",
				"source":   "store",
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	gatewayArgs := func(args ...string) []string {
		return append([]string{
			"gateway",
			"--admin-addr", srv.URL,
			"--admin-basic-auth", "admin:secret",
		}, args...)
	}

	stdout, stderr, err := executeAGWCTL(t, gatewayArgs("builtin-route", "list")...)
	if err != nil {
		t.Fatalf("gateway builtin-route list: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "AGENT-ID") || !strings.Contains(stdout, "/agents/helper") {
		t.Fatalf("stdout missing builtin route table data:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(t, append([]string{"--output", "json"}, gatewayArgs("builtin-route", "get", "builtin-helper")...)...)
	if err != nil {
		t.Fatalf("gateway builtin-route get: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, `"id": "builtin-helper"`) {
		t.Fatalf("stdout missing builtin route id:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"agent_id": "helper-agent"`) {
		t.Fatalf("stdout missing builtin route agent id:\n%s", stdout)
	}
}

func TestGatewayBuiltinRuntimeCommands(t *testing.T) {
	var cancelURL string
	var cancelMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token", "username": "admin"})
		case r.URL.Path == "/admin/builtin/runtime/inflight":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"agent_id": "helper-agent", "session_id": "sess-1", "operation": "turn", "topology_kind": "single"},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/admin/builtin/runtime/turns/"):
			cancelURL = r.URL.String()
			cancelMethod = r.Method
			_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": true, "mode": "graceful"})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	gatewayArgs := func(args ...string) []string {
		return append([]string{"gateway", "--admin-addr", srv.URL, "--admin-basic-auth", "admin:secret"}, args...)
	}

	stdout, stderr, err := executeAGWCTL(t, append([]string{"--output", "json"}, gatewayArgs("builtin-runtime", "inflight")...)...)
	if err != nil {
		t.Fatalf("gateway builtin-runtime inflight: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, `"session_id": "sess-1"`) {
		t.Fatalf("stdout missing in-flight turn:\n%s", stdout)
	}

	stdout, stderr, err = executeAGWCTL(t, append([]string{"--output", "json"}, gatewayArgs("builtin-runtime", "cancel-turn", "helper-agent", "sess-1", "--mode", "graceful")...)...)
	if err != nil {
		t.Fatalf("gateway builtin-runtime cancel-turn: %v\nstderr=%s", err, stderr)
	}
	if cancelMethod != http.MethodDelete {
		t.Fatalf("cancel method = %q, want DELETE", cancelMethod)
	}
	if !strings.Contains(cancelURL, "/admin/builtin/runtime/turns/helper-agent/sess-1") || !strings.Contains(cancelURL, "mode=graceful") {
		t.Fatalf("cancel URL = %q, want the turn path with mode=graceful", cancelURL)
	}
	if !strings.Contains(stdout, `"cancelled": true`) {
		t.Fatalf("stdout missing cancel result:\n%s", stdout)
	}
}
