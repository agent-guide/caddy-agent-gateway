package scheduler

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CredentialScheduler manages an in-memory set of credentials and selects
// the best available one for each request.
type CredentialScheduler interface {
	// Pick selects the best available credential matching the given filter.
	// tried is an optional set of credential IDs that have already been attempted.
	Pick(ctx context.Context, filter Filter, tried map[string]struct{}) (*ManagedCredential, error)

	// MarkResult updates credential availability state from a request result.
	MarkResult(ctx context.Context, result Result)

	// SetSelector replaces the active credential selector.
	SetSelector(s CredentialSelector)

	// SetQuotaCooldownDisabled disables quota cooldown handling when true.
	SetQuotaCooldownDisabled(disable bool)

	// RegisterCredential adds a new credential to the scheduler.
	RegisterCredential(cred *ManagedCredential)

	// UpdateCredential synchronizes updated credential state into the scheduler.
	UpdateCredential(cred *ManagedCredential)

	// DeregisterCredential removes a credential from the scheduler.
	DeregisterCredential(id string)

	// Rebuild recreates the complete scheduler state from a credential snapshot.
	Rebuild(creds []*ManagedCredential)
}

type PredicateCredentialFunc func(cred *ManagedCredential) bool

// NewScheduler constructs a CredentialScheduler with the given credential selector.
// If selector is nil, RoundRobinSelector is used.
func NewScheduler(selector CredentialSelector) CredentialScheduler {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	return &authScheduler{
		selector:        selector,
		scopeSchedulers: make(map[string]*scopeScheduler),
		credScopes:      make(map[string]string),
	}
}

// scheduledState describes how a credential participates in a model shard.
type scheduledState int

const (
	scheduledStateReady    scheduledState = iota
	scheduledStateCooldown                // quota exceeded, known reset time
	scheduledStateBlocked                 // unavailable, unknown reset or other
	scheduledStateDisabled                // intentionally disabled
)

type authScheduler struct {
	mu              sync.Mutex
	selector        CredentialSelector
	scopeSchedulers map[string]*scopeScheduler
	credScopes      map[string]string // credID -> scopeKey
	disableQuota    bool
}

const (
	defaultQuotaBackoffBase = time.Second
	defaultQuotaBackoffMax  = 30 * time.Minute
)

type scopeScheduler struct {
	creds       map[string]*ManagedCredential
	modelShards map[string]*modelScheduler
}

type modelScheduler struct {
	modelKey        string
	entries         map[string]*scheduledCred
	priorityOrder   []int
	readyByPriority map[int]*ReadyBucket
	blocked         []*scheduledCred
}

type scheduledCred struct {
	cred        *ManagedCredential
	priority    int
	state       scheduledState
	nextRetryAt time.Time
}

func (s *authScheduler) SetSelector(selector CredentialSelector) {
	if s == nil {
		return
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selector = selector
}

func (s *authScheduler) SetQuotaCooldownDisabled(disable bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disableQuota = disable
}

func (s *authScheduler) Rebuild(creds []*ManagedCredential) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopeSchedulers = make(map[string]*scopeScheduler)
	s.credScopes = make(map[string]string)
	now := time.Now()
	for _, cred := range creds {
		s.upsertLocked(cred, now)
	}
}

func (s *authScheduler) RegisterCredential(cred *ManagedCredential) {
	s.upsert(cred)
}

func (s *authScheduler) OnCredentialRegistered(_ context.Context, cred *ManagedCredential) {
	s.RegisterCredential(cred)
}

func (s *authScheduler) UpdateCredential(cred *ManagedCredential) {
	s.upsert(cred)
}

func (s *authScheduler) OnCredentialUpdated(_ context.Context, cred *ManagedCredential) {
	s.UpdateCredential(cred)
}

func (s *authScheduler) upsert(cred *ManagedCredential) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertLocked(cred, time.Now())
}

func (s *authScheduler) DeregisterCredential(id string) {
	if s == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(id)
}

func (s *authScheduler) OnCredentialDeregistered(_ context.Context, cred *ManagedCredential) {
	if cred == nil {
		return
	}
	s.DeregisterCredential(cred.ID)
}

