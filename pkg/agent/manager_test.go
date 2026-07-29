package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/agent-guide/agent-gateway/pkg/configstore"
)

// testConfigStore is a minimal in-memory ConfigStore for agent manager tests.
type testConfigStore struct {
	mu    sync.RWMutex
	items map[string]*Agent
}

func newTestConfigStore() configstore.ConfigStore {
	return &testConfigStore{items: make(map[string]*Agent)}
}

func (s *testConfigStore) unwrap(obj any) (*Agent, error) {
	if u, ok := obj.(configstore.ObjectUnwrapper); ok {
		obj = u.ConfigStoreObject()
	}
	cfg, ok := obj.(*Agent)
	if !ok {
		return nil, fmt.Errorf("testConfigStore: unexpected type %T", obj)
	}
	return cfg, nil
}

func (s *testConfigStore) Create(_ context.Context, obj any) error {
	cfg, err := s.unwrap(obj)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[cfg.ID]; exists {
		return fmt.Errorf("already exists: %s", cfg.ID)
	}
	cloned := *cfg
	s.items[cfg.ID] = &cloned
	return nil
}

func (s *testConfigStore) Update(_ context.Context, obj any) error {
	cfg, err := s.unwrap(obj)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cloned := *cfg
	s.items[cfg.ID] = &cloned
	return nil
}

func (s *testConfigStore) Delete(_ context.Context, keyParts ...any) error {
	if len(keyParts) == 0 {
		return fmt.Errorf("key required")
	}
	id := fmt.Sprint(keyParts[0])
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
	return nil
}

func (s *testConfigStore) Get(_ context.Context, keyParts ...any) (any, error) {
	if len(keyParts) == 0 {
		return nil, fmt.Errorf("key required")
	}
	id := fmt.Sprint(keyParts[0])
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg, ok := s.items[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	cloned := *cfg
	return &cloned, nil
}

func (s *testConfigStore) List(_ context.Context) ([]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]any, 0, len(s.items))
	for _, cfg := range s.items {
		cloned := *cfg
		out = append(out, &cloned)
	}
	return out, nil
}

func (s *testConfigStore) ListByTag(_ context.Context, _ string) ([]any, error) {
	return s.List(context.Background())
}

func (s *testConfigStore) ListByTagPrefix(_ context.Context, _ string) ([]any, error) {
	return s.List(context.Background())
}

func (s *testConfigStore) GetByIndex(_ context.Context, _ string, _ any) (any, error) {
	return nil, configstore.ErrNotFound
}

func acpAgent(id string, routeIDs ...string) Agent {
	return Agent{
		ID:      id,
		Name:    id,
		Runtime: Runtime{Type: RuntimeTypeACP, ACP: &ACPRuntime{AgentType: "codex", CWD: "/tmp", AllowedRoots: []string{"/tmp"}}},
		Routes:  Routes{LLMRouteIDs: routeIDs},
	}
}

func TestCreateRejectsDuplicateRouteBinding(t *testing.T) {
	m := NewManager(newTestConfigStore())
	ctx := context.Background()
	if err := m.Create(ctx, acpAgent("a1", "route-1")); err != nil {
		t.Fatalf("create a1: %v", err)
	}
	a2 := acpAgent("a2")
	a2.Routes.LLMRouteIDs = []string{"route-1"}
	err := m.Create(ctx, a2)
	if err == nil {
		t.Fatalf("expected duplicate route binding to be rejected")
	}
}

func TestRefreshDropsAmbiguousRouteMapping(t *testing.T) {
	store := newTestConfigStore()
	m := NewManager(store)
	ctx := context.Background()
	if err := store.Create(ctx, &Agent{ID: "a1", Name: "a1", Runtime: acpAgent("a1").Runtime, Routes: Routes{LLMRouteIDs: []string{"shared-route"}}}); err != nil {
		t.Fatalf("seed a1: %v", err)
	}
	if err := store.Create(ctx, &Agent{ID: "a2", Name: "a2", Runtime: acpAgent("a2").Runtime, Routes: Routes{MCPRouteIDs: []string{"shared-route"}}}); err != nil {
		t.Fatalf("seed a2: %v", err)
	}
	if err := m.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if id, ok := m.ResolveAgentID("shared-route", "", ""); ok {
		t.Fatalf("ambiguous route mapping resolved to %q, want ok=false", id)
	}
}

func TestValidateRuntime(t *testing.T) {
	bad := Agent{ID: "x", Name: "x", Runtime: Runtime{Type: RuntimeTypeACP}}
	if err := bad.Validate(); err == nil {
		t.Fatalf("acp runtime without runtime.acp config must fail")
	}
	httpOK := Agent{ID: "x", Name: "x", Runtime: Runtime{Type: RuntimeTypeHTTP, HTTP: &HTTPRuntime{Endpoint: "https://x"}}}
	if err := httpOK.Validate(); err != nil {
		t.Fatalf("valid http agent rejected: %v", err)
	}
}

func TestResolveAgentIDIndex(t *testing.T) {
	m := NewManager(newTestConfigStore())
	ctx := context.Background()
	if err := m.Create(ctx, acpAgent("a1", "route-1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if id, ok := m.ResolveAgentID("route-1", "", ""); !ok || id != "a1" {
		t.Fatalf("route mapping = (%q,%v), want a1,true", id, ok)
	}
	if _, ok := m.ResolveAgentID("unknown", "unknown", ""); ok {
		t.Fatalf("unknown mapping must be ok=false")
	}
	if err := m.Delete(ctx, "a1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := m.ResolveAgentID("route-1", "", ""); ok {
		t.Fatalf("mapping must be cleared after delete")
	}
}
