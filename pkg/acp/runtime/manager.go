package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	acptransport "github.com/agent-guide/agent-gateway/pkg/acp/transport"
)

const defaultJanitorInterval = 30 * time.Second

// ErrInvalidRequest marks a client-correctable problem with a session or
// transcript request (a disabled service, a cwd outside allowed_roots, or a
// missing session id). Callers map it to HTTP 400; service-not-found maps to
// 404 and unwrapped agent/transport failures map to 502.
var ErrInvalidRequest = errors.New("invalid acp request")

var ErrCapacityExceeded = errors.New("acp instance capacity exceeded")
var ErrRunNotFound = errors.New("acp run not found")
var ErrRunNotReady = errors.New("acp run cancellation is not ready")
var ErrTurnCancelled = errors.New("acp turn cancelled")
var ErrRuntimeConfigRetired = errors.New("acp runtime config is retired")

type Manager struct {
	services    *acpservice.Manager
	active      *ActivityTracker
	permissions *permissionBroker
	runs        *activeRunRegistry

	janitorInterval time.Duration
	notifyObserver  func(string, json.RawMessage)

	mu                sync.Mutex
	instances         map[string]*managedInstance
	ownerFingerprints map[string]string

	closeOnce sync.Once
	done      chan struct{}
}

type managedInstance struct {
	instance *instance
	idleTTL  time.Duration
	lastUsed time.Time
	// fingerprint is the config content hash the instance was created under.
	// A turn only reuses an instance whose fingerprint matches the current
	// config, so a config update can never execute on a stale process.
	fingerprint string
	// retired marks an instance whose config fingerprint was retired while a
	// turn was still active. It accepts no new turns and is reaped by the
	// janitor as soon as the in-flight turn drains.
	retired bool
}

func NewManager(services *acpservice.Manager) *Manager {
	m := &Manager{
		services:          services,
		active:            NewActivityTracker(),
		permissions:       newPermissionBroker(),
		runs:              newActiveRunRegistry(),
		janitorInterval:   defaultJanitorInterval,
		instances:         map[string]*managedInstance{},
		ownerFingerprints: map[string]string{},
		done:              make(chan struct{}),
	}
	go m.janitor()
	return m
}

// Close stops the janitor and tears down every pooled instance, killing the
// underlying agent subprocesses. It is safe to call more than once.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() { close(m.done) })
	m.mu.Lock()
	victims := make([]*instance, 0, len(m.instances))
	for scope, item := range m.instances {
		if item != nil && item.instance != nil {
			victims = append(victims, item.instance)
		}
		delete(m.instances, scope)
	}
	m.mu.Unlock()
	for _, inst := range victims {
		_ = inst.close()
	}
}

const scopeSep = "\x00"

// buildScope assembles the pool key. Fields: serviceID, cwd, threadID,
// sessionID, model.
func buildScope(serviceID, cwd, threadID, sessionID, model string) string {
	return strings.Join([]string{serviceID, cwd, threadID, sessionID, model}, scopeSep)
}

func scopeMatchesThread(scope, serviceID, threadID string) bool {
	parts := strings.Split(scope, scopeSep)
	return len(parts) == 5 && parts[0] == serviceID && parts[2] == threadID
}

// ScopeServiceID extracts the service id encoded as the first segment of a
// pooled-instance scope, or "" if the scope is malformed. It lets callers map a
// PooledInstanceInfo / InFlightTurn back to its owning service without exposing
// the scope-encoding format.
func ScopeServiceID(scope string) string {
	parts := strings.Split(scope, scopeSep)
	if len(parts) == 5 {
		return parts[0]
	}
	return ""
}

// CloseScope tears down the pooled instance for an exact scope, returning
// whether one was closed. An in-flight turn on that scope will fail on its next
// request.
func (m *Manager) CloseScope(scope string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	item := m.instances[scope]
	if item == nil || item.instance == nil {
		m.mu.Unlock()
		return false
	}
	delete(m.instances, scope)
	inst := item.instance
	m.mu.Unlock()
	_ = inst.close()
	return true
}

