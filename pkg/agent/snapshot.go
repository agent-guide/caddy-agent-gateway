package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const (
	definitionPrepareTimeout = 5 * time.Second
	definitionCleanupTimeout = 5 * time.Second
)

// DefinitionCleanup performs slow or external cleanup after a definition
// generation is dispatchable. The manager supplies a detached, bounded
// context; cleanup must not rely on the originating request remaining alive.
type DefinitionCleanup func(context.Context)

// DefinitionCommit publishes prepared runtime state while new snapshot reads
// are excluded. It must be bounded and in-memory only: it must not perform
// config-store, process, transport, or other external I/O, and it must not call
// Manager.GetSnapshot, Snapshot, HasAgent, or SnapshotGeneration because the
// snapshot mutex is not reentrant. Slow cleanup is returned for execution after
// the snapshot lock is released.
type DefinitionCommit func() DefinitionCleanup

// DefinitionListener prepares derived state for a prospective definition
// generation before the snapshot write lock is acquired. The agents slice is a
// deep clone owned by the listener call and never aliases the generation. Store
// reads and other preparation belong here; external cleanup does not.
type DefinitionListener func(ctx context.Context, agents []Agent) DefinitionCommit

// AddDefinitionListener registers a listener prepared and committed for every
// generation publication (Refresh, Create, Update, Delete). Registration is
// expected during bootstrap, before concurrent snapshot reads begin.
func (m *Manager) AddDefinitionListener(listener DefinitionListener) {
	if m == nil || listener == nil {
		return
	}
	m.snapMu.Lock()
	m.listeners = append(m.listeners, listener)
	m.snapMu.Unlock()
}

// GetSnapshot returns one Agent definition from the current immutable
// generation without touching the config store. The returned value is a deep
// clone: mutating it (including through Runtime pointers) cannot corrupt the
// generation. It is the required lookup for per-request dispatch paths.
func (m *Manager) GetSnapshot(id string) (Agent, bool) {
	if m == nil || id == "" {
		return Agent{}, false
	}
	m.snapMu.RLock()
	stored, ok := m.snapshot[id]
	m.snapMu.RUnlock()
	if !ok {
		return Agent{}, false
	}
	cloned, err := cloneAgent(stored)
	if err != nil {
		return Agent{}, false
	}
	return cloned, true
}

// HasAgent reports definition existence from the current generation. It backs
// AgentRoute target validation; disabled Agents still exist.
func (m *Manager) HasAgent(id string) bool {
	if m == nil || id == "" {
		return false
	}
	m.snapMu.RLock()
	_, ok := m.snapshot[id]
	m.snapMu.RUnlock()
	return ok
}

// Snapshot returns a deep-cloned view of every Agent in the current
// generation, sorted by id, without touching the config store.
func (m *Manager) Snapshot() []Agent {
	if m == nil {
		return nil
	}
	m.snapMu.RLock()
	agents := make([]Agent, 0, len(m.snapshot))
	for _, a := range m.snapshot {
		agents = append(agents, a)
	}
	m.snapMu.RUnlock()
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	out := make([]Agent, 0, len(agents))
	for _, a := range agents {
		cloned, err := cloneAgent(a)
		if err != nil {
			continue
		}
		out = append(out, cloned)
	}
	return out
}

// SnapshotGeneration returns the committed generation counter. It only moves
// forward and increments once per commit, letting tests and diagnostics prove
// atomic replacement.
func (m *Manager) SnapshotGeneration() uint64 {
	if m == nil {
		return 0
	}
	m.snapMu.RLock()
	defer m.snapMu.RUnlock()
	return m.generation
}

// prospectiveGeneration builds the complete next generation before a store
// write. mutate receives a private copy of the current generation map; the
// Agent values it inserts must already be deep clones. The caller must hold
// writeMu so no other mutation interleaves between build and commit.
func (m *Manager) prospectiveGeneration(ctx context.Context, mutate func(map[string]Agent)) (map[string]Agent, error) {
	if err := m.ensureSnapshotLoadedLocked(ctx); err != nil {
		return nil, err
	}
	m.snapMu.RLock()
	next := make(map[string]Agent, len(m.snapshot)+1)
	for id, a := range m.snapshot {
		next[id] = a
	}
	m.snapMu.RUnlock()
	mutate(next)
	return next, nil
}

