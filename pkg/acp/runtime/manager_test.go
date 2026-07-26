package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/acp/agentspi"
	acpservice "github.com/agent-guide/agent-gateway/pkg/acp/service"
	acptransport "github.com/agent-guide/agent-gateway/pkg/acp/transport"
)

const fakePoolAgent = "fake-pool"

var fakeOpenCount int32

func init() {
	agentspi.Register(fakePoolAgent, func(agentspi.OpenRequest) (agentspi.Agent, error) {
		return &fakePoolAgentImpl{}, nil
	})
}

type fakePoolAgentImpl struct{}

func (a *fakePoolAgentImpl) Name() string { return fakePoolAgent }

func (a *fakePoolAgentImpl) Open(context.Context, acptransport.Handlers) (acptransport.Transport, error) {
	atomic.AddInt32(&fakeOpenCount, 1)
	return newFakePoolTransport(), nil
}

func (a *fakePoolAgentImpl) InitializeParams() map[string]any        { return map[string]any{} }
func (a *fakePoolAgentImpl) SessionNewParams(string) map[string]any  { return map[string]any{} }
func (a *fakePoolAgentImpl) SessionLoadParams(string) map[string]any { return map[string]any{} }

func (a *fakePoolAgentImpl) PromptParams(string, string, string) map[string]any {
	return map[string]any{}
}

func (a *fakePoolAgentImpl) Cancel(context.Context, acptransport.Transport, string) error { return nil }

type fakePoolTransport struct {
	mu    sync.Mutex
	alive bool
}

func newFakePoolTransport() *fakePoolTransport { return &fakePoolTransport{alive: true} }

func (f *fakePoolTransport) Request(_ context.Context, method string, _ any) (json.RawMessage, error) {
	if method == "session/new" {
		return json.RawMessage(`{"sessionId":"sess-1"}`), nil
	}
	return json.RawMessage(`{}`), nil
}

func (f *fakePoolTransport) Notify(string, any) error { return nil }

func (f *fakePoolTransport) Updates(int) (<-chan acptransport.Message, func()) {
	ch := make(chan acptransport.Message, 8)
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}

func (f *fakePoolTransport) Alive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive
}

func (f *fakePoolTransport) Close() error {
	f.kill()
	return nil
}

// kill simulates the agent process exiting without an explicit Close.
func (f *fakePoolTransport) kill() {
	f.mu.Lock()
	f.alive = false
	f.mu.Unlock()
}

func newTestManager() *Manager {
	return &Manager{
		active:      NewActivityTracker(),
		permissions: newPermissionBroker(),
		runs:        newActiveRunRegistry(),
		instances:   map[string]*managedInstance{},
		done:        make(chan struct{}),
	}
}

func testServiceConfig(t *testing.T) acpservice.ServiceConfig {
	t.Helper()
	return acpservice.ServiceConfig{ID: "svc", AgentType: fakePoolAgent, CWD: t.TempDir()}
}

func transportOf(t *testing.T, inst *instance) *fakePoolTransport {
	t.Helper()
	tr, ok := inst.t.(*fakePoolTransport)
	if !ok {
		t.Fatalf("instance transport is %T, want *fakePoolTransport", inst.t)
	}
	return tr
}

func TestResolveInstanceReusesLiveInstance(t *testing.T) {
	atomic.StoreInt32(&fakeOpenCount, 0)
	m := newTestManager()
	cfg := testServiceConfig(t)
	ctx := context.Background()

	first, err := m.resolveInstance(ctx, "scope-a", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("first resolveInstance: %v", err)
	}
	second, err := m.resolveInstance(ctx, "scope-a", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("second resolveInstance: %v", err)
	}
	if first != second {
		t.Fatal("expected the live instance to be reused")
	}
	if got := atomic.LoadInt32(&fakeOpenCount); got != 1 {
		t.Fatalf("agent opened %d times, want 1", got)
	}
}

func TestResolveInstanceEnforcesServiceMaxInstances(t *testing.T) {
	atomic.StoreInt32(&fakeOpenCount, 0)
	m := newTestManager()
	cfg := testServiceConfig(t)
	cfg.MaxInstances = 1
	ctx := context.Background()

	firstScope := buildScope(cfg.ID, cfg.CWD, "t1", "", "")
	if _, err := m.resolveInstance(ctx, firstScope, cfg, TurnRequest{ThreadID: "t1", Input: "hi"}); err != nil {
		t.Fatalf("first resolveInstance: %v", err)
	}

	secondScope := buildScope(cfg.ID, cfg.CWD, "t2", "", "")
	_, err := m.resolveInstance(ctx, secondScope, cfg, TurnRequest{ThreadID: "t2", Input: "hi"})
	if !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("second resolveInstance error = %v, want ErrCapacityExceeded", err)
	}
	m.mu.Lock()
	_, secondPresent := m.instances[secondScope]
	m.mu.Unlock()
	if secondPresent {
		t.Fatal("capacity-rejected instance was stored in the pool")
	}
	if got := atomic.LoadInt32(&fakeOpenCount); got != 1 {
		t.Fatalf("agent opened %d times, want 1", got)
	}
}

