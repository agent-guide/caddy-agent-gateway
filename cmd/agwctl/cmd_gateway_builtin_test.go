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
