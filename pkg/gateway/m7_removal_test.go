package gateway

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestM7RemovedRuntimeSpecificSource prevents the deleted Agent ingress and ACP
// service management concepts from returning to active Go source. The bundle
// and SQLite preflights are deliberately excluded: they recognize old data
// only to reject it with the offline-migration error.
func TestM7RemovedRuntimeSpecificSource(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	for _, rel := range []string{"pkg/gateway/acproute", "pkg/gateway/builtinroute", "pkg/acp/service"} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Fatalf("removed source directory still exists: %s", rel)
		}
	}

	forbidden := []string{
		"ACPRoute", "BuiltinRoute", "acpRoutes", "builtinRoutes",
		"acpServices", "acp_services", "acp-service", "/admin/acp/services",
		"runtime.acp.service_id", "pkg/acp/service", "acpservice",
		"ACPServiceID", "OwnsService", "EnableACP", "EnableBuiltin",
		"acp_route_ids", "builtin_route_ids",
		"assembleACPWorkspace", "acpRoutesForService", "builtinRoutesForAgent",
		"ListACPServices", "ListACPRoutes", "/admin/builtin/runtime/turns",
	}
	// These files recognize only the listed legacy shapes to migrate or reject
	// them. Token-level exceptions keep every other identifier in test source
	// covered by this gate.
	allowed := map[string]map[string]bool{
		"cmd/agwd/legacy_store_integration_test.go": {
			"acp_services": true,
		},
		"pkg/admin/routes_test.go": {
			"/admin/acp/services": true,
		},
		"pkg/configstore/sqlite/legacy_agent_runtime.go": {
			"acp_services": true, "runtime.acp.service_id": true,
		},
		"pkg/configstore/sqlite/legacy_agent_runtime_test.go": {
			"acp_services": true, "runtime.acp.service_id": true,
		},
		"pkg/configstore/sqlite/usage_test.go": {
			"ACPServiceID": true, "acp_services": true,
		},
		"pkg/gatewaybundle/bundle.go": {
			"acpServices": true, "acpRoutes": true, "builtinRoutes": true,
			"runtime.acp.service_id": true,
		},
		"pkg/gatewaybundle/bundle_test.go": {
			"acpServices": true,
		},
		"scripts/migrate-unified-agent-runtime-go/main.go": {
			"acpServices": true, "acpRoutes": true, "builtinRoutes": true,
			"acp-service": true, "acp_route_ids": true, "builtin_route_ids": true,
		},
		"scripts/migrate-unified-agent-runtime-go/main_test.go": {
			"ACPRoute": true, "BuiltinRoute": true, "acpServices": true,
			"acpRoutes": true, "builtinRoutes": true, "acp_route_ids": true,
		},
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			if rel == ".git" || rel == "vendor" || strings.HasPrefix(rel, "vendor"+string(filepath.Separator)) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || rel == "pkg/gateway/m7_removal_test.go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, token := range forbidden {
			if strings.Contains(string(data), token) && !allowed[rel][token] {
				t.Errorf("removed identifier %q remains in %s", token, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	assertSourceContains(t, root, "standalone/server/server.go", "EnableAgent: true")
	assertSourceContains(t, root, "caddy/dispatcher/caddyfile.go", `case "agent":`)
	assertSourceContains(t, root, "caddy/dispatcher/dispatcher.go", `json:"agent,omitempty"`)
}

func assertSourceContains(t *testing.T, root, rel, token string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), token) {
		t.Errorf("required Agent dispatcher wiring %q missing from %s", token, rel)
	}
}
