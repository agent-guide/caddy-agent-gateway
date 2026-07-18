package builtin

import (
	"context"
	"fmt"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// Reusing an existing session while the agent is at the session cap must never
// evict the reused session (which used to lose its history silently: the
// pre-lookup cap sweep evicted the oldest idle session, recreated it with the
// same id, and the turn continued on an empty history) nor any neighbor.
func TestSessionReuseAtCapKeepsHistory(t *testing.T) {
	store := newSessionStore()
	ctx := context.Background()

	h, _, err := store.begin(ctx, "triage", "first")
	if err != nil {
		t.Fatalf("begin(first) error = %v", err)
	}
	h.commit([]*schema.Message{schema.UserMessage("hello"), schema.AssistantMessage("hi", nil)})

	// Fill the agent to the cap; "first" is now the oldest idle session.
	for i := 1; i < maxSessionsPerAgent; i++ {
		h, _, err := store.begin(ctx, "triage", fmt.Sprintf("filler-%03d", i))
		if err != nil {
			t.Fatalf("begin(filler %d) error = %v", i, err)
		}
		h.release()
	}
	if got := store.sessionCount("triage"); got != maxSessionsPerAgent {
		t.Fatalf("sessionCount = %d, want %d", got, maxSessionsPerAgent)
	}

	h2, history, err := store.begin(ctx, "triage", "first")
	if err != nil {
		t.Fatalf("begin(first) at cap error = %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history = %d messages, want 2 (reuse at cap must not drop the session)", len(history))
	}
	h2.release()
	if got := store.sessionCount("triage"); got != maxSessionsPerAgent {
		t.Fatalf("sessionCount after reuse = %d, want %d (reuse must not evict a neighbor)", got, maxSessionsPerAgent)
	}

	// A genuinely new session at the cap still evicts exactly one idle session.
	h3, _, err := store.begin(ctx, "triage", "brand-new")
	if err != nil {
		t.Fatalf("begin(brand-new) at cap error = %v", err)
	}
	h3.release()
	if got := store.sessionCount("triage"); got != maxSessionsPerAgent {
		t.Fatalf("sessionCount after create = %d, want %d", got, maxSessionsPerAgent)
	}
}

func TestPendingPermissionSessionIsNotEvictedAtCap(t *testing.T) {
	store := newSessionStore()
	ctx := context.Background()

	h, _, err := store.begin(ctx, "triage", "pending")
	if err != nil {
		t.Fatalf("begin(pending) error = %v", err)
	}
	h.commit([]*schema.Message{schema.UserMessage("remember me")})
	store.setPendingPermission("triage", "pending", true)

	for i := 1; i < maxSessionsPerAgent; i++ {
		h, _, err := store.begin(ctx, "triage", fmt.Sprintf("filler-%03d", i))
		if err != nil {
			t.Fatalf("begin(filler %d) error = %v", i, err)
		}
		h.release()
	}

	// Creating another session at the cap must evict an ordinary idle session,
	// never the suspended permission session.
	h, _, err = store.begin(ctx, "triage", "brand-new")
	if err != nil {
		t.Fatalf("begin(brand-new) error = %v", err)
	}
	h.release()

	h, history, err := store.begin(ctx, "triage", "pending")
	if err != nil {
		t.Fatalf("begin(pending) after cap eviction error = %v", err)
	}
	defer h.release()
	if len(history) != 1 || history[0].Content != "remember me" {
		t.Fatalf("pending history = %+v, want the pinned session history", history)
	}
}
