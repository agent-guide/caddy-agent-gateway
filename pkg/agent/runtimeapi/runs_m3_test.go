package runtimeapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunRegistryExactCancelAndTerminalIdempotency(t *testing.T) {
	r := NewRunRegistry()
	var aCalls, bCalls atomic.Int32
	if err := r.Begin("agent", "builtin", "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "s1", func(context.Context, CancelMode) error { aCalls.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.Begin("agent", "builtin", "run-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "s2", func(context.Context, CancelMode) error { bCalls.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	result, err := r.Cancel(t.Context(), "agent", CancelRequest{RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Mode: CancelModeForce})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != RunStateCancelled || result.StopReason != StopReasonCancelled || aCalls.Load() != 1 || bCalls.Load() != 0 {
		t.Fatalf("cancel result/calls = %+v/%d/%d", result, aCalls.Load(), bCalls.Load())
	}
	result, err = r.Cancel(t.Context(), "agent", CancelRequest{RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Mode: CancelModeForce})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != RunStateCancelled || aCalls.Load() != 1 {
		t.Fatalf("repeated cancel = %+v, calls=%d", result, aCalls.Load())
	}
	_, err = r.Cancel(t.Context(), "agent", CancelRequest{RunID: "run-cccccccccccccccccccccccccccccccc", Mode: CancelModeForce})
	var normalized *Error
	if !errors.As(err, &normalized) || normalized.Code != ErrorRunNotFound {
		t.Fatalf("unknown cancel error = %v", err)
	}
}

func TestRunRegistryCancelAgentRetriesPreBindCancellation(t *testing.T) {
	r := NewRunRegistry()
	var calls atomic.Int32
	runID := "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := r.Begin("agent", "acp", runID, "", func(context.Context, CancelMode) error {
		if calls.Add(1) < 3 {
			return NewError(ErrorBackendUnavailable, "run cancellation is not ready; retry")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := r.CancelAgent(ctx, "agent"); err != nil {
		t.Fatalf("CancelAgent: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("cancel calls = %d, want 3", calls.Load())
	}
	items := r.List("agent")
	if len(items) != 1 || items[0].State != RunStateCancelled {
		t.Fatalf("runs after cancellation = %+v", items)
	}
}

func TestRunRegistryTombstoneTTLAndCap(t *testing.T) {
	now := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	r := newRunRegistry(10*time.Minute, 2, func() time.Time { return now })
	ids := []string{"run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "run-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "run-cccccccccccccccccccccccccccccccc"}
	for _, id := range ids {
		if err := r.Begin("a", "builtin", id, "", func(context.Context, CancelMode) error { return nil }); err != nil {
			t.Fatal(err)
		}
		r.Complete("a", id, RunStateCompleted, "")
		now = now.Add(time.Second)
	}
	items := r.List("a")
	if len(items) != 2 || items[0].RunID != ids[1] {
		t.Fatalf("cap items = %+v", items)
	}
	now = now.Add(11 * time.Minute)
	if got := r.List("a"); len(got) != 0 {
		t.Fatalf("expired items = %+v", got)
	}
}

type testContinuation struct {
	resolved atomic.Int32
	expired  atomic.Int32
}

func (b *testContinuation) ValidateContinuationDecision(string, PendingPermission, PermissionDecision) error {
	return nil
}

func (b *testContinuation) ResolveContinuation(context.Context, string, PermissionDecision, time.Time) error {
	b.resolved.Add(1)
	return nil
}
func (b *testContinuation) ExpireContinuation(context.Context, string) error {
	b.expired.Add(1)
	return nil
}

func TestPermissionBrokerConcurrentDecisionHasOneWinner(t *testing.T) {
	b := NewPermissionBroker()
	t.Cleanup(func() { b.Close(WithPermissionSource(context.Background(), "test_cleanup")) })
	binding := &testContinuation{}
	id := "perm-test"
	_, err := b.Register(PendingPermission{RequestID: id, AgentID: "a", RuntimeType: "acp", RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExpiresAt: time.Now().Add(time.Minute)}, "cont-test", binding)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 16)
	for range 16 {
		go func() {
			results <- b.Resolve(WithPermissionSource(context.Background(), "test_transport"), "a", PermissionDecision{RequestID: id, Outcome: "cancelled"})
		}()
	}
	wins := 0
	for range 16 {
		if err := <-results; err == nil {
			wins++
		}
	}
	if wins != 1 || binding.resolved.Load() != 1 {
		t.Fatalf("wins/resolves = %d/%d", wins, binding.resolved.Load())
	}
	audits := b.Audits("a")
	if len(audits) != 16 {
		t.Fatalf("audit attempts = %d, want 16", len(audits))
	}
	for _, audit := range audits {
		if audit.Source != "test_transport" {
			t.Fatalf("audit source = %q", audit.Source)
		}
		if audit.RuntimeType != "acp" || audit.RunID != "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Fatalf("audit correlation = %+v", audit)
		}
	}
}

func TestPermissionBrokerLookupRetainsClaimedCorrelation(t *testing.T) {
	b := NewPermissionBroker()
	t.Cleanup(func() { b.Close(WithPermissionSource(context.Background(), "test_cleanup")) })
	binding := &testContinuation{}
	info := PendingPermission{
		RequestID: "perm-correlation", AgentID: "agent-a", RuntimeType: "acp",
		RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SessionID: "session-a", ExpiresAt: time.Now().Add(time.Minute),
	}
	if _, err := b.Register(info, "cont-correlation", binding); err != nil {
		t.Fatal(err)
	}
	assertCorrelation := func(stage string) {
		t.Helper()
		got, ok := b.LookupPermission(info.RequestID)
		if !ok || got.RequestID != info.RequestID || got.AgentID != info.AgentID || got.RuntimeType != info.RuntimeType || got.RunID != info.RunID || got.SessionID != info.SessionID {
			t.Fatalf("%s correlation = %+v, present=%v", stage, got, ok)
		}
	}
	assertCorrelation("pending")
	if err := b.Resolve(t.Context(), info.AgentID, PermissionDecision{RequestID: info.RequestID, Outcome: "allow"}); err != nil {
		t.Fatal(err)
	}
	assertCorrelation("claimed")
}

func TestPermissionBrokerExpiryClaimsFailClosed(t *testing.T) {
	now := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	var nowNanos atomic.Int64
	nowNanos.Store(now.UnixNano())
	b := NewPermissionBroker()
	t.Cleanup(func() { b.Close(WithPermissionSource(context.Background(), "test_cleanup")) })
	b.mu.Lock()
	b.now = func() time.Time { return time.Unix(0, nowNanos.Load()).UTC() }
	b.mu.Unlock()
	binding := &testContinuation{}
	_, err := b.Register(PendingPermission{RequestID: "perm-exp", AgentID: "a", RuntimeType: "builtin", RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExpiresAt: now.Add(time.Minute)}, "cont-exp", binding)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	nowNanos.Store(now.UnixNano())
	if got := b.List("a"); len(got) != 0 {
		t.Fatalf("expired list=%+v", got)
	}
	if binding.expired.Load() != 0 {
		t.Fatalf("expire calls after read=%d, want 0", binding.expired.Load())
	}
	err = b.Resolve(t.Context(), "a", PermissionDecision{RequestID: "perm-exp"})
	var normalized *Error
	if !errors.As(err, &normalized) || normalized.Code != ErrorPermissionExpired {
		t.Fatalf("resolve expired error=%v", err)
	}
	if binding.expired.Load() != 1 {
		t.Fatalf("expire calls after claim=%d, want 1", binding.expired.Load())
	}
}

func TestPermissionBrokerDefaultExpiryUsesInjectedClock(t *testing.T) {
	now := time.Date(2026, 7, 26, 1, 2, 3, 0, time.UTC)
	b := NewPermissionBroker()
	t.Cleanup(func() { b.Close(WithPermissionSource(context.Background(), "test_cleanup")) })
	b.mu.Lock()
	b.now = func() time.Time { return now }
	b.mu.Unlock()
	if _, err := b.Register(PendingPermission{RequestID: "perm-default", AgentID: "a", RuntimeType: "acp", RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "cont-default", &testContinuation{}); err != nil {
		t.Fatal(err)
	}
	items := b.List("a")
	if len(items) != 1 || !items[0].ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("default expiry=%+v", items)
	}
}

func TestPermissionBrokerOwnsAutomaticExpiryCleanup(t *testing.T) {
	b := NewPermissionBroker()
	t.Cleanup(func() { b.Close(WithPermissionSource(context.Background(), "test_cleanup")) })
	binding := &testContinuation{}
	if _, err := b.Register(PendingPermission{
		RequestID: "perm-auto-expire", AgentID: "a", RuntimeType: "builtin",
		RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExpiresAt: time.Now().Add(20 * time.Millisecond),
	}, "cont-auto-expire", binding); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for binding.expired.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("broker did not invoke expiry cleanup")
		case <-ticker.C:
		}
	}
	if got := b.List("a"); len(got) != 0 {
		t.Fatalf("expired pending records = %+v", got)
	}
	audits := b.Audits("a")
	if len(audits) != 1 || audits[0].Source != "expiry" || audits[0].Result != "expired" {
		t.Fatalf("expiry audits = %+v", audits)
	}
}

