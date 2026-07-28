package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	baseacp "github.com/agent-guide/agent-gateway/pkg/acp"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	adminpkg "github.com/agent-guide/agent-gateway/pkg/admin"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtimeapi"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	configschema "github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	configstoresqlite "github.com/agent-guide/agent-gateway/pkg/configstore/sqlite"
	dispatcherpkg "github.com/agent-guide/agent-gateway/pkg/dispatcher"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	"github.com/agent-guide/agent-gateway/pkg/gateway/acproute"
)

type countingPermissionBinding struct{ calls atomic.Int32 }

func (*countingPermissionBinding) ValidateContinuationDecision(string, runtimeapi.PendingPermission, runtimeapi.PermissionDecision) error {
	return nil
}
func (b *countingPermissionBinding) ResolveContinuation(context.Context, string, runtimeapi.PermissionDecision, time.Time) error {
	b.calls.Add(1)
	return nil
}
func (*countingPermissionBinding) ExpireContinuation(context.Context, string) error { return nil }

func TestAgentBoundACPPermissionEntryPointsHaveOneAtomicWinner(t *testing.T) {
	t.Skip("legacy ACP permission entrypoint removed by M5; common Agent permission tests cover the winner contract")
	ctx := t.Context()
	backend, err := configstore.OpenBackend(ctx, "sqlite", configstoresqlite.Config{SQLitePath: t.TempDir() + "/config.db"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := configschema.RegisterDefaultStores(backend); err != nil {
		t.Fatal(err)
	}
	gw := gateway.NewAgentGateway()
	defer gw.Close()
	if err := gw.Bootstrap(ctx, gateway.BootstrapOptions{ConfigStoreBackend: backend}); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	service := acpservice.ServiceConfig{ID: "svc-permission", Name: "Permission service", AgentType: baseacp.AgentTypeOpencode, CWD: cwd, AllowedRoots: []string{cwd}, PermissionMode: baseacp.PermissionModeInteractive}
	if err := gw.ACPServiceManager().Create(ctx, service); err != nil {
		t.Fatal(err)
	}
	route := acproute.ACPRouteConfig{AgentRouteConfig: acproute.AgentRouteConfig{MatchPolicy: acproute.RouteMatch{PathPrefix: "/acp-permission"}}, ServiceID: service.ID}
	route.Normalize()
	routeConfig, err := route.ToConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.ACPRouteResolver().CreateConfig(ctx, routeConfig, "test"); err != nil {
		t.Fatal(err)
	}
	agentID := "permission-agent"
	if err := gw.AgentManager().Create(ctx, agentpkg.Agent{
		ID: agentID, Name: "Permission Agent",
		Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeACP, ACP: &agentpkg.ACPRuntime{ServiceID: service.ID}},
		Routes:  agentpkg.Routes{ACPRouteIDs: []string{route.ID}},
	}); err != nil {
		t.Fatal(err)
	}

	binding := &countingPermissionBinding{}
	requestID := "perm-entrypoints"
	if _, err := gw.PermissionBroker().Register(runtimeapi.PendingPermission{
		RequestID: requestID, AgentID: agentID, RuntimeType: agentpkg.RuntimeTypeACP,
		RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExpiresAt: time.Now().Add(time.Minute),
	}, "cont-entrypoints", binding); err != nil {
		t.Fatal(err)
	}

	adminHandler := adminpkg.NewHandler(gw, nil)
	dispatcherHandler := dispatcherpkg.NewHandler(gw, nil, nil, dispatcherpkg.HandlerOptions{})
	type entrypoint struct {
		name string
		h    http.Handler
		path string
	}
	entries := []entrypoint{
		{name: "acp_route", h: dispatcherHandler, path: "/acp-permission/permission"},
		{name: "acp_admin", h: adminHandler, path: "/admin/acp/runtime/permissions/" + requestID},
		{name: "agent_admin", h: adminHandler, path: "/admin/agents/" + agentID + "/permissions/" + requestID},
	}
	start := make(chan struct{})
	statuses := make(chan int, len(entries))
	var wg sync.WaitGroup
	for _, entry := range entries {
		entry := entry
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, entry.path, strings.NewReader(`{"request_id":"`+requestID+`","outcome":"cancelled"}`))
			rec := httptest.NewRecorder()
			entry.h.ServeHTTP(rec, req)
			statuses <- rec.Code
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)
	winners := 0
	for status := range statuses {
		if status == http.StatusOK {
			winners++
		} else if status != http.StatusNotFound {
			t.Fatalf("decision status=%d, want 200 or 404", status)
		}
	}
	if winners != 1 || binding.calls.Load() != 1 {
		t.Fatalf("winners=%d continuation calls=%d, want exactly one", winners, binding.calls.Load())
	}
	audits := gw.PermissionBroker().Audits(agentID)
	sources := map[string]int{}
	for _, audit := range audits {
		sources[audit.Source]++
	}
	for _, entry := range entries {
		if sources[entry.name] != 1 {
			t.Fatalf("audit sources=%v, want one decision from %s", sources, entry.name)
		}
	}
}
