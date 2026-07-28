package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestMigrateACPAndBuiltinRoutesAndVirtualKeys(t *testing.T) {
	root := map[string]any{
		"agents": []any{
			map[string]any{"id": "reviewer", "runtime": map[string]any{"type": "acp", "acp": map[string]any{"service_id": "svc"}}, "routes": map[string]any{"acp_route_ids": []any{"acp:svc:review"}}},
			map[string]any{"id": "helper", "runtime": map[string]any{"type": "builtin", "builtin": map[string]any{}}},
		},
		"acpServices":   []any{map[string]any{"id": "svc", "agent_type": "codex", "cwd": "/work", "allowed_roots": []any{"/work"}, "disabled": true}},
		"acpRoutes":     []any{map[string]any{"id": "acp:svc:review", "service_id": "svc", "match_policy": map[string]any{"path_prefix": "/review"}, "created_at": "2026-07-28T13:01:28Z", "updated_at": "2026-07-28T13:01:29Z"}},
		"builtinRoutes": []any{map[string]any{"id": "builtin:helper:help", "agent_id": "helper", "match_policy": map[string]any{"path_prefix": "/help"}}},
		"llmRoutes":     []any{map[string]any{"id": "llm-main"}},
		"mcpRoutes":     []any{map[string]any{"id": "mcp-main"}},
		"virtualKeys":   []any{map[string]any{"id": "vk", "allowed_route_ids": []any{"llm-main", "acp:svc:review", "mcp-main", "builtin:helper:help"}}},
	}
	routes, err := migrate(root)
	if err != nil {
		t.Fatal(err)
	}
	if routes["acp:svc:review"] != "agent:reviewer:review" || routes["builtin:helper:help"] != "agent:helper:help" {
		t.Fatalf("route map = %#v", routes)
	}
	if _, ok := root["acpServices"]; ok {
		t.Fatal("acpServices survived")
	}
	agents := objectList(root["agents"])
	acp := object(object(agents[0]["runtime"])["acp"])
	if acp["agent_type"] != "codex" || !boolValue(agents[0]["disabled"]) {
		t.Fatalf("migrated agent = %#v", agents[0])
	}
	refs := stringList(objectList(root["virtualKeys"])[0]["allowed_route_ids"])
	if len(refs) != 4 || refs[0] != "llm-main" || refs[1] != "agent:reviewer:review" || refs[2] != "mcp-main" || refs[3] != "agent:helper:help" {
		t.Fatalf("refs = %#v", refs)
	}
	converted := objectList(root["agentRoutes"])[0]
	if _, ok := converted["created_at"]; ok {
		t.Fatalf("converted route retained created_at: %#v", converted)
	}
	if _, ok := converted["updated_at"]; ok {
		t.Fatalf("converted route retained updated_at: %#v", converted)
	}
}

func TestMigratePreservesExplicitRouteID(t *testing.T) {
	root := map[string]any{
		"agents":        []any{map[string]any{"id": "helper", "runtime": map[string]any{"type": "builtin", "builtin": map[string]any{}}}},
		"builtinRoutes": []any{map[string]any{"id": "custom-help-route", "agent_id": "helper", "match_policy": map[string]any{"path_prefix": "/help"}}},
	}
	routes, err := migrate(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := routes["custom-help-route"]; got != "custom-help-route" {
		t.Fatalf("explicit route mapping = %q, want preserved ID", got)
	}
	if got := stringValue(objectList(root["agentRoutes"])[0]["id"]); got != "custom-help-route" {
		t.Fatalf("migrated route ID = %q, want preserved ID", got)
	}
}

func TestMigratePreservesUnrelatedObjectsSemantically(t *testing.T) {
	unrelated := map[string]any{
		"apiVersion": "gateway.agw/v1alpha1",
		"kind":       "GatewayBundle",
		"providers":  []any{map[string]any{"id": "provider-a", "config": map[string]any{"nested": []any{"one", 2, true}}}},
		"metadata":   map[string]any{"labels": map[string]any{"owner": "platform"}},
	}
	wantProviders := []any{map[string]any{"id": "provider-a", "config": map[string]any{"nested": []any{"one", 2, true}}}}
	wantMetadata := map[string]any{"labels": map[string]any{"owner": "platform"}}

	if _, err := migrate(unrelated); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unrelated["providers"], wantProviders) || !reflect.DeepEqual(unrelated["metadata"], wantMetadata) {
		t.Fatalf("unrelated objects changed: %#v", unrelated)
	}
}

func TestMigrateRejectsRouteIDCollisionAcrossFamilies(t *testing.T) {
	root := map[string]any{
		"agents":        []any{map[string]any{"id": "helper", "runtime": map[string]any{"type": "builtin", "builtin": map[string]any{}}}},
		"builtinRoutes": []any{map[string]any{"id": "shared", "agent_id": "helper", "match_policy": map[string]any{"path_prefix": "/help"}}},
		"llmRoutes":     []any{map[string]any{"id": "shared"}},
	}
	if _, err := migrate(root); err == nil || !strings.Contains(err.Error(), "route ID collision") {
		t.Fatalf("migrate collision error = %v", err)
	}
}

func TestMigrateRejectsOrphanWithoutMutatingRouteOutput(t *testing.T) {
	root := map[string]any{"agents": []any{}, "acpServices": []any{map[string]any{"id": "orphan"}}}
	if _, err := migrate(root); err == nil {
		t.Fatal("expected orphan error")
	}
}

func TestMigrateRejectsMultiplyBoundACPService(t *testing.T) {
	root := map[string]any{
		"agents": []any{
			map[string]any{"id": "one", "runtime": map[string]any{"type": "acp", "acp": map[string]any{"service_id": "shared"}}},
			map[string]any{"id": "two", "runtime": map[string]any{"type": "acp", "acp": map[string]any{"service_id": "shared"}}},
		},
		"acpServices": []any{map[string]any{"id": "shared"}},
	}
	if _, err := migrate(root); err == nil || !strings.Contains(err.Error(), "multiply-bound") {
		t.Fatalf("migrate multiply-bound error = %v", err)
	}
}

func TestMigrateRejectsUnresolvedVirtualKeyRoute(t *testing.T) {
	root := map[string]any{
		"virtualKeys": []any{map[string]any{"id": "vk", "allowed_route_ids": []any{"missing-route"}}},
	}
	if _, err := migrate(root); err == nil || !strings.Contains(err.Error(), `VirtualKey "vk" references unresolved route "missing-route"`) {
		t.Fatalf("migrate unresolved VirtualKey error = %v", err)
	}
}

func TestMigrateRejectsBuiltinRouteWithMissingAgent(t *testing.T) {
	root := map[string]any{
		"builtinRoutes": []any{map[string]any{"id": "builtin:missing:help", "agent_id": "missing", "match_policy": map[string]any{"path_prefix": "/help"}}},
	}
	if _, err := migrate(root); err == nil || !strings.Contains(err.Error(), `references missing agent "missing"`) {
		t.Fatalf("migrate missing builtin Agent error = %v", err)
	}
}
