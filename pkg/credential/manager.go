package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/agent-guide/agent-gateway/pkg/configstore"
	"github.com/agent-guide/agent-gateway/pkg/credential/model"
	"github.com/google/uuid"
)

const (
	TypeAPIKey     = "api_key"
	TypeOAuthToken = "oauth_token"

	CredentialScopeProviderTypePrefix = "type:"
	CredentialScopeProviderIDPrefix   = "id:"
)

type Credential = model.Credential
type ManagedCredential = model.ManagedCredential
type QuotaState = model.QuotaState
type ModelState = model.ModelState
type Error = model.Error

func ProviderTypeCredentialScope(providerType string) string {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	if providerType == "" {
		return ""
	}
	return CredentialScopeProviderTypePrefix + providerType
}

func ProviderIDCredentialScope(providerID string) string {
	providerID = strings.ToLower(strings.TrimSpace(providerID))
	if providerID == "" {
		return ""
	}
	return CredentialScopeProviderIDPrefix + providerID
}

type CredentialLifecycleListener interface {
	OnCredentialRegistered(ctx context.Context, cred *ManagedCredential)
	OnCredentialUpdated(ctx context.Context, cred *ManagedCredential)
	OnCredentialDeregistered(ctx context.Context, cred *ManagedCredential)
	OnCredentialsReplaced(ctx context.Context, creds []*ManagedCredential)
}

type Manager struct {
	store configstore.ConfigStore

	mu              sync.RWMutex
	refreshLocksMu  sync.Mutex
	refreshLocks    map[string]*credentialRefreshLock
	refreshFailures map[string]credentialRefreshFailure
	creds           map[string]*ManagedCredential
	listeners       []CredentialLifecycleListener
	refresher       Refresher
}

type credentialRefreshLock struct {
	mu   sync.Mutex
	refs int
}

type credentialRefreshFailure struct {
	err        error
	retryAfter time.Time
}

func NewManager(store configstore.ConfigStore) *Manager {
	m := &Manager{
		store: store,
		creds: make(map[string]*ManagedCredential),
	}
	return m
}

func (m *Manager) AddListener(listener CredentialLifecycleListener) {
	if m == nil || listener == nil {
		return
	}
	m.mu.Lock()
	m.listeners = append(m.listeners, listener)
	m.mu.Unlock()
}

func DecodeCredential(data []byte) (any, error) {
	var c Credential
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("decode credential object: %w", err)
	}
	return c.Normalize(), nil
}

// SetRefresher installs the request-time credential refresh transport. The
// transport decides how the configured refresh command is executed.
func (m *Manager) SetRefresher(refresher Refresher) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.refresher = refresher
	m.mu.Unlock()
	m.clearAllRefreshFailures()
}

func (m *Manager) Refresher() Refresher {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.refresher
}

func (m *Manager) Load(ctx context.Context) error {
	if m == nil || m.store == nil {
		return nil
	}
	items, err := m.store.ListByTagPrefix(ctx, "")
	if err != nil {
		return fmt.Errorf("credential manager: load from store: %w", err)
	}
	for _, item := range items {
		cred, ok := item.(*Credential)
		if !ok || cred == nil {
			return fmt.Errorf("credential manager: unexpected credential type %T", item)
		}
		if err := m.RegisterCredential(WithSkipPersist(ctx), cred); err != nil {
			return fmt.Errorf("credential manager: register credential %s: %w", cred.ID, err)
		}
	}
	return nil
}

func (m *Manager) ReloadFromStore(ctx context.Context) error {
	if m == nil || m.store == nil {
		return nil
	}
	items, err := m.store.ListByTagPrefix(ctx, "")
	if err != nil {
		return fmt.Errorf("credential manager: reload from store: %w", err)
	}
	reloaded := make(map[string]*ManagedCredential, len(items))
	for _, item := range items {
		cred, ok := item.(*Credential)
		if !ok || cred == nil {
			return fmt.Errorf("credential manager: unexpected credential type %T", item)
		}
		normalized := cred.Normalize().Clone()
		if normalized.ID == "" {
			return fmt.Errorf("credential manager: credential has empty id")
		}
		reloaded[normalized.ID] = &ManagedCredential{Credential: *normalized}
	}

	m.mu.Lock()
	m.creds = reloaded
	m.mu.Unlock()
	m.clearAllRefreshFailures()

	m.notifyReplaced(ctx, managedCredentialMapSnapshot(reloaded))
	return nil
}