// CloseThread tears down every pooled instance for a service+thread and returns
// the count closed. Intended as an operator escape hatch for a wedged thread.
func (m *Manager) CloseThread(serviceID, threadID string) int {
	if m == nil {
		return 0
	}
	serviceID = strings.TrimSpace(serviceID)
	threadID = strings.TrimSpace(threadID)
	var victims []*instance
	m.mu.Lock()
	for scope, item := range m.instances {
		if item == nil || item.instance == nil {
			delete(m.instances, scope)
			continue
		}
		if scopeMatchesThread(scope, serviceID, threadID) {
			victims = append(victims, item.instance)
			delete(m.instances, scope)
		}
	}
	m.mu.Unlock()
	for _, inst := range victims {
		_ = inst.close()
	}
	return len(victims)
}

// CloseService tears down every pooled instance for a service and returns the
// count closed. This is used after process-affecting service config changes so
// the next turn starts an agent with the latest launch environment.
func (m *Manager) CloseService(serviceID string) int {
	if m == nil {
		return 0
	}
	serviceID = strings.TrimSpace(serviceID)
	var victims []*instance
	m.mu.Lock()
	for scope, item := range m.instances {
		if item == nil || item.instance == nil {
			delete(m.instances, scope)
			continue
		}
		parts := strings.Split(scope, scopeSep)
		if len(parts) == 5 && parts[0] == serviceID {
			victims = append(victims, item.instance)
			delete(m.instances, scope)
		}
	}
	m.mu.Unlock()
	for _, inst := range victims {
		_ = inst.close()
	}
	return len(victims)
}

// RetireOwner retires every pooled instance owned by ownerID whose config
// fingerprint differs from keepFingerprint (an empty keep retires all). Idle
// instances close immediately; an instance with an active turn is marked
// retired so the in-flight turn drains, accepts no further reuse, and is
// reaped by the janitor once idle. It returns the number of instances
// closed or marked.
func (m *Manager) RetireOwner(ownerID, keepFingerprint string) int {
	marked, cleanup := m.RetireOwnerDeferred(ownerID, keepFingerprint)
	cleanup()
	return marked
}

// RetireOwnerDeferred performs the bounded, in-memory part of RetireOwner and
// returns cleanup that closes idle victims outside a caller's coordination
// lock. The state transition takes effect before this method returns.
func (m *Manager) RetireOwnerDeferred(ownerID, keepFingerprint string) (int, func()) {
	if m == nil {
		return 0, func() {}
	}
	ownerID = strings.TrimSpace(ownerID)
	var victims []*instance
	marked := 0
	m.mu.Lock()
	if m.ownerFingerprints == nil {
		m.ownerFingerprints = map[string]string{}
	}
	// Persist the currently allowed configured-runtime fingerprint. This closes
	// the gap where a request cloned an old config before retirement and would
	// otherwise create and insert a stale instance after this scan completes.
	if keepFingerprint == "" {
		delete(m.ownerFingerprints, ownerID)
	} else {
		m.ownerFingerprints[ownerID] = keepFingerprint
	}
	for scope, item := range m.instances {
		if item == nil || item.instance == nil {
			delete(m.instances, scope)
			continue
		}
		if ScopeServiceID(scope) != ownerID {
			continue
		}
		if keepFingerprint != "" && item.fingerprint == keepFingerprint && !item.retired {
			continue
		}
		if m.active.IsActive(scope) {
			if !item.retired {
				item.retired = true
				marked++
			}
			continue
		}
		victims = append(victims, item.instance)
		delete(m.instances, scope)
	}
	m.mu.Unlock()
	cleanup := func() {
		for _, inst := range victims {
			_ = inst.close()
		}
	}
	return len(victims) + marked, cleanup
}

func (m *Manager) ServeTurn(ctx context.Context, serviceID string, req TurnRequest, emit EventSink) error {
	if m == nil || m.services == nil {
		return fmt.Errorf("acp runtime manager is not configured")
	}
	cfg, err := m.services.Get(ctx, strings.TrimSpace(serviceID))
	if err != nil {
		return err
	}
	if cfg.Disabled {
		return fmt.Errorf("acp service %q is disabled", cfg.ID)
	}
	return m.serveTurn(ctx, cfg, req, emit, false)
}

// ServeConfiguredTurn executes an Agent-owned ACP runtime from an
// identity-free snapshot. It performs no service-store lookup; ownerID is the
// pool/permission ownership key and is never exposed through the common SPI.
func (m *Manager) ServeConfiguredTurn(ctx context.Context, ownerID string, runtimeCfg RuntimeConfig, req TurnRequest, emit EventSink) error {
	if m == nil {
		return fmt.Errorf("acp runtime manager is not configured")
	}
	cfg, err := runtimeCfg.serviceConfig(ownerID)
	if err != nil {
		return err
	}
	return m.serveTurn(ctx, cfg, req, emit, true)
}