func TestResolveInstanceFreshSessionReplacesInstance(t *testing.T) {
	atomic.StoreInt32(&fakeOpenCount, 0)
	m := newTestManager()
	cfg := testServiceConfig(t)
	ctx := context.Background()

	first, err := m.resolveInstance(ctx, "scope-a", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("first resolveInstance: %v", err)
	}
	firstTransport := transportOf(t, first)

	second, err := m.resolveInstance(ctx, "scope-a", cfg, TurnRequest{ThreadID: "t1", Input: "hi", FreshSession: true})
	if err != nil {
		t.Fatalf("fresh resolveInstance: %v", err)
	}
	if first == second {
		t.Fatal("fresh_session must not reuse the pooled instance")
	}
	if firstTransport.Alive() {
		t.Fatal("evicted instance transport was not closed")
	}
	if got := atomic.LoadInt32(&fakeOpenCount); got != 2 {
		t.Fatalf("agent opened %d times, want 2", got)
	}
}

func TestResolveInstanceEvictsDeadInstance(t *testing.T) {
	atomic.StoreInt32(&fakeOpenCount, 0)
	m := newTestManager()
	cfg := testServiceConfig(t)
	ctx := context.Background()

	first, err := m.resolveInstance(ctx, "scope-a", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("first resolveInstance: %v", err)
	}
	transportOf(t, first).kill()

	second, err := m.resolveInstance(ctx, "scope-a", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("second resolveInstance: %v", err)
	}
	if first == second {
		t.Fatal("a dead instance must not be reused")
	}
	if got := atomic.LoadInt32(&fakeOpenCount); got != 2 {
		t.Fatalf("agent opened %d times, want 2", got)
	}
}

// The fake transport binds every new session to "sess-1", so a follow-up turn
// that echoes that id back must adopt the thread-scoped instance.
func TestResolveInstanceAdoptsSessionScopedInstance(t *testing.T) {
	atomic.StoreInt32(&fakeOpenCount, 0)
	m := newTestManager()
	cfg := testServiceConfig(t)
	ctx := context.Background()

	threadScope := buildScope(cfg.ID, cfg.CWD, "t1", "", "")
	first, err := m.resolveInstance(ctx, threadScope, cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("first resolveInstance: %v", err)
	}
	if first.sessionID != "sess-1" {
		t.Fatalf("unexpected fake session id %q", first.sessionID)
	}

	sessionScope := buildScope(cfg.ID, cfg.CWD, "t1", "sess-1", "")
	second, err := m.resolveInstance(ctx, sessionScope, cfg, TurnRequest{ThreadID: "t1", SessionID: "sess-1", Input: "hi"})
	if err != nil {
		t.Fatalf("session-addressed resolveInstance: %v", err)
	}
	if first != second {
		t.Fatal("session-addressed turn must adopt the thread-scoped instance")
	}
	if got := atomic.LoadInt32(&fakeOpenCount); got != 1 {
		t.Fatalf("agent opened %d times, want 1", got)
	}

	m.mu.Lock()
	_, oldPresent := m.instances[threadScope]
	_, newPresent := m.instances[sessionScope]
	m.mu.Unlock()
	if oldPresent || !newPresent {
		t.Fatalf("instance was not rebound: thread scope present=%v, session scope present=%v", oldPresent, newPresent)
	}
}

func TestResolveInstanceDoesNotAdoptMismatchedSession(t *testing.T) {
	atomic.StoreInt32(&fakeOpenCount, 0)
	m := newTestManager()
	cfg := testServiceConfig(t)
	ctx := context.Background()

	threadScope := buildScope(cfg.ID, cfg.CWD, "t1", "", "")
	first, err := m.resolveInstance(ctx, threadScope, cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("first resolveInstance: %v", err)
	}

	otherScope := buildScope(cfg.ID, cfg.CWD, "t1", "other-session", "")
	second, err := m.resolveInstance(ctx, otherScope, cfg, TurnRequest{ThreadID: "t1", SessionID: "other-session", Input: "hi"})
	if err != nil {
		t.Fatalf("second resolveInstance: %v", err)
	}
	if first == second {
		t.Fatal("an instance bound to a different session must not be adopted")
	}
	if got := atomic.LoadInt32(&fakeOpenCount); got != 2 {
		t.Fatalf("agent opened %d times, want 2", got)
	}
}

