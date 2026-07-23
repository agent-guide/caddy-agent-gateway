package virtualkey

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/configmgr"
	"github.com/agent-guide/agent-gateway/pkg/configstore"
)

var (
	ErrVirtualKeyNotCarried    = errors.New("virtual key is not carried")
	ErrVirtualKeyNotConfigured = errors.New("virtual key is not configured")
)

type VirtualKeyListOptions struct {
	Tag string
}

type VirtualKeyManager struct {
	base  *configmgr.BaseConfigManager[VirtualKey]
	store configstore.ConfigStore

	mu sync.RWMutex

	dynamicKeyIndex map[string]string
	limiters        *limiterRegistry
}

func NewVirtualKeyManager(store configstore.ConfigStore) *VirtualKeyManager {
	return NewVirtualKeyManagerWithClock(store, nil)
}

func NewVirtualKeyManagerWithClock(store configstore.ConfigStore, clock Clock) *VirtualKeyManager {
	return &VirtualKeyManager{
		base: configmgr.NewBaseConfigManager(store, configmgr.Definition[VirtualKey]{
			GetID:  virtualKeyID,
			Decode: decodeVirtualKeyItem,
			Clone:  cloneVirtualKey,
			PrepareCreate: func(key VirtualKey) (any, VirtualKey, error) {
				if key.ID == "" {
					return nil, VirtualKey{}, fmt.Errorf("id is required")
				}
				if key.Key == "" {
					return nil, VirtualKey{}, fmt.Errorf("key is required")
				}
				if err := key.ValidateConfiguration(); err != nil {
					return nil, VirtualKey{}, err
				}
				key.NormalizeTimestamps(time.Now().UTC())
				return storedVirtualKey{key: &key, tag: key.Tag}, key, nil
			},
			PrepareUpdate: func(id string, current VirtualKey, key VirtualKey) (any, VirtualKey, error) {
				if err := key.ValidateConfiguration(); err != nil {
					return nil, VirtualKey{}, err
				}
				key.ID = id
				key.Key = current.Key
				key.CreatedAt = current.CreatedAt
				key.UpdatedAt = time.Now().UTC()
				return &key, key, nil
			},
			MatchesListQuery: func(key VirtualKey, query configmgr.ListQuery) bool {
				return query.Tag == "" || key.Tag == query.Tag
			},
			NotConfiguredErr: func(string) error {
				return ErrVirtualKeyNotConfigured
			},
			StoreNilErr: func() error {
				return fmt.Errorf("virtual key store is not configured")
			},
		}),
		store:           store,
		dynamicKeyIndex: map[string]string{},
		limiters:        newLimiterRegistry(clock),
	}
}

func (m *VirtualKeyManager) Reset() {
	m.base.Reset()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.dynamicKeyIndex = map[string]string{}
	m.limiters.reset()
}

func (m *VirtualKeyManager) GetByKey(ctx context.Context, key string) (VirtualKey, error) {
	if key == "" {
		return VirtualKey{}, ErrVirtualKeyNotCarried
	}

	m.mu.RLock()
	dynamicID, ok := m.dynamicKeyIndex[key]
	store := m.store
	m.mu.RUnlock()

	if ok {
		virtualKey, err := m.base.Get(ctx, dynamicID)
		if err == nil {
			return cloneVirtualKey(virtualKey), nil
		}
	}

	if store == nil {
		return VirtualKey{}, ErrVirtualKeyNotConfigured
	}

	item, err := store.GetByIndex(ctx, "key", key)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			return VirtualKey{}, ErrVirtualKeyNotConfigured
		}
		return VirtualKey{}, fmt.Errorf("load virtual key %q: %w", key, err)
	}

	virtualKey, err := decodeVirtualKeyItem("", item)
	if err != nil {
		return VirtualKey{}, err
	}
	m.base.Cache(virtualKey)
	m.cacheDynamicKey(virtualKey.ID, virtualKey.Key)
	return cloneVirtualKey(virtualKey), nil
}

func (m *VirtualKeyManager) GetByID(ctx context.Context, id string) (VirtualKey, error) {
	if id == "" {
		return VirtualKey{}, fmt.Errorf("id is required")
	}

	virtualKey, err := m.base.Get(ctx, id)
	if err != nil {
		if errors.Is(err, configstore.ErrNotFound) {
			return VirtualKey{}, ErrVirtualKeyNotConfigured
		}
		if errors.Is(err, ErrVirtualKeyNotConfigured) {
			return VirtualKey{}, err
		}
		return VirtualKey{}, fmt.Errorf("load virtual key %q: %w", id, err)
	}
	m.cacheDynamicKey(virtualKey.ID, virtualKey.Key)
	return cloneVirtualKey(virtualKey), nil
}