func (m *Manager) serveTurn(ctx context.Context, cfg acpservice.ServiceConfig, req TurnRequest, emit EventSink, enforceOwnerFingerprint bool) error {
	req.ThreadID = strings.TrimSpace(req.ThreadID)
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Input = strings.TrimSpace(req.Input)
	if req.ThreadID == "" {
		return fmt.Errorf("thread_id is required")
	}
	if req.Input == "" {
		return fmt.Errorf("input is required")
	}
	var run *activeRun
	if strings.TrimSpace(req.RunID) != "" {
		var runCtx context.Context
		runCtx, run = m.runs.begin(ctx, cfg.ID, strings.TrimSpace(req.RunID), req.SessionID)
		if run == nil {
			return fmt.Errorf("%w: duplicate run_id %q", ErrInvalidRequest, req.RunID)
		}
		ctx = runCtx
		defer m.runs.finish(run)
	}
	cwd := strings.TrimSpace(req.CWD)
	if cwd == "" {
		cwd = cfg.CWD
	}
	if err := acpservice.ValidateCWDAllowed(cwd, cfg.AllowedRoots); err != nil {
		return err
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = cfg.DefaultModel
	}
	scope := buildScope(cfg.ID, cwd, req.ThreadID, req.SessionID, model)
	release, err := m.active.Begin(scope)
	if err != nil {
		return err
	}
	defer release()

	inst, err := m.resolveInstanceForOwner(ctx, scope, cfg, req, enforceOwnerFingerprint)
	if err != nil {
		return err
	}
	if run != nil {
		if err := m.runs.bind(run, inst); err != nil {
			return err
		}
	}
	stopReason, err := inst.prompt(ctx, req, emit)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return ErrTurnCancelled
		}
		return err
	}
	if emit != nil {
		return emit(TurnEvent{Event: "done", StopReason: stopReason})
	}
	return nil
}

// CancelRun sends native session/cancel to exactly one active logical run and
// cancels that run's context. It never closes a scope, thread, or pool entry.
func (m *Manager) CancelRun(ownerID, runID string) error {
	if m == nil || m.runs == nil {
		return ErrRunNotFound
	}
	return m.runs.cancel(strings.TrimSpace(ownerID), strings.TrimSpace(runID))
}

// RequestCancelRun records fail-closed lifecycle cancellation. Unlike the
// exact CancelRun operation, a pre-bind run accepts the request and is stopped
// by bind before its prompt starts.
func (m *Manager) RequestCancelRun(ownerID, runID string) error {
	if m == nil || m.runs == nil {
		return ErrRunNotFound
	}
	return m.runs.requestCancel(strings.TrimSpace(ownerID), strings.TrimSpace(runID))
}

func (m *Manager) ListActiveRuns(ownerID string) []ActiveRunInfo {
	if m == nil || m.runs == nil {
		return []ActiveRunInfo{}
	}
	return m.runs.list(strings.TrimSpace(ownerID))
}

