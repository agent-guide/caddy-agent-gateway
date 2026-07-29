package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/configstore"
)

// AgentRouteLookup resolves unified ingress routes that target an Agent. It is
// used to prevent deleting a definition while a route still references it.
type AgentRouteLookup interface {
	AgentRouteIDsForAgent(ctx context.Context, agentID string) ([]string, error)
}

// Manager owns agent CRUD, the deep-cloned definition snapshot used by
// per-request dispatch, and the in-memory resource-route -> agent index used
// for write-time attribution. The snapshot and index are rebuilt on every
// mutation and never read from the config store on the hot path.
type Manager struct {
	store       configstore.ConfigStore
	routeLookup AgentRouteLookup

	// writeMu serializes the validate -> prospective-generation ->
	// store-write -> generation-commit sequence so the P0
	// one-runtime-one-agent invariant holds under concurrent mutations and
	// the committed generation always reflects the store write that just
	// succeeded. It is independent of mu (index maps) and snapMu (snapshot).
	writeMu sync.Mutex

	// snapMu guards the immutable definition generation. Listener preparation
	// and cleanup run outside it; only bounded in-memory listener commits share
	// the write-locked publication window.
	snapMu         sync.RWMutex
	snapshot       map[string]Agent
	snapshotLoaded bool
	generation     uint64
	listeners      []DefinitionListener

	mu      sync.RWMutex
	byRoute map[string]string // route id -> agent id
}

func NewManager(store configstore.ConfigStore) *Manager {
	return &Manager{
		store:    store,
		snapshot: map[string]Agent{},
		byRoute:  map[string]string{},
	}
}

// SetRouteLookup wires the optional unified AgentRoute reference lookup.
func (m *Manager) SetRouteLookup(lookup AgentRouteLookup) {
	if m == nil {
		return
	}
	m.routeLookup = lookup
}

func (m *Manager) List(ctx context.Context) ([]Agent, error) {
	if m == nil || m.store == nil {
		return nil, ErrAgentNotConfigured
	}
	items, err := m.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Agent, 0, len(items))
	for _, item := range items {
		cfg, err := decodeAgentItem("", item)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	return out, nil
}

func (m *Manager) Get(ctx context.Context, id string) (Agent, error) {
	if id == "" {
		return Agent{}, fmt.Errorf("id is required")
	}
	if m == nil || m.store == nil {
		return Agent{}, ErrAgentNotConfigured
	}
	item, err := m.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			return Agent{}, ErrAgentNotConfigured
		}
		return Agent{}, err
	}
	return decodeAgentItem(id, item)
}

func (m *Manager) Create(ctx context.Context, a Agent) error {
	if m == nil || m.store == nil {
		return ErrAgentNotConfigured
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	a.Normalize()
	if err := a.Validate(); err != nil {
		return err
	}
	if err := m.checkRouteUniqueness(ctx, a, ""); err != nil {
		return err
	}
	a.NormalizeTimestamps(time.Now().UTC())
	cloned, err := cloneAgent(a)
	if err != nil {
		return err
	}
	prospective, err := m.prospectiveGeneration(ctx, func(gen map[string]Agent) {
		gen[cloned.ID] = cloned
	})
	if err != nil {
		return err
	}
	if err := m.store.Create(ctx, storedAgent{cfg: &a, tag: a.Runtime.Type}); err != nil {
		return err
	}
	m.commitGeneration(ctx, prospective)
	return nil
}

func (m *Manager) Update(ctx context.Context, id string, a Agent) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}
	if m == nil || m.store == nil {
		return ErrAgentNotConfigured
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	current, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	a.ID = id
	a.CreatedAt = current.CreatedAt
	a.Normalize()
	if err := a.Validate(); err != nil {
		return err
	}
	if err := m.checkRouteUniqueness(ctx, a, id); err != nil {
		return err
	}
	a.NormalizeTimestamps(time.Now().UTC())
	cloned, err := cloneAgent(a)
	if err != nil {
		return err
	}
	prospective, err := m.prospectiveGeneration(ctx, func(gen map[string]Agent) {
		gen[cloned.ID] = cloned
	})
	if err != nil {
		return err
	}
	if err := m.store.Update(ctx, storedAgent{cfg: &a, tag: a.Runtime.Type}); err != nil {
		return err
	}
	m.commitGeneration(ctx, prospective)
	return nil
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}
	if m == nil || m.store == nil {
		return ErrAgentNotConfigured
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	if m.routeLookup != nil {
		routeIDs, err := m.routeLookup.AgentRouteIDsForAgent(ctx, id)
		if err != nil {
			return fmt.Errorf("check agent route references: %w", err)
		}
		if len(routeIDs) > 0 {
			return fmt.Errorf("%w: agent %q is targeted by agent route %q", ErrAgentRouteTarget, id, routeIDs[0])
		}
	}
	prospective, err := m.prospectiveGeneration(ctx, func(gen map[string]Agent) {
		delete(gen, id)
	})
	if err != nil {
		return err
	}
	if err := m.store.Delete(ctx, id); err != nil {
		return err
	}
	m.commitGeneration(ctx, prospective)
	return nil
}

