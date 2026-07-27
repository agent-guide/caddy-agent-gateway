package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/configstore"
)

// ACPRouteServiceLookup resolves an ACP route id to its backing service id. The
// agent manager uses it to enforce that an agent's acp_route_ids all point at
// the agent's runtime service. It is optional; when nil the consistency check
// is skipped (the manager still enforces service_id uniqueness).
type ACPRouteServiceLookup interface {
	ACPRouteServiceID(ctx context.Context, routeID string) (string, error)
}

// BuiltinRouteAgentLookup resolves a builtin route id to the agent id the route
// targets. Optional companion to ACPRouteServiceLookup: when the wired route
// lookup also implements it, the manager enforces that an agent's
// builtin_route_ids all target that agent.
type BuiltinRouteAgentLookup interface {
	BuiltinRouteAgentID(ctx context.Context, routeID string) (string, error)
}

// Manager owns agent CRUD, the deep-cloned definition snapshot used by
// per-request dispatch, and the in-memory route/service -> agent index used
// for write-time attribution. The snapshot and index are rebuilt on every
// mutation and never read from the config store on the hot path.
type Manager struct {
	store       configstore.ConfigStore
	routeLookup ACPRouteServiceLookup

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

	mu        sync.RWMutex
	byService map[string]string // acp service id -> agent id
	byRoute   map[string]string // route id -> agent id
}

func NewManager(store configstore.ConfigStore) *Manager {
	return &Manager{
		store:     store,
		snapshot:  map[string]Agent{},
		byService: map[string]string{},
		byRoute:   map[string]string{},
	}
}

// SetRouteLookup wires the optional ACP route -> service resolver used for the
// acp_route_ids consistency check.
func (m *Manager) SetRouteLookup(lookup ACPRouteServiceLookup) {
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
	if err := m.checkServiceUniqueness(ctx, a, ""); err != nil {
		return err
	}
	if err := m.checkRouteUniqueness(ctx, a, ""); err != nil {
		return err
	}
	if err := m.checkRouteConsistency(ctx, a); err != nil {
		return err
	}
	// No auto-created service write path ships yet, so clients cannot assert
	// provenance on create. Preserve this only through update of an existing
	// server-owned value.
	a.OwnsService = false
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
	a.OwnsService = current.OwnsService
	a.Normalize()
	if err := a.Validate(); err != nil {
		return err
	}
	if err := m.checkServiceUniqueness(ctx, a, id); err != nil {
		return err
	}
	if err := m.checkRouteUniqueness(ctx, a, id); err != nil {
		return err
	}
	if err := m.checkRouteConsistency(ctx, a); err != nil {
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

// checkServiceUniqueness enforces the P0 one-runtime-one-agent rule: a given ACP
// service_id is bound by at most one agent. excludeID is the agent being updated
// (so it does not collide with itself).
func (m *Manager) checkServiceUniqueness(ctx context.Context, a Agent, excludeID string) error {
	serviceID := a.ACPServiceID()
	if serviceID == "" {
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
		if other.ACPServiceID() == serviceID {
			return fmt.Errorf("acp service %q is already bound by agent %q", serviceID, other.ID)
		}
	}
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

// checkRouteConsistency enforces that every acp_route_id resolves to the agent's
// runtime service, and that every builtin_route_id targets this agent. Each
// check is skipped when the corresponding lookup is not wired.
func (m *Manager) checkRouteConsistency(ctx context.Context, a Agent) error {
	if serviceID := a.ACPServiceID(); serviceID != "" && m.routeLookup != nil {
		for _, routeID := range a.Routes.ACPRouteIDs {
			routeService, err := m.routeLookup.ACPRouteServiceID(ctx, routeID)
			if err != nil {
				return fmt.Errorf("resolve acp route %q: %w", routeID, err)
			}
			if routeService != serviceID {
				return fmt.Errorf("acp route %q targets service %q, not the agent runtime service %q", routeID, routeService, serviceID)
			}
		}
	}
	if a.Runtime.Type == RuntimeTypeBuiltin && len(a.Routes.BuiltinRouteIDs) > 0 {
		lookup, ok := m.routeLookup.(BuiltinRouteAgentLookup)
		if !ok {
			return nil
		}
		for _, routeID := range a.Routes.BuiltinRouteIDs {
			routeAgent, err := lookup.BuiltinRouteAgentID(ctx, routeID)
			if err != nil {
				return fmt.Errorf("resolve builtin route %q: %w", routeID, err)
			}
			if routeAgent != a.ID {
				return fmt.Errorf("builtin route %q targets agent %q, not this agent %q", routeID, routeAgent, a.ID)
			}
		}
	}
	return nil
}

// Refresh decodes and deep-clones the complete store result into a fresh
// definition generation, then commits it atomically. The derived
// route/service -> agent attribution index is rebuilt as part of the commit.
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

// ResolveAgentID maps an originating route/service back to a single agent for
// write-time usage attribution. It implements usage.AgentAttributor. It returns
// ok=false when the mapping is empty (the caller then leaves agent_id empty).
func (m *Manager) ResolveAgentID(routeID, serviceID, sessionID string) (string, bool) {
	_ = sessionID
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
	if serviceID != "" {
		if id, ok := m.byService[serviceID]; ok {
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
	for _, routeID := range a.Routes.ACPRouteIDs {
		if routeID != "" {
			out[routeID] = struct{}{}
		}
	}
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
	for _, routeID := range a.Routes.BuiltinRouteIDs {
		if routeID != "" {
			out[routeID] = struct{}{}
		}
	}
	return out
}
