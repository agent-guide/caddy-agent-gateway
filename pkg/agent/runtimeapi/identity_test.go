package runtimeapi

import (
	"context"
	"sync"
	"testing"
)

func TestNewRunIDFormatAndUniqueness(t *testing.T) {
	const count = 1000
	seen := make(map[string]struct{}, count)
	for range count {
		id, err := NewRunID()
		if err != nil {
			t.Fatalf("NewRunID() error = %v", err)
		}
		if !ValidRunID(id) {
			t.Fatalf("NewRunID() = %q, want valid run id", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate run id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestIdentityContextMerge(t *testing.T) {
	ctx := WithIdentities(context.Background(), Identities{TraceID: "trace", SpanID: "span", AgentID: "old"})
	ctx = MergeIdentities(ctx, Identities{AgentID: "agent", RuntimeType: "builtin", RunID: "run", SegmentIndex: 2})
	got, ok := IdentitiesFromContext(ctx)
	if !ok {
		t.Fatal("IdentitiesFromContext() missing")
	}
	want := Identities{TraceID: "trace", SpanID: "span", AgentID: "agent", RuntimeType: "builtin", RunID: "run", SegmentIndex: 2}
	if got != want {
		t.Fatalf("identities = %+v, want %+v", got, want)
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ids, ok := IdentitiesFromContext(ctx); !ok || ids.RunID != "run" {
				t.Errorf("concurrent context read = %+v, %v", ids, ok)
			}
		}()
	}
	wg.Wait()
}

func TestRestoreRunSequencerRejectsInvalidIdentity(t *testing.T) {
	if _, err := RestoreRunSequencer("agent", "fake", EventCursor{RunID: "not-a-run"}); err == nil {
		t.Fatal("RestoreRunSequencer() accepted invalid run id")
	}
	if _, err := RestoreRunSequencer("", "fake", EventCursor{RunID: "run-0123456789abcdef0123456789abcdef"}); err == nil {
		t.Fatal("RestoreRunSequencer() accepted empty agent id")
	}
}
