package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/adminclient"
	"github.com/agent-guide/agent-gateway/pkg/gateway/agentroute"
	"github.com/agent-guide/agent-gateway/pkg/gatewaybundle"
)

func TestApplyAgentRoutesStripsExportedManagedTimestamps(t *testing.T) {
	created := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/agents/routes":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/admin/agents/routes":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if _, ok := body["created_at"]; ok {
				t.Fatalf("create body retained created_at: %#v", body)
			}
			if _, ok := body["updated_at"]; ok {
				t.Fatalf("create body retained updated_at: %#v", body)
			}
			created = true
			w.WriteHeader(http.StatusCreated)
			body["source"] = "store"
			body["read_only"] = false
			_ = json.NewEncoder(w).Encode(body)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	now := time.Now().UTC()
	bundle := &gatewaybundle.GatewayBundle{AgentRoutes: []agentroute.AgentRouteConfig{{
		AgentRouteBaseConfig: agentroute.AgentRouteBaseConfig{
			ID: "agent:reviewer:review", MatchPolicy: agentroute.RouteMatch{PathPrefix: "/review"},
			CreatedAt: now, UpdatedAt: now,
		},
		AgentID: "reviewer",
	}}}
	client := adminclient.New(adminclient.Config{BaseURL: srv.URL})
	var applyErr error
	if err := applyAgentRoutes(context.Background(), client, bundle, func(_, _, _ string, err error) { applyErr = err }); err != nil {
		t.Fatal(err)
	}
	if applyErr != nil {
		t.Fatal(applyErr)
	}
	if !created {
		t.Fatal("AgentRoute create was not called")
	}
}