func (m *Manager) ListSessions(ctx context.Context, serviceID string, req ListSessionsRequest) (ListSessionsResponse, error) {
	if m == nil || m.services == nil {
		return ListSessionsResponse{}, fmt.Errorf("acp runtime manager is not configured")
	}
	cfg, err := m.services.Get(ctx, strings.TrimSpace(serviceID))
	if err != nil {
		return ListSessionsResponse{}, err
	}
	if cfg.Disabled {
		return ListSessionsResponse{}, fmt.Errorf("%w: acp service %q is disabled", ErrInvalidRequest, cfg.ID)
	}
	if req.CWD = strings.TrimSpace(req.CWD); req.CWD != "" {
		if err := acpservice.ValidateCWDAllowed(req.CWD, cfg.AllowedRoots); err != nil {
			return ListSessionsResponse{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
	}
	return listAgentSessions(ctx, cfg, req)
}

func (m *Manager) ListConfiguredSessions(ctx context.Context, ownerID string, runtimeCfg RuntimeConfig, req ListSessionsRequest) (ListSessionsResponse, error) {
	cfg, err := runtimeCfg.serviceConfig(strings.TrimSpace(ownerID))
	if err != nil {
		return ListSessionsResponse{}, err
	}
	if req.CWD = strings.TrimSpace(req.CWD); req.CWD != "" {
		if err := acpservice.ValidateCWDAllowed(req.CWD, cfg.AllowedRoots); err != nil {
			return ListSessionsResponse{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
	}
	return listAgentSessions(ctx, cfg, req)
}

// ResolvePermission answers one pending interactive permission request. The
// outcome must be the ACP discriminator "selected" (with the chosen option id
// exactly as offered by the agent) or "cancelled".
func (m *Manager) ResolvePermission(decision PermissionDecision) error {
	if m == nil || m.permissions == nil {
		return fmt.Errorf("acp runtime manager is not configured")
	}
	if strings.TrimSpace(decision.RequestID) == "" {
		return fmt.Errorf("request_id is required")
	}
	resp := acptransport.PermissionResponse{Outcome: strings.TrimSpace(decision.Outcome)}
	switch resp.Outcome {
	case acptransport.PermissionOutcomeSelected:
		resp.SelectedOptionID = strings.TrimSpace(decision.OptionID)
		if resp.SelectedOptionID == "" {
			return fmt.Errorf("option_id is required when outcome is %q", acptransport.PermissionOutcomeSelected)
		}
	case acptransport.PermissionOutcomeCancelled:
	default:
		return fmt.Errorf("unsupported outcome %q (want %q or %q)", decision.Outcome, acptransport.PermissionOutcomeSelected, acptransport.PermissionOutcomeCancelled)
	}
	return m.permissions.resolve(decision.RequestID, resp)
}

// ListPendingPermissions returns the in-flight interactive permission
// requests awaiting a decision.
func (m *Manager) ListPendingPermissions() []PendingPermissionInfo {
	if m == nil || m.permissions == nil {
		return nil
	}
	return m.permissions.list()
}

func (m *Manager) LoadTranscript(ctx context.Context, serviceID string, req TranscriptRequest) (TranscriptResponse, error) {
	if m == nil || m.services == nil {
		return TranscriptResponse{}, fmt.Errorf("acp runtime manager is not configured")
	}
	cfg, err := m.services.Get(ctx, strings.TrimSpace(serviceID))
	if err != nil {
		return TranscriptResponse{}, err
	}
	if cfg.Disabled {
		return TranscriptResponse{}, fmt.Errorf("%w: acp service %q is disabled", ErrInvalidRequest, cfg.ID)
	}
	if req.CWD = strings.TrimSpace(req.CWD); req.CWD != "" {
		if err := acpservice.ValidateCWDAllowed(req.CWD, cfg.AllowedRoots); err != nil {
			return TranscriptResponse{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
	}
	return loadAgentTranscript(ctx, cfg, req)
}

func (m *Manager) LoadConfiguredTranscript(ctx context.Context, ownerID string, runtimeCfg RuntimeConfig, req TranscriptRequest) (TranscriptResponse, error) {
	cfg, err := runtimeCfg.serviceConfig(strings.TrimSpace(ownerID))
	if err != nil {
		return TranscriptResponse{}, err
	}
	if req.CWD = strings.TrimSpace(req.CWD); req.CWD != "" {
		if err := acpservice.ValidateCWDAllowed(req.CWD, cfg.AllowedRoots); err != nil {
			return TranscriptResponse{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
	}
	return loadAgentTranscript(ctx, cfg, req)
}

func (m *Manager) ListInFlight() []InFlightTurn {
	if m == nil || m.active == nil {
		return nil
	}
	return m.active.List()
}

// ListInstances returns an operator-facing snapshot of the pooled instances,
// including each instance's cached session metadata, sorted by scope.
func (m *Manager) ListInstances() []PooledInstanceInfo {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	out := make([]PooledInstanceInfo, 0, len(m.instances))
	for scope, item := range m.instances {
		if item == nil || item.instance == nil {
			continue
		}
		out = append(out, PooledInstanceInfo{
			Scope:     scope,
			SessionID: item.instance.sessionID,
			Alive:     item.instance.alive(),
			Active:    m.active.IsActive(scope),
			LastUsed:  item.lastUsed,
			IdleTTL:   item.idleTTL,
			Metadata:  item.instance.meta.snapshot(),
		})
	}
	m.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].Scope < out[b].Scope })
	return out
}

// ListOwnerInstances keeps pool-scope encoding inside the ACP runtime while
// exposing the instances owned by one Agent/service identity to adapters.
func (m *Manager) ListOwnerInstances(ownerID string) []PooledInstanceInfo {
	ownerID = strings.TrimSpace(ownerID)
	out := make([]PooledInstanceInfo, 0)
	for _, inst := range m.ListInstances() {
		if ScopeServiceID(inst.Scope) == ownerID {
			out = append(out, inst)
		}
	}
	return out
}

func (m *Manager) resolveInstance(ctx context.Context, scope string, cfg acpservice.ServiceConfig, req TurnRequest) (*instance, error) {
	return m.resolveInstanceForOwner(ctx, scope, cfg, req, false)
}

func (m *Manager) resolveInstanceForOwner(ctx context.Context, scope string, cfg acpservice.ServiceConfig, req TurnRequest, enforceOwnerFingerprint bool) (*instance, error) {
	fingerprint := configFingerprint(cfg)
	// ServeTurn holds the per-scope activity lock for the whole turn, so only one
	// turn resolves a given scope at a time and the janitor skips active scopes;
	// no other goroutine mutates this scope's pool entry while we are here.
	m.mu.Lock()
	if enforceOwnerFingerprint && !m.ownerFingerprintCurrentLocked(cfg.ID, fingerprint) {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: owner %q", ErrRuntimeConfigRetired, cfg.ID)
	}
	if item := m.instances[scope]; item != nil && item.instance != nil {
		// Reuse only a live instance created under the current config
		// fingerprint, and never when the caller asked for a fresh session.
		// Otherwise evict and tear down the stale/dead/retired instance.
		if !req.FreshSession && item.instance.alive() && !item.retired && item.fingerprint == fingerprint {
			item.lastUsed = time.Now().UTC()
			inst := item.instance
			m.mu.Unlock()
			return inst, nil
		}
		delete(m.instances, scope)
		stale := item.instance
		m.mu.Unlock()
		_ = stale.close()
	} else if inst := m.adoptSessionInstanceLocked(scope, fingerprint, cfg, req); inst != nil {
		m.mu.Unlock()
		return inst, nil
	} else {
		if cfg.MaxInstances > 0 && m.serviceInstanceCountLocked(cfg.ID) >= cfg.MaxInstances {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: service %q reached max_instances %d", ErrCapacityExceeded, cfg.ID, cfg.MaxInstances)
		}
		m.mu.Unlock()
	}

	inst, err := newInstance(ctx, cfg, req, m.permissions, m.notifyObserver)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if enforceOwnerFingerprint && !m.ownerFingerprintCurrentLocked(cfg.ID, fingerprint) {
		m.mu.Unlock()
		_ = inst.close()
		return nil, fmt.Errorf("%w: owner %q", ErrRuntimeConfigRetired, cfg.ID)
	}
	if cfg.MaxInstances > 0 && m.serviceInstanceCountLocked(cfg.ID) >= cfg.MaxInstances {
		m.mu.Unlock()
		_ = inst.close()
		return nil, fmt.Errorf("%w: service %q reached max_instances %d", ErrCapacityExceeded, cfg.ID, cfg.MaxInstances)
	}
	m.instances[scope] = &managedInstance{instance: inst, idleTTL: cfg.IdleTTL, lastUsed: time.Now().UTC(), fingerprint: fingerprint}
	m.mu.Unlock()
	return inst, nil
}

func (m *Manager) ownerFingerprintCurrentLocked(ownerID, fingerprint string) bool {
	current, ok := m.ownerFingerprints[strings.TrimSpace(ownerID)]
	return ok && current != "" && current == fingerprint
}

func (m *Manager) serviceInstanceCountLocked(serviceID string) int {
	count := 0
	for scope, item := range m.instances {
		if item == nil || item.instance == nil {
			continue
		}
		parts := strings.Split(scope, scopeSep)
		if len(parts) == 5 && parts[0] == serviceID {
			count++
		}
	}
	return count
}

// adoptSessionInstanceLocked rebinds the thread's empty-session-scope instance
// to a session-addressed scope when the requested session id is the one that
// instance is already bound to. This keeps one live agent process serving the
// thread when a client switches from thread-only addressing (turn 1, no
// session_id) to explicit session addressing (later turns echo back the
// session id from the session event), instead of spawning a second process and
// replaying session/load. The caller must hold m.mu.
func (m *Manager) adoptSessionInstanceLocked(scope string, fingerprint string, cfg acpservice.ServiceConfig, req TurnRequest) *instance {
	if req.FreshSession || req.SessionID == "" {
		return nil
	}
	cwd := strings.TrimSpace(req.CWD)
	if cwd == "" {
		cwd = cfg.CWD
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = cfg.DefaultModel
	}
	from := buildScope(cfg.ID, cwd, req.ThreadID, "", model)
	item := m.instances[from]
	if item == nil || item.instance == nil || item.instance.sessionID != req.SessionID {
		return nil
	}
	// Never adopt an instance created under a retired config fingerprint.
	if item.retired || item.fingerprint != fingerprint {
		return nil
	}
	// Never steal an instance from a turn that is still running on the
	// thread-addressed scope, and never adopt a dead process.
	if m.active.IsActive(from) || !item.instance.alive() {
		return nil
	}
	if !m.rebindLocked(from, scope) {
		return nil
	}
	return item.instance
}

// rebindLocked moves a pooled instance from one scope key to another. It is a
// no-op (returning false) when the scopes are equal, the source is absent, or
// the destination is already occupied. The caller must hold m.mu.
func (m *Manager) rebindLocked(fromScope, toScope string) bool {
	if fromScope == toScope {
		return false
	}
	item := m.instances[fromScope]
	if item == nil || item.instance == nil {
		return false
	}
	if existing := m.instances[toScope]; existing != nil && existing.instance != nil {
		return false
	}
	delete(m.instances, fromScope)
	item.lastUsed = time.Now().UTC()
	m.instances[toScope] = item
	return true
}

func (m *Manager) janitor() {
	ticker := time.NewTicker(m.janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.reapIdle(time.Now().UTC())
		}
	}
}

// reapIdle tears down pooled instances that are idle beyond their IdleTTL or
// whose process has already exited. Instances with an active turn are left
// alone; the active turn owns them until it releases the scope.
func (m *Manager) reapIdle(now time.Time) {
	var victims []*instance
	m.mu.Lock()
	for scope, item := range m.instances {
		if item == nil || item.instance == nil {
			delete(m.instances, scope)
			continue
		}
		if !shouldReap(now, item.lastUsed, item.idleTTL, item.instance.alive(), m.active.IsActive(scope), item.retired) {
			continue
		}
		victims = append(victims, item.instance)
		delete(m.instances, scope)
	}
	m.mu.Unlock()
	for _, inst := range victims {
		_ = inst.close()
	}
}

// shouldReap reports whether a pooled instance can be torn down. An instance
// with an active turn is never reaped. A dead transport is always reaped, and
// so is an instance retired by a config-fingerprint change once its in-flight
// turn has drained. A live, idle instance is otherwise reaped only when
// idleTTL > 0 and it has been idle for longer than idleTTL.
func shouldReap(now, lastUsed time.Time, idleTTL time.Duration, alive, active, retired bool) bool {
	if active {
		return false
	}
	if !alive || retired {
		return true
	}
	if idleTTL <= 0 {
		return false
	}
	return now.Sub(lastUsed) > idleTTL
}

type InFlightTurn struct {
	Scope string `json:"scope"`
}

type ActivityTracker struct {
	mu     sync.Mutex
	active map[string]struct{}
}

func NewActivityTracker() *ActivityTracker {
	return &ActivityTracker{active: map[string]struct{}{}}
}

func (t *ActivityTracker) Begin(scope string) (func(), error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return nil, fmt.Errorf("scope is required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.active[scope]; exists {
		return nil, fmt.Errorf("acp turn is already active for scope")
	}
	t.active[scope] = struct{}{}
	return func() {
		t.mu.Lock()
		delete(t.active, scope)
		t.mu.Unlock()
	}, nil
}

func (t *ActivityTracker) IsActive(scope string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.active[strings.TrimSpace(scope)]
	return ok
}

func (t *ActivityTracker) List() []InFlightTurn {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]InFlightTurn, 0, len(t.active))
	for scope := range t.active {
		out = append(out, InFlightTurn{Scope: scope})
	}
	return out
}
