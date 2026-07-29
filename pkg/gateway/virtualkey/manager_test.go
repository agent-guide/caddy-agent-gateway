package virtualkey

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/configstore"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type testManagedVirtualKeyStore struct {
	items    map[string]*VirtualKey
	getCalls int
}

func (s *testManagedVirtualKeyStore) List(ctx context.Context) ([]any, error) {
	return s.ListByTag(ctx, "")
}

func (s *testManagedVirtualKeyStore) ListByTag(_ context.Context, tag string) ([]any, error) {
	out := make([]any, 0, len(s.items))
	for _, item := range s.items {
		if tag != "" && item.Tag != tag {
			continue
		}
		cloned := *item
		out = append(out, &cloned)
	}
	return out, nil
}

func (s *testManagedVirtualKeyStore) ListByTagPrefix(ctx context.Context, tagPrefix string) ([]any, error) {
	return s.ListByTag(ctx, tagPrefix)
}

func (s *testManagedVirtualKeyStore) Create(_ context.Context, obj any) error {
	if unwrapper, ok := obj.(interface{ ConfigStoreObject() any }); ok {
		obj = unwrapper.ConfigStoreObject()
	}
	item, ok := obj.(*VirtualKey)
	if !ok {
		return errors.New("unexpected type")
	}
	if s.items == nil {
		s.items = map[string]*VirtualKey{}
	}
	cloned := *item
	s.items[cloned.ID] = &cloned
	return nil
}

func (s *testManagedVirtualKeyStore) Update(_ context.Context, obj any) error {
	item, ok := obj.(*VirtualKey)
	if !ok {
		return errors.New("unexpected type")
	}
	if _, ok := s.items[item.ID]; !ok {
		return configstore.ErrNotFound
	}
	return s.Create(context.Background(), obj)
}

func (s *testManagedVirtualKeyStore) Delete(_ context.Context, keyParts ...any) error {
	id, _ := keyParts[0].(string)
	delete(s.items, id)
	return nil
}