// checkRouteUniqueness keeps the route -> agent attribution mapping
// unambiguous. Route ids are globally unique in the shared routes store, so any
// route id may be claimed by at most one agent across all route families.
func (m *Manager) checkRouteUniqueness(ctx context.Context, a Agent, excludeID string) error {
	routeIDs := agentRouteIDs(a)
	if len(routeIDs) == 0 {
		return nil
	}
	existing, err := m.List(ctx)
	if err != nil {
		return err
	}
	for _, other := range existing {
		if other.ID == excludeID || other.ID == a.ID {
			continue
		}
		for routeID := range agentRouteIDs(other) {
			if _, ok := routeIDs[routeID]; ok {
				return fmt.Errorf("route %q is already bound by agent %q", routeID, other.ID)
			}
		}
	}
	return nil
}

// Refresh decodes and deep-clones the complete store result into a fresh
// definition generation, then commits it atomically. The derived
// resource-route -> agent attribution index is rebuilt as part of the commit.
func (m *Manager) Refresh(ctx context.Context) error {
	if m == nil || m.store == nil {
		return nil
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	return m.refreshLocked(ctx)
}

// refreshLocked replaces the current definition generation from the store.
// The caller must hold writeMu so the List -> commit sequence cannot publish a
// stale store view after a concurrent Create, Update, or Delete has committed.
func (m *Manager) refreshLocked(ctx context.Context) error {
	agents, err := m.List(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]Agent, len(agents))
	for _, a := range agents {
		cloned, err := cloneAgent(a)
		if err != nil {
			return err
		}
		next[cloned.ID] = cloned
	}
	m.commitGeneration(ctx, next)
	return nil
}

// Recommit republishes the already-loaded definition generation without
// reading the Agent store. It is used when an external runtime record changes
// and definition listeners must rebuild derived runtime snapshots.
func (m *Manager) Recommit(ctx context.Context) error {
	if m == nil || m.store == nil {
		return nil
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	m.snapMu.RLock()
	loaded := m.snapshotLoaded
	m.snapMu.RUnlock()
	if !loaded {
		// The initial store load already commits and notifies listeners; a
		// second identical generation would add no reconciliation value.
		return m.refreshLocked(ctx)
	}
	m.snapMu.RLock()
	next := make(map[string]Agent, len(m.snapshot))
	for id, a := range m.snapshot {
		next[id] = a
	}
	m.snapMu.RUnlock()
	m.commitGeneration(ctx, next)
	return nil
}

// ResolveAgentID maps an originating resource route back to a single agent for
// write-time usage attribution. The service/session arguments remain part of
// the protocol-neutral usage seam for MCP callers but do not identify an Agent.
// It returns ok=false when the route mapping is empty.
func (m *Manager) ResolveAgentID(routeID, serviceID, sessionID string) (string, bool) {
	_, _ = serviceID, sessionID
	if m == nil {
		return "", false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if routeID != "" {
		if id, ok := m.byRoute[routeID]; ok {
			return id, true
		}
	}
	return "", false
}

type storedAgent struct {
	cfg any
	tag string
}

func (c storedAgent) ConfigStoreObject() any { return c.cfg }
func (c storedAgent) ConfigStoreTag() string { return c.tag }

func decodeAgentItem(id string, item any) (Agent, error) {
	cfg, ok := item.(*Agent)
	if !ok || cfg == nil || cfg.ID == "" {
		if id == "" {
			id = "<unknown>"
		}
		return Agent{}, fmt.Errorf("agent %q has unexpected type %T", id, item)
	}
	cloned := *cfg
	return cloned, nil
}

func agentRouteIDs(a Agent) map[string]struct{} {
	out := map[string]struct{}{}
	for _, routeID := range a.Routes.LLMRouteIDs {
		if routeID != "" {
			out[routeID] = struct{}{}
		}
	}
	for _, routeID := range a.Routes.MCPRouteIDs {
		if routeID != "" {
			out[routeID] = struct{}{}
		}
	}
	return out
}