func (m *Manager) RegisterCredential(ctx context.Context, cred *Credential) error {
	if m == nil {
		return fmt.Errorf("credential manager: manager is nil")
	}
	if cred == nil {
		return fmt.Errorf("credential manager: credential is nil")
	}
	cred = cred.Normalize()
	applyDefaultCredentialScope(cred)
	if err := cred.Validate(); err != nil {
		return fmt.Errorf("credential manager: %w", err)
	}

	original := cred
	cred = cred.Clone()
	if cred.ID == "" {
		cred.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	if cred.CreatedAt.IsZero() {
		cred.CreatedAt = now
	}
	cred.UpdatedAt = now

	if !shouldSkipPersist(ctx) {
		if err := m.createOrUpdate(ctx, cred); err != nil {
			return err
		}
	}

	managed := &ManagedCredential{Credential: *cred}
	m.mu.Lock()
	m.creds[cred.ID] = managed
	m.mu.Unlock()
	m.clearRefreshFailure(cred.ID)

	m.notifyRegistered(ctx, managed)
	original.ID = cred.ID
	original.CreatedAt = cred.CreatedAt
	original.UpdatedAt = cred.UpdatedAt
	return nil
}

func (m *Manager) UpdateCredential(ctx context.Context, cred *Credential) error {
	if m == nil {
		return fmt.Errorf("credential manager: manager is nil")
	}
	if cred == nil {
		return fmt.Errorf("credential manager: credential is nil")
	}

	cred = cred.Clone().Normalize()
	applyDefaultCredentialScope(cred)
	if err := cred.Validate(); err != nil {
		return fmt.Errorf("credential manager: %w", err)
	}
	cred.UpdatedAt = time.Now().UTC()
	if !shouldSkipPersist(ctx) {
		if err := m.update(ctx, cred); err != nil {
			return err
		}
	}

	m.mu.Lock()
	managed := mergeManagedCredentialLocked(m.creds[cred.ID], cred)
	m.creds[cred.ID] = managed
	m.mu.Unlock()
	m.clearRefreshFailure(cred.ID)

	m.notifyUpdated(ctx, managed)
	return nil
}

func (m *Manager) DeregisterCredential(ctx context.Context, id string) error {
	if m == nil {
		return nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("credential manager: id is empty")
	}

	m.mu.Lock()
	cred := m.creds[id]
	_, ok := m.creds[id]
	delete(m.creds, id)
	m.mu.Unlock()
	m.clearRefreshFailure(id)
	if !ok {
		return nil
	}

	if !shouldSkipPersist(ctx) && m.store != nil {
		if err := m.store.Delete(ctx, id); err != nil {
			return fmt.Errorf("credential manager: delete from store: %w", err)
		}
	}
	if cred != nil {
		m.notifyDeregistered(ctx, cred)
	}
	return nil
}

func applyDefaultCredentialScope(cred *Credential) {
	if cred == nil || cred.ScopeValue() != "" {
		return
	}
	cred.Scope = ProviderIDCredentialScope(cred.ProviderID)
}

func (m *Manager) GetCredential(id string) *ManagedCredential {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if cred := m.creds[id]; cred != nil {
		return cred.Clone()
	}
	return nil
}

func (m *Manager) ListCredentials(filter Filter) []*ManagedCredential {
	if m == nil {
		return nil
	}
	providerType := strings.ToLower(strings.TrimSpace(filter.ProviderType))
	providerID := strings.ToLower(strings.TrimSpace(filter.ProviderID))
	credentialType := strings.ToLower(strings.TrimSpace(filter.Type))

	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*ManagedCredential, 0, len(m.creds))
	for _, cred := range m.creds {
		if cred == nil {
			continue
		}
		if providerType != "" && strings.ToLower(cred.ProviderType) != providerType {
			continue
		}
		if providerID != "" && strings.ToLower(cred.ProviderID) != providerID {
			continue
		}
		if credentialType != "" && strings.ToLower(cred.Type) != credentialType {
			continue
		}
		out = append(out, cred.Clone())
	}
	return out
}