func TestPermissionBrokerCloseDrainsAndRejectsLateRegistration(t *testing.T) {
	b := NewPermissionBroker()
	binding := &testContinuation{}
	info := PendingPermission{RequestID: "perm-close", AgentID: "a", RuntimeType: "acp", RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := b.Register(info, "cont-close", binding); err != nil {
		t.Fatal(err)
	}
	b.Close(WithPermissionSource(t.Context(), "process_shutdown"))
	if binding.expired.Load() != 1 || len(b.List("a")) != 0 {
		t.Fatalf("close cleanup: expired=%d pending=%d", binding.expired.Load(), len(b.List("a")))
	}
	if _, err := b.Register(info, "cont-late", binding); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("late registration error = %v", err)
	}
	audits := b.Audits("a")
	if len(audits) != 1 || audits[0].Source != "process_shutdown" || audits[0].Result != "cancelled" {
		t.Fatalf("shutdown audits = %+v", audits)
	}
}

func TestPendingPermissionJSONContainsNoNativePayloadSurface(t *testing.T) {
	raw, err := json.Marshal(PendingPermission{RequestID: "perm-public", AgentID: "a", RuntimeType: "acp", RunID: "run-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actions: []PermissionAction{{ActionID: "tool-1", Name: "Write file"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "native") || strings.Contains(string(raw), "details") || strings.Contains(string(raw), "rawInput") {
		t.Fatalf("public permission leaked native fields: %s", raw)
	}
}
