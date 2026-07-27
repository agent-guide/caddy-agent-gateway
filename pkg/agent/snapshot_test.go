package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/configstore"
)

// countingConfigStore wraps the in-memory store and counts reads so tests can
// prove that snapshot lookups never touch the config store.
type countingConfigStore struct {
	configstore.ConfigStore
	reads atomic.Int64
}

type blockingListConfigStore struct {
	configstore.ConfigStore
	mu        sync.Mutex
	blockNext bool
	listed    chan struct{}
	release   chan struct{}
}

func (s *blockingListConfigStore) arm() (<-chan struct{}, chan<- struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockNext = true
	s.listed = make(chan struct{})
	s.release = make(chan struct{})
	return s.listed, s.release
}

func (s *blockingListConfigStore) List(ctx context.Context) ([]any, error) {
	items, err := s.ConfigStore.List(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	block := s.blockNext
	listed, release := s.listed, s.release
	s.blockNext = false
	s.mu.Unlock()
	if block {
		close(listed)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return items, nil
}

func (s *countingConfigStore) Get(ctx context.Context, keyParts ...any) (any, error) {
	s.reads.Add(1)
	return s.ConfigStore.Get(ctx, keyParts...)
}

func (s *countingConfigStore) List(ctx context.Context) ([]any, error) {
	s.reads.Add(1)
	return s.ConfigStore.List(ctx)
}

func snapshotTestAgent(id string) Agent {
	return Agent{
		ID:        id,
		Name:      id,
		Runtime:   Runtime{Type: RuntimeTypeACP, ACP: &ACPRuntime{ServiceID: "svc-" + id}},
		Resources: Resources{MCPServiceIDs: []string{"docs"}},
	}
}

func TestGetSnapshotDeepCloneMutationIsolation(t *testing.T) {
	m := NewManager(newTestConfigStore())
	if err := m.Create(context.Background(), snapshotTestAgent("a1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, ok := m.GetSnapshot("a1")
	if !ok {
		t.Fatal("GetSnapshot returned ok=false")
	}
	// Mutate every reference-typed field on the returned value; the committed
	// generation must be unaffected.
	got.Runtime.ACP.ServiceID = "mutated"
	got.Resources.MCPServiceIDs[0] = "mutated"
	again, ok := m.GetSnapshot("a1")
	if !ok {
		t.Fatal("second GetSnapshot returned ok=false")
	}
	if again.Runtime.ACP.ServiceID != "svc-a1" || again.Resources.MCPServiceIDs[0] != "docs" {
		t.Fatalf("snapshot was corrupted by caller mutation: %+v", again)
	}
	for _, a := range m.Snapshot() {
		a.Runtime.ACP.ServiceID = "mutated-again"
	}
	final, _ := m.GetSnapshot("a1")
	if final.Runtime.ACP.ServiceID != "svc-a1" {
		t.Fatalf("Snapshot list aliases the generation: %+v", final)
	}
}

func TestSnapshotReadsDoNotTouchStore(t *testing.T) {
	store := &countingConfigStore{ConfigStore: newTestConfigStore()}
	m := NewManager(store)
	if err := m.Create(context.Background(), snapshotTestAgent("a1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := store.reads.Load()
	if _, ok := m.GetSnapshot("a1"); !ok {
		t.Fatal("GetSnapshot returned ok=false")
	}
	if !m.HasAgent("a1") || m.HasAgent("ghost") {
		t.Fatal("HasAgent existence check is wrong")
	}
	if got := len(m.Snapshot()); got != 1 {
		t.Fatalf("Snapshot len = %d, want 1", got)
	}
	if _, ok := m.GetSnapshot("ghost"); ok {
		t.Fatal("GetSnapshot(ghost) returned ok=true")
	}
	if store.reads.Load() != before {
		t.Fatalf("snapshot reads touched the store %d times", store.reads.Load()-before)
	}
}

func TestSnapshotGenerationSwapAndInvalidation(t *testing.T) {
	ctx := context.Background()
	m := NewManager(newTestConfigStore())
	if err := m.Create(ctx, snapshotTestAgent("a1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	genAfterCreate := m.SnapshotGeneration()

	updated := snapshotTestAgent("a1")
	updated.Description = "updated"
	if err := m.Update(ctx, "a1", updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if m.SnapshotGeneration() != genAfterCreate+1 {
		t.Fatalf("generation after update = %d, want %d", m.SnapshotGeneration(), genAfterCreate+1)
	}
	got, ok := m.GetSnapshot("a1")
	if !ok || got.Description != "updated" {
		t.Fatalf("snapshot after update = %+v, ok=%v", got, ok)
	}

	if err := m.Delete(ctx, "a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := m.GetSnapshot("a1"); ok {
		t.Fatal("GetSnapshot returned deleted agent")
	}
	if m.SnapshotGeneration() != genAfterCreate+2 {
		t.Fatalf("generation after delete = %d, want %d", m.SnapshotGeneration(), genAfterCreate+2)
	}
}

func TestRefreshCannotPublishStaleViewAfterConcurrentUpdate(t *testing.T) {
	ctx := context.Background()
	store := &blockingListConfigStore{ConfigStore: newTestConfigStore()}
	m := NewManager(store)
	if err := m.Create(ctx, snapshotTestAgent("a1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	listed, release := store.arm()
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- m.Refresh(ctx) }()
	<-listed

	updated := snapshotTestAgent("a1")
	updated.Description = "updated"
	updateDone := make(chan error, 1)
	go func() { updateDone <- m.Update(ctx, "a1", updated) }()
	select {
	case err := <-updateDone:
		t.Fatalf("Update interleaved with an in-progress Refresh: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-refreshDone; err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if err := <-updateDone; err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, ok := m.GetSnapshot("a1")
	if !ok || got.Description != "updated" {
		t.Fatalf("final snapshot = %+v, ok=%v; stale Refresh overwrote Update", got, ok)
	}
}

func TestRecommitDoesNotReadAgentStore(t *testing.T) {
	store := &countingConfigStore{ConfigStore: newTestConfigStore()}
	m := NewManager(store)
	if err := m.Create(context.Background(), snapshotTestAgent("a1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := store.reads.Load()
	if err := m.Recommit(context.Background()); err != nil {
		t.Fatalf("Recommit: %v", err)
	}
	if got := store.reads.Load(); got != before {
		t.Fatalf("Recommit read Agent store: before=%d after=%d", before, got)
	}
}

func TestFailedStoreWriteDoesNotCommitGeneration(t *testing.T) {
	ctx := context.Background()
	m := NewManager(newTestConfigStore())
	if err := m.Create(ctx, snapshotTestAgent("a1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	gen := m.SnapshotGeneration()
	// Duplicate create fails at the store write; the prospective generation
	// must be discarded.
	if err := m.Create(ctx, snapshotTestAgent("a1")); err == nil {
		t.Fatal("duplicate Create succeeded")
	}
	if m.SnapshotGeneration() != gen {
		t.Fatalf("generation after failed create = %d, want %d", m.SnapshotGeneration(), gen)
	}
}

func TestDefinitionListenerObservesEveryCommit(t *testing.T) {
	ctx := context.Background()
	m := NewManager(newTestConfigStore())
	var calls [][]Agent
	m.AddDefinitionListener(func(_ context.Context, agents []Agent) DefinitionCommit {
		return func() DefinitionCleanup {
			calls = append(calls, agents)
			return nil
		}
	})
	if err := m.Create(ctx, snapshotTestAgent("a1")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Delete(ctx, "a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// The first mutation lazily loads the snapshot (one empty refresh commit),
	// then create and delete each commit one generation.
	if len(calls) != 3 || len(calls[0]) != 0 || len(calls[1]) != 1 || calls[1][0].ID != "a1" || len(calls[2]) != 0 {
		t.Fatalf("listener calls = %+v", calls)
	}
	// The listener payload is a clone: mutating it never corrupts a generation.
	if err := m.Create(ctx, snapshotTestAgent("a2")); err != nil {
		t.Fatalf("Create a2: %v", err)
	}
	calls[3][0].Runtime.ACP.ServiceID = "mutated"
	got, _ := m.GetSnapshot("a2")
	if got.Runtime.ACP.ServiceID != "svc-a2" {
		t.Fatalf("listener payload aliases the generation: %+v", got)
	}
}

func TestDefinitionListenerPreparationAndCleanupDoNotBlockSnapshotReads(t *testing.T) {
	m := NewManager(newTestConfigStore())
	a := snapshotTestAgent("a1")
	if err := m.Create(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	prepareEntered, releasePrepare := make(chan struct{}), make(chan struct{})
	cleanupEntered, releaseCleanup := make(chan struct{}), make(chan struct{})
	m.AddDefinitionListener(func(context.Context, []Agent) DefinitionCommit {
		close(prepareEntered)
		<-releasePrepare
		return func() DefinitionCleanup {
			return func(context.Context) {
				close(cleanupEntered)
				<-releaseCleanup
			}
		}
	})
	a.Description = "updated"
	done := make(chan error, 1)
	go func() { done <- m.Update(context.Background(), a.ID, a) }()
	<-prepareEntered
	read := make(chan Agent, 1)
	go func() { got, _ := m.GetSnapshot(a.ID); read <- got }()
	select {
	case got := <-read:
		if got.Description == "updated" {
			t.Fatalf("prospective generation became visible during prepare: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot read blocked by listener preparation")
	}
	close(releasePrepare)
	<-cleanupEntered
	go func() { got, _ := m.GetSnapshot(a.ID); read <- got }()
	select {
	case got := <-read:
		if got.Description != "updated" {
			t.Fatalf("committed generation not visible during cleanup: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot read blocked by listener cleanup")
	}
	close(releaseCleanup)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
