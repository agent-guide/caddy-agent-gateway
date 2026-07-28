package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayAgentRouteCommands(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token", "username": "admin"})
		case "/admin/agents/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{
				"id": "assistant", "kind": "agent", "protocol": "agent", "agent_id": "assistant",
				"match_policy": map[string]any{"path_prefix": "/agents/assistant"}, "source": "store",
			}}})
		case "/admin/agents/routes/assistant":
			if r.Method == http.MethodDelete {
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "assistant", "kind": "agent", "protocol": "agent", "agent_id": "assistant", "source": "store",
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	args := func(parts ...string) []string {
		return append([]string{"gateway", "--admin-addr", srv.URL, "--admin-basic-auth", "admin:secret"}, parts...)
	}
	stdout, stderr, err := executeAGWCTL(t, append([]string{"--output", "json"}, args("agent-route", "list")...)...)
	if err != nil {
		t.Fatalf("agent-route list: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, `"agent_id": "assistant"`) {
		t.Fatalf("list output missing Agent target:\n%s", stdout)
	}
	stdout, stderr, err = executeAGWCTL(t, append([]string{"--output", "json"}, args("agent-route", "get", "assistant")...)...)
	if err != nil {
		t.Fatalf("agent-route get: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, `"kind": "agent"`) {
		t.Fatalf("get output missing unified route kind:\n%s", stdout)
	}
	stdout, stderr, err = executeAGWCTL(t, append([]string{"--output", "json"}, args("agent-route", "delete", "assistant")...)...)
	if err != nil {
		t.Fatalf("agent-route delete: %v\nstderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, `"status": "deleted"`) {
		t.Fatalf("delete output missing status:\n%s", stdout)
	}
}
