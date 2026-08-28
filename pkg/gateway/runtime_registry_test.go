package gateway

import (
	"errors"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/agent"
	agentruntime "github.com/agent-guide/agent-gateway/pkg/agent/runtime"
	"github.com/agent-guide/agent-gateway/pkg/agent/runtime/runtimetest"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
	configschema "github.com/agent-guide/agent-gateway/pkg/configstore/schema"
	configstoresqlite "github.com/agent-guide/agent-gateway/pkg/configstore/sqlite"
)

func TestAgentGatewayOwnsRuntimeRegistry(t *testing.T) {
	t.Parallel()

	gateway := NewAgentGateway()
	if gateway.RuntimeRegistry() == nil {
		t.Fatal("RuntimeRegistry() = nil")
	}
}

func TestAgentGatewayBootstrapRegistersLinkedNativeBackends(t *testing.T) {
	ctx := t.Context()
	store, err := configstore.OpenBackend(ctx, "sqlite", configstoresqlite.Config{SQLitePath: t.TempDir() + "/config.db"}, nil)
	if err != nil {
		t.Fatalf("OpenBackend: %v", err)
	}
	if err := configschema.RegisterDefaultStores(store); err != nil {
		t.Fatalf("RegisterDefaultStores: %v", err)
	}
	gateway := NewAgentGateway()
	if err := gateway.Bootstrap(ctx, BootstrapOptions{ConfigStoreBackend: store}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	got := gateway.RuntimeRegistry().RuntimeTypes()
	if len(got) != 2 || got[0] != agent.RuntimeTypeACP || got[1] != agent.RuntimeTypeBuiltin {
		t.Fatalf("runtime types = %v, want [acp builtin]", got)
	}
}

func TestAgentGatewayResetReplacesRuntimeRegistry(t *testing.T) {
	t.Parallel()

	gateway := NewAgentGateway()
	backend := runtimetest.NewBackend("fake")
	if err := gateway.RuntimeRegistry().Register(backend); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	old := gateway.RuntimeRegistry()
	gateway.Reset()
	current := gateway.RuntimeRegistry()
	if current == nil || current == old {
		t.Fatal("Reset() did not install a fresh runtime registry")
	}
	if _, err := current.Resolve("fake"); !errors.Is(err, agentruntime.ErrRuntimeNotExecutable) {
		t.Fatalf("Resolve(fake) after Reset error = %v", err)
	}
}