func (s *authScheduler) OnCredentialsReplaced(_ context.Context, creds []*ManagedCredential) {
	s.Rebuild(creds)
}

func (s *authScheduler) pickCredential(_ context.Context, scopeKey, model string, predicate PredicateCredentialFunc) (*ManagedCredential, error) {
	if s == nil {
		return nil, &Error{Code: "credential_not_found", Message: "no credential available"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	scopeScheduler := s.scopeSchedulers[scopeKey]
	if scopeScheduler == nil {
		return nil, &Error{Code: "credential_not_found", Message: "no credential available"}
	}

	shard := scopeScheduler.ensureModelLocked(model, time.Now())
	if shard == nil {
		return nil, &Error{Code: "credential_not_found", Message: "no credential available"}
	}

	if picked := shard.pickReadyLocked(s.selector, predicate); picked != nil {
		return picked, nil
	}
	return nil, shard.unavailableErrorLocked(scopeKey, model, predicate)
}

func (s *authScheduler) Pick(ctx context.Context, filter Filter, tried map[string]struct{}) (*ManagedCredential, error) {
	if s == nil {
		return nil, &Error{Code: "credential_not_found", Message: "no credential available"}
	}
	scopeKey := filter.CredentialScope
	if scopeKey == "" {
		return nil, &Error{Code: "invalid_credential_scope", Message: "credential_scope cannot be empty"}
	}
	predicate := func(cred *ManagedCredential) bool {
		if cred == nil {
			return false
		}
		if len(tried) > 0 {
			if _, ok := tried[cred.ID]; ok {
				return false
			}
		}
		return matchFilter(cred, filter)
	}
	return s.pickCredentialWithSelector(ctx, scopeKey, filter.Model, filter.Selector, predicate)
}

func (s *authScheduler) pickCredentialWithSelector(_ context.Context, scopeKey, model string, selectorName string, predicate PredicateCredentialFunc) (*ManagedCredential, error) {
	if s == nil {
		return nil, &Error{Code: "credential_not_found", Message: "no credential available"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	scopeScheduler := s.scopeSchedulers[scopeKey]
	if scopeScheduler == nil {
		return nil, &Error{Code: "credential_not_found", Message: "no credential available"}
	}

	shard := scopeScheduler.ensureModelLocked(model, time.Now())
	if shard == nil {
		return nil, &Error{Code: "credential_not_found", Message: "no credential available"}
	}

	selector := s.selector
	switch strings.ToLower(strings.TrimSpace(selectorName)) {
	case "fill_first":
		selector = &FillFirstSelector{}
	case "round_robin", "":
	}
	if picked := shard.pickReadyLocked(selector, predicate); picked != nil {
		return picked, nil
	}
	return nil, shard.unavailableErrorLocked(scopeKey, model, predicate)
}

func (s *authScheduler) MarkResult(_ context.Context, result Result) {
	if s == nil {
		return
	}
	credID := strings.TrimSpace(result.CredentialID)
	if credID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	scopeKey := s.credScopes[credID]
	if scopeKey == "" {
		return
	}
	scopeScheduler := s.scopeSchedulers[scopeKey]
	if scopeScheduler == nil {
		return
	}
	cred := scopeScheduler.creds[credID]
	if cred == nil {
		return
	}

	now := time.Now().UTC()
	changed := false
	if result.Success {
		if cred.Unavailable || cred.LastError != nil || cred.Quota.Exceeded || cred.AuthInvalid {
			cred.Unavailable = false
			cred.LastError = nil
			cred.NextRetryAfter = time.Time{}
			cred.Quota = QuotaState{}
			cred.AuthInvalid = false
			changed = true
		}
		if result.Model != "" {
			if state := cred.ModelStates[result.Model]; state != nil {
				if state.Unavailable || state.LastError != nil || state.Quota.Exceeded || state.AuthInvalid {
					state.Unavailable = false
					state.LastError = nil
					state.NextRetryAfter = time.Time{}
					state.Quota = QuotaState{}
					state.AuthInvalid = false
					state.UpdatedAt = now
					changed = true
				}
			}
		}
	} else if result.Error != nil {
		rerr := result.Error
		changed = true
		if result.CredentialWide {
			cred.CredentialWideUnavailable = true
			if result.RetryAfter != nil {
				cred.CredentialWideNextRetryAfter = now.Add(*result.RetryAfter)
			}
		} else {
			isQuota := rerr.HTTPStatus == http.StatusTooManyRequests
			if isQuota && !s.disableQuota {
				backoffLevel := cred.Quota.BackoffLevel
				backoff := computeBackoff(backoffLevel, defaultQuotaBackoffBase, defaultQuotaBackoffMax)
				if result.RetryAfter != nil && *result.RetryAfter > backoff {
					backoff = *result.RetryAfter
				}
				cred.Quota = QuotaState{
					Exceeded:      true,
					Reason:        rerr.Message,
					NextRecoverAt: now.Add(backoff),
					BackoffLevel:  backoffLevel + 1,
				}
				cred.Unavailable = true
				cred.NextRetryAfter = now.Add(backoff)
			} else if rerr.HTTPStatus == http.StatusUnauthorized || rerr.HTTPStatus == http.StatusForbidden {
				cred.AuthInvalid = true
				cred.LastError = rerr
			} else {
				cred.LastError = rerr
				if result.RetryAfter != nil {
					cred.Unavailable = true
					cred.NextRetryAfter = now.Add(*result.RetryAfter)
				}
			}

			if result.Model != "" {
				if cred.ModelStates == nil {
					cred.ModelStates = make(map[string]*ModelState)
				}
				state := cred.ModelStates[result.Model]
				if state == nil {
					state = &ModelState{}
					cred.ModelStates[result.Model] = state
				}
				state.UpdatedAt = now
				if isQuota && !s.disableQuota {
					state.Quota = cred.Quota
					state.Unavailable = true
					state.NextRetryAfter = cred.NextRetryAfter
				} else if rerr.HTTPStatus == http.StatusUnauthorized || rerr.HTTPStatus == http.StatusForbidden {
					state.AuthInvalid = true
					state.LastError = rerr
				} else {
					state.LastError = rerr
					if result.RetryAfter != nil {
						state.Unavailable = true
						state.NextRetryAfter = now.Add(*result.RetryAfter)
					}
				}
			}
		}
	}

	if changed {
		cred.StateUpdatedAt = now
		for _, shard := range scopeScheduler.modelShards {
			if shard != nil {
				shard.upsertEntryLocked(cred, now)
			}
		}
	}
}

func (s *authScheduler) upsertLocked(cred *ManagedCredential, now time.Time) {
	if cred == nil {
		return
	}
	scopeKey := cred.ScopeValue()
	if scopeKey == "" {
		return
	}
	credID := cred.ID
	if credID == "" || cred.Disabled {
		s.removeLocked(credID)
		return
	}
	if prev := s.credScopes[credID]; prev != "" && prev != scopeKey {
		if prevScopeScheduler := s.scopeSchedulers[prev]; prevScopeScheduler != nil {
			prevScopeScheduler.removeLocked(credID)
		}
	}
	s.credScopes[credID] = scopeKey
	s.ensureScopeLocked(scopeKey).upsertLocked(cred, now)
}

func (s *authScheduler) removeLocked(credID string) {
	if credID == "" {
		return
	}
	if scopeKey := s.credScopes[credID]; scopeKey != "" {
		if scopeScheduler := s.scopeSchedulers[scopeKey]; scopeScheduler != nil {
			scopeScheduler.removeLocked(credID)
		}
		delete(s.credScopes, credID)
	}
}

func (s *authScheduler) ensureScopeLocked(scopeKey string) *scopeScheduler {
	schedulerScope := s.scopeSchedulers[scopeKey]
	if schedulerScope == nil {
		schedulerScope = &scopeScheduler{
			creds:       make(map[string]*ManagedCredential),
			modelShards: make(map[string]*modelScheduler),
		}
		s.scopeSchedulers[scopeKey] = schedulerScope
	}
	return schedulerScope
}

func (p *scopeScheduler) upsertLocked(cred *ManagedCredential, now time.Time) {
	if p == nil || cred == nil {
		return
	}
	p.creds[cred.ID] = cred
	for _, shard := range p.modelShards {
		if shard != nil {
			shard.upsertEntryLocked(cred, now)
		}
	}
}

func (p *scopeScheduler) removeLocked(credID string) {
	if p == nil || credID == "" {
		return
	}
	delete(p.creds, credID)
	for _, shard := range p.modelShards {
		if shard != nil {
			shard.removeEntryLocked(credID)
		}
	}
}

func (p *scopeScheduler) ensureModelLocked(modelKey string, now time.Time) *modelScheduler {
	if p == nil {
		return nil
	}
	if shard, ok := p.modelShards[modelKey]; ok && shard != nil {
		shard.promoteExpiredLocked(now)
		return shard
	}
	shard := &modelScheduler{
		modelKey:        modelKey,
		entries:         make(map[string]*scheduledCred),
		readyByPriority: make(map[int]*ReadyBucket),
	}
	for _, cred := range p.creds {
		if cred != nil {
			shard.upsertEntryLocked(cred, now)
		}
	}
	p.modelShards[modelKey] = shard
	return shard
}

func (m *modelScheduler) upsertEntryLocked(cred *ManagedCredential, now time.Time) {
	if m == nil || cred == nil {
		return
	}
	entry, ok := m.entries[cred.ID]
	if !ok || entry == nil {
		entry = &scheduledCred{}
		m.entries[cred.ID] = entry
	}
	prevCred := entry.cred
	prevState := entry.state
	prevNext := entry.nextRetryAt
	prevPriority := entry.priority

	entry.cred = cred
	entry.priority = cred.Priority()
	entry.nextRetryAt = time.Time{}

	blocked, reason, next := isCredentialBlockedForModel(cred, m.modelKey, now)
	switch {
	case !blocked:
		entry.state = scheduledStateReady
	case reason == blockReasonCooldown:
		entry.state = scheduledStateCooldown
		entry.nextRetryAt = next
	case reason == blockReasonDisabled:
		entry.state = scheduledStateDisabled
	default:
		entry.state = scheduledStateBlocked
		entry.nextRetryAt = next
	}

	// Rebuild indexes only when something changed.
	if ok && prevCred == cred && prevState == entry.state && prevNext.Equal(entry.nextRetryAt) && prevPriority == entry.priority {
		return
	}
	m.rebuildIndexesLocked()
}

func (m *modelScheduler) removeEntryLocked(credID string) {
	if m == nil || credID == "" {
		return
	}
	if _, ok := m.entries[credID]; !ok {
		return
	}
	delete(m.entries, credID)
	m.rebuildIndexesLocked()
}

func (m *modelScheduler) promoteExpiredLocked(now time.Time) {
	if m == nil || len(m.blocked) == 0 {
		return
	}
	changed := false
	for _, entry := range m.blocked {
		if entry == nil || entry.cred == nil || entry.nextRetryAt.IsZero() || entry.nextRetryAt.After(now) {
			continue
		}
		blocked, reason, next := isCredentialBlockedForModel(entry.cred, m.modelKey, now)
		switch {
		case !blocked:
			entry.state = scheduledStateReady
			entry.nextRetryAt = time.Time{}
		case reason == blockReasonCooldown:
			entry.state = scheduledStateCooldown
			entry.nextRetryAt = next
		case reason == blockReasonDisabled:
			entry.state = scheduledStateDisabled
			entry.nextRetryAt = time.Time{}
		default:
			entry.state = scheduledStateBlocked
			entry.nextRetryAt = next
		}
		changed = true
	}
	if changed {
		m.rebuildIndexesLocked()
	}
}

func (m *modelScheduler) pickReadyLocked(selector CredentialSelector, predicate func(*ManagedCredential) bool) *ManagedCredential {
	if m == nil {
		return nil
	}
	m.promoteExpiredLocked(time.Now())

	// Find the highest priority bucket with a matching ready credential.
	bestPriority := 0
	found := false
	for _, p := range m.priorityOrder {
		bucket := m.readyByPriority[p]
		if bucket == nil {
			continue
		}
		if hasMatch(bucket, predicate) {
			if !found || p > bestPriority {
				bestPriority = p
				found = true
			}
		}
	}
	if !found {
		return nil
	}
	return selector.PickFromBucket(m.readyByPriority[bestPriority], predicate)
}

func hasMatch(bucket *ReadyBucket, predicate func(*ManagedCredential) bool) bool {
	for _, cred := range bucket.creds {
		if predicate == nil || predicate(cred) {
			return true
		}
	}
	return false
}

func (m *modelScheduler) unavailableErrorLocked(scopeKey, model string, predicate func(*ManagedCredential) bool) error {
	now := time.Now()
	total := 0
	cooldownCount := 0
	earliest := time.Time{}
	for _, entry := range m.entries {
		if predicate != nil && !predicate(entry.cred) {
			continue
		}
		total++
		if entry.state != scheduledStateCooldown {
			continue
		}
		cooldownCount++
		if !entry.nextRetryAt.IsZero() && (earliest.IsZero() || entry.nextRetryAt.Before(earliest)) {
			earliest = entry.nextRetryAt
		}
	}
	if total == 0 {
		return &Error{Code: "credential_not_found", Message: "no credential available"}
	}
	if cooldownCount == total && !earliest.IsZero() {
		resetIn := earliest.Sub(now)
		if resetIn < 0 {
			resetIn = 0
		}
		return &cooldownError{model: model, scopeKey: scopeKey, resetIn: formatDuration(resetIn)}
	}
	return &Error{Code: "credential_unavailable", Message: "no credential available"}
}

func formatDuration(d time.Duration) string {
	secs := int(math.Ceil(d.Seconds()))
	if secs <= 0 {
		return "0s"
	}
	return strconv.Itoa(secs) + "s"
}

func matchFilter(cred *ManagedCredential, filter Filter) bool {
	if cred == nil {
		return false
	}
	if credentialType := strings.ToLower(strings.TrimSpace(filter.Type)); credentialType != "" && strings.ToLower(cred.Type) != credentialType {
		return false
	}
	return true
}

func computeBackoff(level int, base, maxBackoff time.Duration) time.Duration {
	if level <= 0 {
		return base
	}
	d := base
	for i := 0; i < level && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

func (m *modelScheduler) rebuildIndexesLocked() {
	m.readyByPriority = make(map[int]*ReadyBucket)
	m.priorityOrder = m.priorityOrder[:0]
	m.blocked = m.blocked[:0]

	byPriority := make(map[int][]*scheduledCred)
	for _, entry := range m.entries {
		if entry == nil || entry.cred == nil {
			continue
		}
		switch entry.state {
		case scheduledStateReady:
			p := entry.priority
			byPriority[p] = append(byPriority[p], entry)
		case scheduledStateCooldown, scheduledStateBlocked:
			m.blocked = append(m.blocked, entry)
		}
	}

	for priority, entries := range byPriority {
		sort.Slice(entries, func(i, j int) bool { return entries[i].cred.ID < entries[j].cred.ID })
		creds := make([]*ManagedCredential, len(entries))
		for i, e := range entries {
			creds[i] = e.cred
		}
		m.readyByPriority[priority] = &ReadyBucket{creds: creds}
		m.priorityOrder = append(m.priorityOrder, priority)
	}
	sort.Slice(m.priorityOrder, func(i, j int) bool { return m.priorityOrder[i] > m.priorityOrder[j] })
	sort.Slice(m.blocked, func(i, j int) bool {
		l, r := m.blocked[i], m.blocked[j]
		if l == nil || r == nil {
			return l != nil
		}
		if l.nextRetryAt.Equal(r.nextRetryAt) {
			return l.cred.ID < r.cred.ID
		}
		if l.nextRetryAt.IsZero() {
			return false
		}
		if r.nextRetryAt.IsZero() {
			return true
		}
		return l.nextRetryAt.Before(r.nextRetryAt)
	})
}