func (s *testManagedVirtualKeyStore) Get(_ context.Context, keyParts ...any) (any, error) {
	s.getCalls++
	id, _ := keyParts[0].(string)
	item, ok := s.items[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	cloned := *item
	return &cloned, nil
}

func (s *testManagedVirtualKeyStore) GetByIndex(_ context.Context, indexName string, value any) (any, error) {
	s.getCalls++
	key, _ := value.(string)
	for _, item := range s.items {
		if item.Key == key {
			cloned := *item
			return &cloned, nil
		}
	}
	return nil, configstore.ErrNotFound
}

func TestVirtualKeyManagerGetCachesDynamicKey(t *testing.T) {
	store := &testManagedVirtualKeyStore{
		items: map[string]*VirtualKey{
			"vk-test": {ID: "vk-test", Key: "lk-test", Tag: "admin"},
		},
	}
	manager := NewVirtualKeyManager(store)

	got, err := manager.GetByKey(context.Background(), "lk-test")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got.Tag != "admin" {
		t.Fatalf("Tag = %q, want admin", got.Tag)
	}

	if _, err := manager.GetByKey(context.Background(), "lk-test"); err != nil {
		t.Fatalf("second Get returned error: %v", err)
	}
	if store.getCalls != 1 {
		t.Fatalf("store get calls = %d, want 1", store.getCalls)
	}
}

func TestVirtualKeyManagerCreateUpdateDeleteManageCache(t *testing.T) {
	store := &testManagedVirtualKeyStore{items: map[string]*VirtualKey{}}
	manager := NewVirtualKeyManager(store)

	if err := manager.Create(context.Background(), VirtualKey{
		ID:  "vk-test",
		Key: "lk-test",
		Tag: "created",
	}); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	store.items["vk-test"].Tag = "stale-store-value"
	got, err := manager.GetByID(context.Background(), "vk-test")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got.Tag != "created" {
		t.Fatalf("Tag = %q, want created", got.Tag)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("CreatedAt is zero after create")
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt is zero after create")
	}
	createdAt := got.CreatedAt
	firstUpdatedAt := got.UpdatedAt

	if err := manager.Update(context.Background(), "vk-test", VirtualKey{
		Tag: "updated",
	}); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	got, err = manager.GetByID(context.Background(), "vk-test")
	if err != nil {
		t.Fatalf("Get after update returned error: %v", err)
	}
	if got.Tag != "updated" {
		t.Fatalf("Tag = %q, want updated", got.Tag)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt changed on update: got %v want %v", got.CreatedAt, createdAt)
	}
	if got.UpdatedAt.Before(firstUpdatedAt) {
		t.Fatalf("UpdatedAt moved backwards: got %v want >= %v", got.UpdatedAt, firstUpdatedAt)
	}
	if got.UpdatedAt.Equal(time.Time{}) {
		t.Fatal("UpdatedAt is zero after update")
	}

	if err := manager.Delete(context.Background(), "vk-test"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if _, err := manager.GetByID(context.Background(), "vk-test"); !errors.Is(err, ErrVirtualKeyNotConfigured) {
		t.Fatalf("Get after delete error = %v, want ErrVirtualKeyNotConfigured", err)
	}
}

func TestVirtualKeyManagerReadBoundariesDeepClone(t *testing.T) {
	storePolicy := &VirtualKeyRateLimits{LLM: &RateLimit{RequestsPerMinute: 60, Burst: 2}}
	store := &testManagedVirtualKeyStore{items: map[string]*VirtualKey{
		"vk-test": {
			ID: "vk-test", Key: "secret", AllowedRouteIDs: []string{"route-a"},
			RateLimits: storePolicy,
		},
	}}
	manager := NewVirtualKeyManager(store)

	first, err := manager.GetByKey(context.Background(), "secret")
	if err != nil {
		t.Fatal(err)
	}
	first.AllowedRouteIDs[0] = "mutated"
	first.RateLimits.LLM.Burst = 99

	second, err := manager.GetByID(context.Background(), "vk-test")
	if err != nil {
		t.Fatal(err)
	}
	if second.AllowedRouteIDs[0] != "route-a" || second.RateLimits.LLM.Burst != 2 {
		t.Fatalf("GetByID shared mutable state: %+v", second)
	}
	items, err := manager.List(context.Background(), VirtualKeyListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	items[0].RateLimits.LLM.Burst = 77
	third, err := manager.GetByID(context.Background(), "vk-test")
	if err != nil {
		t.Fatal(err)
	}
	if third.RateLimits.LLM.Burst != 2 || storePolicy.LLM.Burst != 2 {
		t.Fatalf("List/cache/store shared policy pointers: third=%+v store=%+v", third.RateLimits, storePolicy)
	}
}

func TestVirtualKeyRateLimiterDimensionsAndRefill(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000, 0)}
	manager := NewVirtualKeyManagerWithClock(nil, clock)
	key := VirtualKey{
		ID: "vk-a",
		RateLimits: &VirtualKeyRateLimits{
			LLM:   &RateLimit{RequestsPerMinute: 60, Burst: 1},
			MCP:   &RateLimit{RequestsPerMinute: 60, Burst: 1},
			Agent: &RateLimit{RequestsPerMinute: 60, Burst: 1},
		},
	}
	for _, dimension := range []RateLimitDimension{
		RateLimitDimensionLLM, RateLimitDimensionMCP,
		RateLimitDimensionAgent,
	} {
		got, err := manager.Admit(key, dimension)
		if err != nil || !got.Allowed {
			t.Fatalf("first Admit(%s) = %+v, %v", dimension, got, err)
		}
		got, err = manager.Admit(key, dimension)
		if err != nil || got.Allowed || got.RetryAfterSeconds != 1 {
			t.Fatalf("second Admit(%s) = %+v, %v", dimension, got, err)
		}
		for range 10 {
			if denied, denyErr := manager.Admit(key, dimension); denyErr != nil || denied.Allowed {
				t.Fatalf("repeated denied Admit(%s) = %+v, %v", dimension, denied, denyErr)
			}
		}
	}
	clock.Advance(time.Second)
	got, err := manager.Admit(key, RateLimitDimensionLLM)
	if err != nil || !got.Allowed {
		t.Fatalf("Admit after refill = %+v, %v", got, err)
	}
}

func TestVirtualKeyRateLimiterIsolationAndUnlimited(t *testing.T) {
	manager := NewVirtualKeyManager(nil)
	unlimited, err := manager.Admit(VirtualKey{ID: "unlimited"}, RateLimitDimensionLLM)
	if err != nil || !unlimited.Allowed {
		t.Fatalf("unlimited Admit = %+v, %v", unlimited, err)
	}
	policy := &VirtualKeyRateLimits{LLM: &RateLimit{RequestsPerMinute: 1, Burst: 1}}
	for _, id := range []string{"vk-a", "vk-b"} {
		got, err := manager.Admit(VirtualKey{ID: id, RateLimits: policy}, RateLimitDimensionLLM)
		if err != nil || !got.Allowed {
			t.Fatalf("first Admit(%s) = %+v, %v", id, got, err)
		}
	}
}

func TestVirtualKeyRateLimiterConcurrentBurst(t *testing.T) {
	manager := NewVirtualKeyManager(nil)
	key := VirtualKey{
		ID: "vk-a",
		RateLimits: &VirtualKeyRateLimits{
			LLM: &RateLimit{RequestsPerMinute: 1, Burst: 8},
		},
	}
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := manager.Admit(key, RateLimitDimensionLLM)
			if err != nil {
				t.Errorf("Admit returned error: %v", err)
				return
			}
			if got.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 8 {
		t.Fatalf("allowed = %d, want 8", got)
	}
}

func TestVirtualKeyRateLimiterUpdateDeleteAndResetRemoveBuckets(t *testing.T) {
	store := &testManagedVirtualKeyStore{items: map[string]*VirtualKey{}}
	manager := NewVirtualKeyManager(store)
	key := VirtualKey{
		ID: "vk-a", Key: "secret",
		RateLimits: &VirtualKeyRateLimits{LLM: &RateLimit{RequestsPerMinute: 1, Burst: 1}},
	}
	if err := manager.Create(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if got, _ := manager.Admit(key, RateLimitDimensionLLM); !got.Allowed {
		t.Fatal("initial admission denied")
	}
	if got, _ := manager.Admit(key, RateLimitDimensionLLM); got.Allowed {
		t.Fatal("second admission allowed")
	}
	key.RateLimits.LLM.Burst = 2
	if err := manager.Update(context.Background(), key.ID, key); err != nil {
		t.Fatal(err)
	}
	updated, err := manager.GetByID(context.Background(), key.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got, _ := manager.Admit(updated, RateLimitDimensionLLM); !got.Allowed {
			t.Fatal("updated bucket did not reset to new burst")
		}
	}
	if err := manager.Delete(context.Background(), key.ID); err != nil {
		t.Fatal(err)
	}
	if len(manager.limiters.buckets) != 0 {
		t.Fatalf("buckets after delete = %d, want 0", len(manager.limiters.buckets))
	}
	manager.limiters.buckets[limiterKey{virtualKeyID: "other", dimension: RateLimitDimensionLLM}] = &limiterBucket{}
	manager.Reset()
	if len(manager.limiters.buckets) != 0 {
		t.Fatalf("buckets after reset = %d, want 0", len(manager.limiters.buckets))
	}
}

// TestVirtualKeyRateLimiterAgentDimensionDoesNotConsumeLLMOrMCP covers the §7
// requirement that an agent turn's internal LLM and MCP calls do not consume
// ingress buckets when they do not re-enter the dispatcher.
func TestVirtualKeyRateLimiterAgentDimensionDoesNotConsumeLLMOrMCP(t *testing.T) {
	manager := NewVirtualKeyManager(nil)
	key := VirtualKey{
		ID: "vk-a",
		RateLimits: &VirtualKeyRateLimits{
			LLM:   &RateLimit{RequestsPerMinute: 1, Burst: 1},
			MCP:   &RateLimit{RequestsPerMinute: 1, Burst: 1},
			Agent: &RateLimit{RequestsPerMinute: 1, Burst: 1},
		},
	}

	if got, _ := manager.Admit(key, RateLimitDimensionAgent); !got.Allowed {
		t.Fatal("agent admission denied on first turn")
	}
	if got, _ := manager.Admit(key, RateLimitDimensionAgent); got.Allowed {
		t.Fatal("second agent admission allowed; agent bucket should be exhausted")
	}

	// LLM and MCP ingress are unaffected by the agent admission. Internal calls
	// likewise spend no ingress tokens unless they re-enter the dispatcher.
	for _, dimension := range []RateLimitDimension{RateLimitDimensionLLM, RateLimitDimensionMCP} {
		if got, _ := manager.Admit(key, dimension); !got.Allowed {
			t.Fatalf("ingress Admit(%s) denied; agent turn leaked into ingress", dimension)
		}
	}
	if got, _ := manager.Admit(key, RateLimitDimensionAgent); got.Allowed {
		t.Fatal("agent admission allowed after ingress traffic; buckets cross-contaminated")
	}
}

// TestVirtualKeyRateLimiterConcurrentDenialsDoNotConsumeTokens covers the §14
// requirement that concurrent denied admissions consume no tokens and do not
// delay future capacity: once burst is spent, hammering the bucket with denials
// must not borrow against or block the next refill.
func TestVirtualKeyRateLimiterConcurrentDenialsDoNotConsumeTokens(t *testing.T) {
	clock := &fakeClock{now: time.Unix(5_000, 0)}
	manager := NewVirtualKeyManagerWithClock(nil, clock)
	key := VirtualKey{
		ID:         "vk-a",
		RateLimits: &VirtualKeyRateLimits{LLM: &RateLimit{RequestsPerMinute: 60, Burst: 1}},
	}

	// Spend the single burst token.
	if got, _ := manager.Admit(key, RateLimitDimensionLLM); !got.Allowed {
		t.Fatal("initial admission denied")
	}

	// Fire many concurrent denials against the exhausted bucket.
	var wg sync.WaitGroup
	var denied atomic.Int32
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _ := manager.Admit(key, RateLimitDimensionLLM)
			if got.Allowed {
				t.Errorf("concurrent denial was allowed")
				return
			}
			denied.Add(1)
		}()
	}
	wg.Wait()
	if got := denied.Load(); got != 50 {
		t.Fatalf("denied = %d, want 50", got)
	}

	// Advancing the clock by exactly one refill interval must yield exactly one
	// token — no more, because the 50 denials consumed none.
	clock.Advance(time.Second)
	if got, _ := manager.Admit(key, RateLimitDimensionLLM); !got.Allowed {
		t.Fatal("admission after refill denied")
	}
	if got, _ := manager.Admit(key, RateLimitDimensionLLM); got.Allowed {
		t.Fatal("second admission after one-token refill allowed; denials leaked capacity")
	}
}