func (m *VirtualKeyManager) List(ctx context.Context, opts VirtualKeyListOptions) ([]VirtualKey, error) {
	keys, err := m.base.List(ctx, configmgr.ListQuery{Tag: opts.Tag})
	if err != nil {
		return nil, err
	}

	cached := make(map[string]VirtualKey, len(keys))
	for _, key := range keys {
		cached[key.ID] = key
	}
	m.cacheDynamicKeys(cached)
	for i := range keys {
		keys[i] = cloneVirtualKey(keys[i])
	}
	return keys, nil
}

func (m *VirtualKeyManager) Create(ctx context.Context, key VirtualKey) error {
	if err := m.base.Create(ctx, key); err != nil {
		return err
	}
	m.cacheDynamicKey(key.ID, key.Key)
	return nil
}

func (m *VirtualKeyManager) Update(ctx context.Context, id string, key VirtualKey) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}

	current, err := m.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if err := m.base.Update(ctx, id, key); err != nil {
		return err
	}

	key.ID = id
	key.Key = current.Key
	key.CreatedAt = current.CreatedAt
	key.UpdatedAt = time.Now().UTC()
	m.cacheDynamicKey(key.ID, key.Key)
	if !rateLimitsEqual(current.RateLimits, key.RateLimits) {
		m.limiters.remove(id)
	}
	return nil
}

func (m *VirtualKeyManager) Admit(key VirtualKey, dimension RateLimitDimension) (Admission, error) {
	if m == nil || m.limiters == nil {
		return Admission{}, fmt.Errorf("virtual key limiter is not configured")
	}
	return m.limiters.admit(key, dimension)
}

type storedVirtualKey struct {
	key any
	tag string
}

func (k storedVirtualKey) ConfigStoreObject() any {
	return k.key
}

func (k storedVirtualKey) ConfigStoreTag() string {
	return k.tag
}

func (m *VirtualKeyManager) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}

	current, err := m.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if err := m.base.Delete(ctx, id); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.dynamicKeyIndex, current.Key)
	m.limiters.remove(id)
	return nil
}

// cacheDynamicKey indexes a virtual key's bearer value -> id. It takes only the
// immutable scalar identity fields, never the full VirtualKey, so it cannot
// alias the cached object's mutable slices or policy pointers even if the
// caller passes an un-cloned cached value.
func (m *VirtualKeyManager) cacheDynamicKey(id, bearerKey string) {
	if id == "" || bearerKey == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dynamicKeyIndex == nil {
		m.dynamicKeyIndex = map[string]string{}
	}
	m.dynamicKeyIndex[bearerKey] = id
}

func (m *VirtualKeyManager) cacheDynamicKeys(keys map[string]VirtualKey) {
	if len(keys) == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dynamicKeyIndex == nil {
		m.dynamicKeyIndex = map[string]string{}
	}
	for _, key := range keys {
		if key.ID == "" || key.Key == "" {
			continue
		}
		m.dynamicKeyIndex[key.Key] = key.ID
	}
}

func decodeVirtualKeyItem(keyID string, item any) (VirtualKey, error) {
	virtualKey, ok := item.(*VirtualKey)
	if !ok || virtualKey == nil || virtualKey.ID == "" || virtualKey.Key == "" {
		if keyID == "" {
			keyID = "<unknown>"
		}
		return VirtualKey{}, fmt.Errorf("virtual key %q has unexpected type %T", keyID, item)
	}

	cloned := *virtualKey
	if len(cloned.AllowedRouteIDs) > 0 {
		cloned.AllowedRouteIDs = append([]string(nil), cloned.AllowedRouteIDs...)
	}
	cloned.RateLimits = cloneRateLimits(cloned.RateLimits)
	return cloned, nil
}

func cloneVirtualKey(key VirtualKey) VirtualKey {
	if len(key.AllowedRouteIDs) > 0 {
		key.AllowedRouteIDs = append([]string(nil), key.AllowedRouteIDs...)
	}
	key.RateLimits = cloneRateLimits(key.RateLimits)
	return key
}

func cloneRateLimits(limits *VirtualKeyRateLimits) *VirtualKeyRateLimits {
	if limits == nil {
		return nil
	}
	cloned := *limits
	if limits.LLM != nil {
		value := *limits.LLM
		cloned.LLM = &value
	}
	if limits.MCP != nil {
		value := *limits.MCP
		cloned.MCP = &value
	}
	if limits.Agent != nil {
		value := *limits.Agent
		cloned.Agent = &value
	}
	return &cloned
}

func rateLimitsEqual(a, b *VirtualKeyRateLimits) bool {
	if a == nil || b == nil {
		return a == b
	}
	return rateLimitEqual(a.LLM, b.LLM) &&
		rateLimitEqual(a.MCP, b.MCP) &&
		rateLimitEqual(a.Agent, b.Agent)
}

func rateLimitEqual(a, b *RateLimit) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func virtualKeyID(key VirtualKey) string {
	return key.ID
}