// ensureSnapshotLoadedLocked initializes the generation when necessary. The
// caller must hold writeMu; using refreshLocked avoids recursively acquiring
// that lock from Create/Update/Delete/Recommit.
func (m *Manager) ensureSnapshotLoadedLocked(ctx context.Context) error {
	m.snapMu.RLock()
	loaded := m.snapshotLoaded
	m.snapMu.RUnlock()
	if loaded {
		return nil
	}
	return m.refreshLocked(ctx)
}

// commitGeneration prepares listener state without excluding readers,
// atomically swaps the definition snapshot and derived attribution index with
// bounded listener commits, then performs external cleanup after readers are
// admitted to the new generation. The swap itself cannot fail.
func (m *Manager) commitGeneration(ctx context.Context, agents map[string]Agent) {
	byService, byRoute := buildAttributionIndex(agents)
	m.snapMu.RLock()
	listeners := append([]DefinitionListener(nil), m.listeners...)
	m.snapMu.RUnlock()
	commits := make([]DefinitionCommit, 0, len(listeners))
	for _, listener := range listeners {
		prepareCtx, cancelPrepare := context.WithTimeout(context.WithoutCancel(ctx), definitionPrepareTimeout)
		if commit := listener(prepareCtx, cloneAgentList(agents)); commit != nil {
			commits = append(commits, commit)
		}
		cancelPrepare()
	}

	m.snapMu.Lock()
	m.snapshot = agents
	m.snapshotLoaded = true
	m.generation++
	m.mu.Lock()
	m.byService = byService
	m.byRoute = byRoute
	m.mu.Unlock()
	cleanups := make([]DefinitionCleanup, 0, len(commits))
	for _, commit := range commits {
		if cleanup := commit(); cleanup != nil {
			cleanups = append(cleanups, cleanup)
		}
	}
	m.snapMu.Unlock()

	if len(cleanups) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), definitionCleanupTimeout)
	defer cancel()
	for _, cleanup := range cleanups {
		cleanup(cleanupCtx)
	}
}

// buildAttributionIndex derives the route/service -> agent maps defensively: a
// service_id or route_id claimed by more than one agent is dropped rather than
// silently picking a winner.
func buildAttributionIndex(agents map[string]Agent) (map[string]string, map[string]string) {
	byService := map[string]string{}
	ambiguousServices := map[string]struct{}{}
	byRoute := map[string]string{}
	ambiguousRoutes := map[string]struct{}{}
	for _, a := range agents {
		if svc := a.ACPServiceID(); svc != "" {
			if owner, exists := byService[svc]; exists && owner != a.ID {
				delete(byService, svc)
				ambiguousServices[svc] = struct{}{}
			} else if _, bad := ambiguousServices[svc]; !bad {
				byService[svc] = a.ID
			}
		}
		for routeID := range agentRouteIDs(a) {
			if owner, exists := byRoute[routeID]; exists && owner != a.ID {
				delete(byRoute, routeID)
				ambiguousRoutes[routeID] = struct{}{}
			} else if _, ambiguous := ambiguousRoutes[routeID]; !ambiguous {
				byRoute[routeID] = a.ID
			}
		}
	}
	return byService, byRoute
}

func cloneAgentList(agents map[string]Agent) []Agent {
	out := make([]Agent, 0, len(agents))
	for _, a := range agents {
		cloned, err := cloneAgent(a)
		if err != nil {
			continue
		}
		out = append(out, cloned)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// cloneAgent deep-clones a definition through its JSON form so no pointer,
// slice, map, topology, middleware, environment, or reference field aliases
// the source. Definitions are JSON-persisted objects, so the round trip is
// lossless; an error is only possible for a corrupted in-memory value.
func cloneAgent(a Agent) (Agent, error) {
	data, err := json.Marshal(a)
	if err != nil {
		return Agent{}, fmt.Errorf("clone agent %q: %w", a.ID, err)
	}
	var out Agent
	if err := json.Unmarshal(data, &out); err != nil {
		return Agent{}, fmt.Errorf("clone agent %q: %w", a.ID, err)
	}
	// Preserve non-serialized legacy test/migration fields until M7 deletes the
	// unreachable compatibility code. They can never originate in a new store.
	out.OwnsService = a.OwnsService
	out.Routes.ACPRouteIDs = append([]string(nil), a.Routes.ACPRouteIDs...)
	out.Routes.BuiltinRouteIDs = append([]string(nil), a.Routes.BuiltinRouteIDs...)
	if out.Runtime.ACP != nil && a.Runtime.ACP != nil {
		out.Runtime.ACP.ServiceID = a.Runtime.ACP.ServiceID
	}
	return out, nil
}