func TestResolveInstanceDoesNotAdoptActiveOrDeadInstance(t *testing.T) {
	atomic.StoreInt32(&fakeOpenCount, 0)
	m := newTestManager()
	cfg := testServiceConfig(t)
	ctx := context.Background()

	threadScope := buildScope(cfg.ID, cfg.CWD, "t1", "", "")
	first, err := m.resolveInstance(ctx, threadScope, cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("first resolveInstance: %v", err)
	}

	sessionScope := buildScope(cfg.ID, cfg.CWD, "t1", "sess-1", "")
	release, err := m.active.Begin(threadScope)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	busy, err := m.resolveInstance(ctx, sessionScope, cfg, TurnRequest{ThreadID: "t1", SessionID: "sess-1", Input: "hi"})
	if err != nil {
		t.Fatalf("resolveInstance with active sibling: %v", err)
	}
	if busy == first {
		t.Fatal("an instance with an active turn must not be adopted")
	}
	release()
	m.CloseScope(sessionScope)

	transportOf(t, first).kill()
	dead, err := m.resolveInstance(ctx, sessionScope, cfg, TurnRequest{ThreadID: "t1", SessionID: "sess-1", Input: "hi"})
	if err != nil {
		t.Fatalf("resolveInstance with dead sibling: %v", err)
	}
	if dead == first {
		t.Fatal("a dead instance must not be adopted")
	}
}

