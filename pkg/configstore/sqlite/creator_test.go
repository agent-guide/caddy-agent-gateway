package sqlite

import (
	"context"
	"testing"

	"github.com/agent-guide/agent-gateway/internal/observability/usage"
	configstore "github.com/agent-guide/agent-gateway/pkg/configstore"
	"github.com/agent-guide/agent-gateway/pkg/configstore/schema"
)

func TestRegisterDefaultStoresRegistersAllSchemas(t *testing.T) {
	t.Helper()

	creator, err := Open(context.Background(), Config{
		SQLitePath: t.TempDir() + "/configstore.db",
	}, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	backend := configstore.NewBackend(creator)
	if err := schema.RegisterDefaultStores(backend); err != nil {
		t.Fatalf("register default stores: %v", err)
	}

	for _, name := range []string{
		schema.StoreProviders,
		schema.StoreCredentials,
		schema.StoreRoutes,
		schema.StoreVirtualKeys,
		schema.StoreManagedModels,
	} {
		if _, err := backend.Get(name); err != nil {
			t.Fatalf("build registered store %q: %v", name, err)
		}
	}
}

// TestBackendForwardsUsageDB guards the metrics wiring: the generic Backend
// wrapper must forward the creator's usage.SQLDBProvider capability, otherwise
// the gateway silently falls back to a no-op usage observer and never persists
// LLM/MCP/ACP usage events.
func TestBackendForwardsUsageDB(t *testing.T) {
	creator, err := Open(context.Background(), Config{
		SQLitePath: t.TempDir() + "/configstore.db",
	}, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	var backend configstore.ConfigStoreBackend = configstore.NewBackend(creator)
	provider, ok := backend.(usage.SQLDBProvider)
	if !ok {
		t.Fatalf("generic backend does not satisfy usage.SQLDBProvider")
	}
	if provider.UsageDB() == nil {
		t.Fatalf("forwarded UsageDB returned nil")
	}
}