func (m *Manager) RefreshCredentialIfNeeded(ctx context.Context, credID string) (*ManagedCredential, error) {
	if m == nil {
		return nil, &Error{Code: "manager_nil", Message: "credential manager not initialized"}
	}
	credID = strings.TrimSpace(credID)
	if credID == "" {
		return nil, &Error{Code: "credential_id_empty", Message: "credential id is empty"}
	}

	m.mu.RLock()
	stored := m.creds[credID]
	m.mu.RUnlock()
	if stored == nil {
		return nil, &Error{Code: "credential_not_found", Message: "credential not found"}
	}

	current := stored.Clone()
	if !credentialNeedsRefresh(&current.Credential, time.Now().UTC()) {
		return current, nil
	}

	// Serialize rotation of one credential's refresh token, while allowing
	// unrelated credentials to refresh concurrently. Re-read after acquiring
	// the lock because another request may already have rotated this token.
	unlockRefresh := m.lockCredentialRefresh(credID)
	defer unlockRefresh()
	m.mu.RLock()
	stored = m.creds[credID]
	m.mu.RUnlock()
	if stored == nil {
		return nil, &Error{Code: "credential_not_found", Message: "credential not found"}
	}
	current = stored.Clone()
	if !credentialNeedsRefresh(&current.Credential, time.Now().UTC()) {
		return current, nil
	}
	if err := m.cachedRefreshFailure(credID, time.Now().UTC()); err != nil {
		return nil, err
	}

	refresher := m.Refresher()
	if refresher == nil {
		err := fmt.Errorf("credential manager: credential %q requires refresh but no refresher is configured", current.ID)
		m.cacheRefreshFailure(credID, err)
		return nil, err
	}

	// Refresh-token rotation and persistence form one critical operation. Once
	// it starts, a disconnected client must not cancel either half: the
	// upstream may already have consumed the old refresh token by the time the
	// request context is canceled. The refresher still applies its own bounded
	// execution timeout.
	refreshCtx := context.WithoutCancel(ctx)
	updated, err := refresher.Refresh(refreshCtx, current.Credential.Clone())
	if err != nil {
		m.cacheRefreshFailure(credID, err)
		return nil, err
	}
	if updated == nil {
		return current, nil
	}

	updated = updated.Clone().Normalize()
	// The external refresher may update token material and descriptive
	// metadata, but it cannot move or reclassify the persisted credential.
	updated.ID = current.ID
	updated.ProviderType = current.ProviderType
	updated.ProviderID = current.ProviderID
	updated.Type = current.Type
	updated.Scope = current.Scope
	updated.Disabled = current.Disabled
	updated.CreatedAt = current.CreatedAt
	if updated.Label == "" {
		updated.Label = current.Label
	}
	updated.Metadata = mergeCredentialMetadata(current.Metadata, updated.Metadata)
	if err := m.UpdateCredential(refreshCtx, updated); err != nil {
		m.cacheRefreshFailure(credID, err)
		return nil, err
	}
	return m.GetCredential(updated.ID), nil
}

func (m *Manager) cachedRefreshFailure(credID string, now time.Time) error {
	m.refreshLocksMu.Lock()
	defer m.refreshLocksMu.Unlock()
	failure, ok := m.refreshFailures[credID]
	if !ok {
		return nil
	}
	if !failure.retryAfter.After(now) {
		delete(m.refreshFailures, credID)
		return nil
	}
	return failure.err
}

func (m *Manager) cacheRefreshFailure(credID string, err error) {
	if err == nil {
		return
	}
	m.refreshLocksMu.Lock()
	if m.refreshFailures == nil {
		m.refreshFailures = make(map[string]credentialRefreshFailure)
	}
	m.refreshFailures[credID] = credentialRefreshFailure{
		err:        err,
		retryAfter: time.Now().UTC().Add(RefreshFailureCooldown),
	}
	m.refreshLocksMu.Unlock()
}

func (m *Manager) clearRefreshFailure(credID string) {
	m.refreshLocksMu.Lock()
	delete(m.refreshFailures, credID)
	m.refreshLocksMu.Unlock()
}

func (m *Manager) clearAllRefreshFailures() {
	m.refreshLocksMu.Lock()
	clear(m.refreshFailures)
	m.refreshLocksMu.Unlock()
}

