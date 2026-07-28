package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	baseacp "github.com/agent-guide/agent-gateway/pkg/acp"
	acpruntime "github.com/agent-guide/agent-gateway/pkg/acp/runtime"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	agentpkg "github.com/agent-guide/agent-gateway/pkg/agent"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	configschema "github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	configstoresqlite "github.com/agent-guide/agent-gateway/pkg/configstore/sqlite"
	"github.com/agent-guide/agent-gateway/pkg/gateway"
	"go.uber.org/zap"
)

type adminRetireRecordingRuntime struct {
	mu          sync.Mutex
	retirements []string
}

func (*adminRetireRecordingRuntime) ServeConfiguredTurn(context.Context, string, acpruntime.RuntimeConfig, acpruntime.TurnRequest, acpruntime.EventSink) error {
	return nil
}

func (r *adminRetireRecordingRuntime) RetireOwnerDeferred(ownerID, keep string) (int, func()) {
	r.mu.Lock()
	r.retirements = append(r.retirements, ownerID+"|"+keep)
	r.mu.Unlock()
	return 0, func() {}
}

func (r *adminRetireRecordingRuntime) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.retirements...)
}

func TestUpdateACPServiceRecommitsAgentConfigsAndRetiresOwner(t *testing.T) {
	t.Skip("legacy ACP service surface removed by unified Agent runtime M5")
	ctx := t.Context()
	backend, err := configstore.OpenBackend(ctx, "sqlite", configstoresqlite.Config{SQLitePath: t.TempDir() + "/config.db"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := configschema.RegisterDefaultStores(backend); err != nil {
		t.Fatal(err)
	}
	serviceStore, err := backend.Get(configschema.StoreACPServices)
	if err != nil {
		t.Fatal(err)
	}
	agentStore, err := backend.Get(configschema.StoreAgents)
	if err != nil {
		t.Fatal(err)
	}
	services := acpservice.NewManager(serviceStore)
	agents := agentpkg.NewManager(agentStore)
	cwd := t.TempDir()
	service := acpservice.ServiceConfig{ID: "svc-recommit", Name: "Service", AgentType: baseacp.AgentTypeOpencode, CWD: cwd, AllowedRoots: []string{cwd}, DefaultModel: "model-a"}
	if err := services.Create(ctx, service); err != nil {
		t.Fatal(err)
	}
	native := &adminRetireRecordingRuntime{}
	acpBackend := gateway.NewACPBackend(native)
	agents.AddDefinitionListener(acpBackend.PrepareRuntimeConfigs)
	if err := agents.Create(ctx, agentpkg.Agent{ID: "agent-recommit", Name: "Agent", Runtime: agentpkg.Runtime{Type: agentpkg.RuntimeTypeACP, ACP: &agentpkg.ACPRuntime{ServiceID: service.ID}}}); err != nil {
		t.Fatal(err)
	}
	initial := native.snapshot()
	if len(initial) == 0 || initial[len(initial)-1] == "agent-recommit|" {
		t.Fatalf("initial accepted fingerprint was not established: %v", initial)
	}

	service.DefaultModel = "model-b"
	body, err := json.Marshal(service)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{sharedACPServiceManager: services, agentManager: agents, logger: zap.NewNop()}
	req := httptest.NewRequest(http.MethodPut, "/admin/acp/services/"+service.ID, bytes.NewReader(body))
	req.SetPathValue("id", service.ID)
	rec := httptest.NewRecorder()
	h.handleUpdateACPService(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := native.snapshot()
	if len(got) != len(initial)+1 || got[len(got)-1] == initial[len(initial)-1] || got[len(got)-1] == "agent-recommit|" {
		t.Fatalf("Admin update did not recommit a new accepted fingerprint: before=%v after=%v", initial, got)
	}
}