func TestRebindLocked(t *testing.T) {
	m := newTestManager()
	cfg := testServiceConfig(t)
	ctx := context.Background()

	if _, err := m.resolveInstance(ctx, "scope-a", cfg, TurnRequest{ThreadID: "t1", Input: "hi"}); err != nil {
		t.Fatalf("resolveInstance a: %v", err)
	}
	if _, err := m.resolveInstance(ctx, "scope-b", cfg, TurnRequest{ThreadID: "t1", Input: "hi"}); err != nil {
		t.Fatalf("resolveInstance b: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rebindLocked("scope-a", "scope-a") {
		t.Fatal("rebinding a scope onto itself must be a no-op")
	}
	if m.rebindLocked("missing", "scope-c") {
		t.Fatal("rebinding an absent source must be a no-op")
	}
	if m.rebindLocked("scope-a", "scope-b") {
		t.Fatal("rebinding onto an occupied destination must be a no-op")
	}
	if !m.rebindLocked("scope-a", "scope-c") {
		t.Fatal("rebind to a free destination failed")
	}
	if _, present := m.instances["scope-a"]; present {
		t.Fatal("source scope still present after rebind")
	}
	if _, present := m.instances["scope-c"]; !present {
		t.Fatal("destination scope missing after rebind")
	}
}

func TestReapIdleClosesIdleInstance(t *testing.T) {
	m := newTestManager()
	cfg := testServiceConfig(t)
	ctx := context.Background()

	inst, err := m.resolveInstance(ctx, "scope-idle", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("resolveInstance: %v", err)
	}
	m.mu.Lock()
	m.instances["scope-idle"].idleTTL = time.Millisecond
	m.instances["scope-idle"].lastUsed = time.Now().UTC().Add(-time.Hour)
	m.mu.Unlock()

	m.reapIdle(time.Now().UTC())

	m.mu.Lock()
	_, present := m.instances["scope-idle"]
	m.mu.Unlock()
	if present {
		t.Fatal("idle instance was not reaped")
	}
	if transportOf(t, inst).Alive() {
		t.Fatal("reaped instance transport was not closed")
	}
}

func TestReapIdleSkipsActiveScope(t *testing.T) {
	m := newTestManager()
	cfg := testServiceConfig(t)
	ctx := context.Background()

	if _, err := m.resolveInstance(ctx, "scope-busy", cfg, TurnRequest{ThreadID: "t1", Input: "hi"}); err != nil {
		t.Fatalf("resolveInstance: %v", err)
	}
	m.mu.Lock()
	m.instances["scope-busy"].idleTTL = time.Millisecond
	m.instances["scope-busy"].lastUsed = time.Now().UTC().Add(-time.Hour)
	m.mu.Unlock()

	release, err := m.active.Begin("scope-busy")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer release()

	m.reapIdle(time.Now().UTC())

	m.mu.Lock()
	_, present := m.instances["scope-busy"]
	m.mu.Unlock()
	if !present {
		t.Fatal("an instance with an active turn must not be reaped")
	}
}

func TestManagerCloseTearsDownInstances(t *testing.T) {
	m := NewManager(nil)
	cfg := testServiceConfig(t)
	ctx := context.Background()

	inst, err := m.resolveInstance(ctx, "scope-a", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("resolveInstance: %v", err)
	}

	m.Close()
	if transportOf(t, inst).Alive() {
		t.Fatal("Close did not tear down the pooled instance")
	}
	m.Close() // must be idempotent
}

func TestCloseThreadTearsDownMatchingInstances(t *testing.T) {
	m := newTestManager()
	cfg := testServiceConfig(t)
	ctx := context.Background()

	keep, err := m.resolveInstance(ctx, buildScope("svc", cfg.CWD, "other", "", ""), cfg, TurnRequest{ThreadID: "other", Input: "hi"})
	if err != nil {
		t.Fatalf("resolveInstance keep: %v", err)
	}
	a, err := m.resolveInstance(ctx, buildScope("svc", cfg.CWD, "t1", "s1", ""), cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("resolveInstance a: %v", err)
	}
	b, err := m.resolveInstance(ctx, buildScope("svc", cfg.CWD, "t1", "s2", ""), cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("resolveInstance b: %v", err)
	}

	if closed := m.CloseThread("svc", "t1"); closed != 2 {
		t.Fatalf("CloseThread closed %d instances, want 2", closed)
	}
	if transportOf(t, a).Alive() || transportOf(t, b).Alive() {
		t.Fatal("thread instances were not torn down")
	}
	if !transportOf(t, keep).Alive() {
		t.Fatal("an instance from a different thread was torn down")
	}
}

func TestCloseServiceTearsDownMatchingInstances(t *testing.T) {
	m := newTestManager()
	cfg := testServiceConfig(t)
	ctx := context.Background()

	keepCfg := cfg
	keepCfg.ID = "other"
	keep, err := m.resolveInstance(ctx, buildScope("other", keepCfg.CWD, "t1", "", ""), keepCfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("resolveInstance keep: %v", err)
	}
	a, err := m.resolveInstance(ctx, buildScope("svc", cfg.CWD, "t1", "s1", ""), cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err != nil {
		t.Fatalf("resolveInstance a: %v", err)
	}
	b, err := m.resolveInstance(ctx, buildScope("svc", cfg.CWD, "t2", "s2", ""), cfg, TurnRequest{ThreadID: "t2", Input: "hi"})
	if err != nil {
		t.Fatalf("resolveInstance b: %v", err)
	}

	if closed := m.CloseService("svc"); closed != 2 {
		t.Fatalf("CloseService closed %d instances, want 2", closed)
	}
	if transportOf(t, a).Alive() || transportOf(t, b).Alive() {
		t.Fatal("service instances were not torn down")
	}
	if !transportOf(t, keep).Alive() {
		t.Fatal("an instance from a different service was torn down")
	}
}

// slowInitAgent blocks session/new until the context is cancelled, to exercise
// the initialize timeout.
type slowInitAgent struct{ stubAgent }

func (slowInitAgent) Open(context.Context, acptransport.Handlers) (acptransport.Transport, error) {
	return &slowInitTransport{}, nil
}

type slowInitTransport struct{ closed bool }

func (s *slowInitTransport) Request(ctx context.Context, method string, _ any) (json.RawMessage, error) {
	if method == "session/new" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return json.RawMessage(`{}`), nil
}
func (s *slowInitTransport) Notify(string, any) error { return nil }
func (s *slowInitTransport) Updates(int) (<-chan acptransport.Message, func()) {
	return make(chan acptransport.Message), func() {}
}
func (s *slowInitTransport) Alive() bool  { return !s.closed }
func (s *slowInitTransport) Close() error { s.closed = true; return nil }

func init() {
	agentspi.Register("slow-init", func(agentspi.OpenRequest) (agentspi.Agent, error) {
		return slowInitAgent{}, nil
	})
}

func TestInitializeTimesOut(t *testing.T) {
	prev := initializeTimeout
	initializeTimeout = 100 * time.Millisecond
	defer func() { initializeTimeout = prev }()

	m := newTestManager()
	cfg := testServiceConfig(t)
	cfg.AgentType = "slow-init"

	_, err := m.resolveInstance(context.Background(), "scope", cfg, TurnRequest{ThreadID: "t1", Input: "hi"})
	if err == nil {
		t.Fatal("resolveInstance returned nil error despite a hung session/new")
	}
}

func TestShouldReap(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name     string
		lastUsed time.Time
		idleTTL  time.Duration
		alive    bool
		active   bool
		want     bool
	}{
		{"active is never reaped", now.Add(-time.Hour), time.Millisecond, true, true, false},
		{"dead is always reaped", now, 0, false, false, true},
		{"dead but active stays", now, 0, false, true, false},
		{"live idle disabled stays", now.Add(-time.Hour), 0, true, false, false},
		{"live within ttl stays", now, time.Hour, true, false, false},
		{"live beyond ttl reaped", now.Add(-2 * time.Hour), time.Hour, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldReap(now, tc.lastUsed, tc.idleTTL, tc.alive, tc.active); got != tc.want {
				t.Fatalf("shouldReap = %v, want %v", got, tc.want)
			}
		})
	}
}