func mergeCredentialMetadata(current, updated map[string]any) map[string]any {
	if len(current) == 0 && len(updated) == 0 {
		return nil
	}
	merged := make(map[string]any, len(current)+len(updated))
	for key, value := range current {
		merged[key] = value
	}
	for key, value := range updated {
		merged[key] = value
	}
	return merged
}

func (m *Manager) lockCredentialRefresh(credID string) func() {
	m.refreshLocksMu.Lock()
	if m.refreshLocks == nil {
		m.refreshLocks = make(map[string]*credentialRefreshLock)
	}
	lock := m.refreshLocks[credID]
	if lock == nil {
		lock = &credentialRefreshLock{}
		m.refreshLocks[credID] = lock
	}
	lock.refs++
	m.refreshLocksMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		m.refreshLocksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(m.refreshLocks, credID)
		}
		m.refreshLocksMu.Unlock()
	}
}

func (m *Manager) create(ctx context.Context, cred *Credential) error {
	if m.store == nil {
		return nil
	}
	if err := m.store.Create(ctx, cred); err != nil {
		return fmt.Errorf("credential manager: create credential %s: %w", cred.ID, err)
	}
	return nil
}

func (m *Manager) createOrUpdate(ctx context.Context, cred *Credential) error {
	if m.store == nil {
		return nil
	}
	if err := m.create(ctx, cred); err == nil {
		return nil
	}
	return m.update(ctx, cred)
}

func (m *Manager) update(ctx context.Context, cred *Credential) error {
	if m.store == nil {
		return nil
	}
	if err := m.store.Update(ctx, cred); err != nil {
		return fmt.Errorf("credential manager: update credential %s: %w", cred.ID, err)
	}
	return nil
}

func mergeManagedCredentialLocked(existing *ManagedCredential, spec *Credential) *ManagedCredential {
	managed := &ManagedCredential{Credential: *spec.Clone()}
	if existing == nil {
		return managed
	}
	managed.Unavailable = existing.Unavailable
	managed.NextRetryAfter = existing.NextRetryAfter
	managed.Quota = existing.Quota
	managed.AuthInvalid = existing.AuthInvalid
	managed.StateUpdatedAt = existing.StateUpdatedAt
	if existing.LastError != nil {
		managed.LastError = &Error{
			Code:       existing.LastError.Code,
			Message:    existing.LastError.Message,
			Retryable:  existing.LastError.Retryable,
			HTTPStatus: existing.LastError.HTTPStatus,
		}
	}
	if len(existing.ModelStates) > 0 {
		managed.ModelStates = make(map[string]*ModelState, len(existing.ModelStates))
		for k, v := range existing.ModelStates {
			managed.ModelStates[k] = v.Clone()
		}
	}
	return managed
}

func (m *Manager) listenersSnapshot() []CredentialLifecycleListener {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.listeners) == 0 {
		return nil
	}
	out := make([]CredentialLifecycleListener, len(m.listeners))
	copy(out, m.listeners)
	return out
}

func (m *Manager) notifyRegistered(ctx context.Context, cred *ManagedCredential) {
	for _, listener := range m.listenersSnapshot() {
		listener.OnCredentialRegistered(ctx, cred)
	}
}

func (m *Manager) notifyUpdated(ctx context.Context, cred *ManagedCredential) {
	for _, listener := range m.listenersSnapshot() {
		listener.OnCredentialUpdated(ctx, cred)
	}
}

func (m *Manager) notifyDeregistered(ctx context.Context, cred *ManagedCredential) {
	for _, listener := range m.listenersSnapshot() {
		listener.OnCredentialDeregistered(ctx, cred)
	}
}

func (m *Manager) notifyReplaced(ctx context.Context, creds []*ManagedCredential) {
	for _, listener := range m.listenersSnapshot() {
		listener.OnCredentialsReplaced(ctx, creds)
	}
}

func managedCredentialMapSnapshot(creds map[string]*ManagedCredential) []*ManagedCredential {
	if len(creds) == 0 {
		return nil
	}
	out := make([]*ManagedCredential, 0, len(creds))
	for _, cred := range creds {
		if cred == nil {
			continue
		}
		out = append(out, cred.Clone())
	}
	return out
}
