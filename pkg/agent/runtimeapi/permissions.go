package runtimeapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

type PermissionAction struct {
	ActionID string `json:"action_id"`
	Name     string `json:"name,omitempty"`
}

type PermissionOption struct {
	OptionID string `json:"option_id"`
	Kind     string `json:"kind,omitempty"`
	Name     string `json:"name,omitempty"`
}

// PendingPermission is the runtime-neutral, claimable operator record. The
// opaque token is deliberately absent; native continuation identity remains in
// the selected backend's private store.
type PendingPermission struct {
	RequestID   string               `json:"request_id"`
	AgentID     string               `json:"agent_id"`
	RuntimeType string               `json:"runtime_type"`
	RunID       string               `json:"run_id"`
	SessionID   string               `json:"session_id,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	ExpiresAt   time.Time            `json:"expires_at"`
	Actions     []PermissionAction   `json:"actions,omitempty"`
	Options     []PermissionOption   `json:"options,omitempty"`
	ResumeMode  PermissionResumeMode `json:"resume_mode"`
	// TTL is an internal registration hint. When ExpiresAt is omitted, the
	// broker derives it from its injectable clock and this duration.
	TTL time.Duration `json:"-"`
}

// ContinuationResolver resolves an opaque token through a backend-owned store.
// The common broker never retains native waiter/checkpoint state or a
// per-request callback that closes over such state.
type ContinuationResolver interface {
	// ValidateContinuationDecision must be pure. The broker invokes it while
	// holding the claim lock so invalid input cannot consume a one-shot claim.
	ValidateContinuationDecision(string, PendingPermission, PermissionDecision) error
	ResolveContinuation(context.Context, string, PermissionDecision, time.Time) error
	ExpireContinuation(context.Context, string) error
}

type permissionEntry struct {
	info  PendingPermission
	token string
}

// PermissionBroker is the sole owner of pending/claimable permission state.
// Claim removes the entry before invoking backend code, so errors are terminal.
type PermissionBroker struct {
	mu        sync.Mutex
	pending   map[string]*permissionEntry
	claimed   map[string]claimedPermission
	expired   map[string]expiredPermission
	resolvers map[string]ContinuationResolver
	audits    []PermissionAudit
	now       func() time.Time
	wake      chan struct{}
	done      chan struct{}
	closed    bool
	closeOnce sync.Once
	wg        sync.WaitGroup
}

type claimedPermission struct {
	agentID, runtimeType string
	runID, sessionID     string
	at                   time.Time
}

type expiredPermission struct {
	agentID string
	at      time.Time
}

func claimedPermissionFrom(info PendingPermission, at time.Time) claimedPermission {
	return claimedPermission{
		agentID: info.AgentID, runtimeType: info.RuntimeType,
		runID: info.RunID, sessionID: info.SessionID, at: at,
	}
}

type permissionSourceKey struct{}

func WithPermissionSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, permissionSourceKey{}, strings.TrimSpace(source))
}
func permissionSource(ctx context.Context) string {
	source, _ := ctx.Value(permissionSourceKey{}).(string)
	if source == "" {
		return "unknown"
	}
	return source
}

type PermissionAudit struct {
	RequestID   string    `json:"request_id"`
	AgentID     string    `json:"agent_id"`
	RuntimeType string    `json:"runtime_type,omitempty"`
	RunID       string    `json:"run_id,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	Source      string    `json:"source"`
	Result      string    `json:"result"`
	At          time.Time `json:"at"`
}

// PermissionCorrelation is the durable, non-secret identity retained while a
// permission is pending and for the bounded claimed tombstone lifetime.
type PermissionCorrelation struct {
	RequestID   string
	AgentID     string
	RuntimeType string
	RunID       string
	SessionID   string
}

func NewPermissionBroker() *PermissionBroker {
	b := &PermissionBroker{
		pending: map[string]*permissionEntry{}, claimed: map[string]claimedPermission{}, expired: map[string]expiredPermission{},
		resolvers: map[string]ContinuationResolver{}, now: time.Now, wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
	b.wg.Add(1)
	go b.expiryLoop()
	return b
}

// NewContinuationToken returns an unguessable process-local backend lookup key.
func NewContinuationToken() (string, error) { return opaquePermissionID("cont-") }

// Register publishes a backend continuation that was already stored under
// token. resolver is retained once per runtime type, never in the claimable
// record. A failed publication leaves removal of the pre-stored backend token
// to the caller.
func (b *PermissionBroker) Register(info PendingPermission, token string, resolver ContinuationResolver) (string, error) {
	if b == nil || resolver == nil || strings.TrimSpace(token) == "" || strings.TrimSpace(info.AgentID) == "" || strings.TrimSpace(info.RuntimeType) == "" || !ValidRunID(strings.TrimSpace(info.RunID)) {
		return "", NewError(ErrorInvalidRequest, "invalid permission registration")
	}
	now := b.now().UTC()
	if info.CreatedAt.IsZero() {
		info.CreatedAt = now
	}
	if info.ExpiresAt.IsZero() {
		ttl := info.TTL
		if ttl <= 0 {
			ttl = 10 * time.Minute
		}
		info.ExpiresAt = now.Add(ttl)
	}
	if !info.ExpiresAt.After(now) {
		return "", NewError(ErrorPermissionExpired, "permission request is expired")
	}
	requestID := strings.TrimSpace(info.RequestID)
	if requestID == "" {
		var err error
		requestID, err = opaquePermissionID("perm-")
		if err != nil {
			return "", WrapError(ErrorTurnFailed, "generate permission request id", err)
		}
	}
	info.RequestID = requestID
	info.AgentID, info.RuntimeType, info.RunID, info.SessionID = strings.TrimSpace(info.AgentID), strings.TrimSpace(info.RuntimeType), strings.TrimSpace(info.RunID), strings.TrimSpace(info.SessionID)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return "", NewError(ErrorBackendUnavailable, "permission broker is closed")
	}
	b.sweepExpiredLocked(now)
	if b.pending[requestID] != nil {
		b.mu.Unlock()
		return "", NewError(ErrorInvalidRequest, "permission request is already registered")
	}
	b.resolvers[info.RuntimeType] = resolver
	b.pending[requestID] = &permissionEntry{info: info, token: strings.TrimSpace(token)}
	delete(b.claimed, requestID)
	delete(b.expired, requestID)
	b.mu.Unlock()
	b.signalExpiryLoop()
	return requestID, nil
}

func (b *PermissionBroker) List(agentID string) []PendingPermission {
	if b == nil {
		return []PendingPermission{}
	}
	now := b.now().UTC()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []PendingPermission{}
	for _, e := range b.pending {
		if e.info.AgentID == strings.TrimSpace(agentID) && now.Before(e.info.ExpiresAt) {
			out = append(out, clonePendingPermission(e.info))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestID < out[j].RequestID })
	return out
}

// LookupPermission returns common correlation for a pending or recently
// claimed opaque request id. Retaining only non-secret identity lets audit
// paths keep run/session attribution after an atomic winner removes the
// continuation from the pending set.
func (b *PermissionBroker) LookupPermission(requestID string) (PermissionCorrelation, bool) {
	if b == nil || strings.TrimSpace(requestID) == "" {
		return PermissionCorrelation{}, false
	}
	now := b.now().UTC()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweepExpiredLocked(now)
	requestID = strings.TrimSpace(requestID)
	if e := b.pending[requestID]; e != nil {
		return PermissionCorrelation{
			RequestID: requestID, AgentID: e.info.AgentID, RuntimeType: e.info.RuntimeType,
			RunID: e.info.RunID, SessionID: e.info.SessionID,
		}, true
	}
	if e, exists := b.claimed[requestID]; exists {
		return PermissionCorrelation{
			RequestID: requestID, AgentID: e.agentID, RuntimeType: e.runtimeType,
			RunID: e.runID, SessionID: e.sessionID,
		}, true
	}
	return PermissionCorrelation{}, false
}

func (b *PermissionBroker) Resolve(ctx context.Context, agentID string, decision PermissionDecision) error {
	e, err := b.claim(strings.TrimSpace(agentID), strings.TrimSpace(decision.RequestID), false, &decision)
	if err != nil {
		b.audit(agentID, decision.RequestID, permissionSource(ctx), string(PublicError(err).ErrorType))
		return err
	}
	resolver := b.resolver(e.info.RuntimeType)
	if resolver == nil {
		err = NewError(ErrorPermissionNotFound, "permission continuation not found")
	} else {
		err = resolver.ResolveContinuation(ctx, e.token, decision, e.info.ExpiresAt)
	}
	result := "resolved"
	if err != nil {
		result = continuationFailureResult(err)
	}
	b.audit(agentID, decision.RequestID, permissionSource(ctx), result)
	return err
}

func (b *PermissionBroker) Expire(ctx context.Context, agentID, requestID string) error {
	e, err := b.claim(strings.TrimSpace(agentID), strings.TrimSpace(requestID), true, nil)
	if err != nil {
		return err
	}
	resolver := b.resolver(e.info.RuntimeType)
	if resolver == nil {
		err = NewError(ErrorPermissionNotFound, "permission continuation not found")
	} else {
		err = resolver.ExpireContinuation(ctx, e.token)
	}
	result := "expired"
	if err != nil {
		result = continuationFailureResult(err)
	}
	b.audit(agentID, requestID, permissionSource(ctx), result)
	return err
}

func (b *PermissionBroker) Audits(agentID string) []PermissionAudit {
	if b == nil {
		return []PermissionAudit{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []PermissionAudit{}
	for _, a := range b.audits {
		if a.AgentID == strings.TrimSpace(agentID) {
			out = append(out, a)
		}
	}
	return out
}

// RecordContinuationLost records that a previously claimed backend
// continuation could no longer be resumed. It does not recreate claim state.
func (b *PermissionBroker) RecordContinuationLost(ctx context.Context, agentID, requestID string) {
	b.audit(agentID, requestID, permissionSource(ctx), "continuation_lost")
}

func (b *PermissionBroker) audit(agentID, requestID, source, result string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	requestID = strings.TrimSpace(requestID)
	audit := PermissionAudit{RequestID: requestID, AgentID: strings.TrimSpace(agentID), Source: source, Result: result, At: b.now().UTC()}
	if entry := b.pending[requestID]; entry != nil {
		audit.RuntimeType, audit.RunID, audit.SessionID = entry.info.RuntimeType, entry.info.RunID, entry.info.SessionID
	} else if claimed, ok := b.claimed[requestID]; ok {
		if audit.AgentID == "" {
			audit.AgentID = claimed.agentID
		}
		audit.RuntimeType, audit.RunID, audit.SessionID = claimed.runtimeType, claimed.runID, claimed.sessionID
	}
	b.audits = append(b.audits, audit)
	if len(b.audits) > 4096 {
		b.audits = append([]PermissionAudit(nil), b.audits[len(b.audits)-4096:]...)
	}
	b.mu.Unlock()
}

// DrainRun claims every permission for a run and fails each continuation closed.
func (b *PermissionBroker) DrainRun(ctx context.Context, agentID, runID string) int {
	if b == nil {
		return 0
	}
	var claimed []*permissionEntry
	b.mu.Lock()
	for id, e := range b.pending {
		if e.info.AgentID == agentID && e.info.RunID == runID {
			claimed = append(claimed, e)
			delete(b.pending, id)
			b.claimed[id] = claimedPermissionFrom(e.info, b.now().UTC())
		}
	}
	b.mu.Unlock()
	for _, e := range claimed {
		err := b.expireContinuation(ctx, e)
		result := "cancelled"
		if err != nil {
			result = continuationFailureResult(err)
		}
		b.audit(e.info.AgentID, e.info.RequestID, permissionSource(ctx), result)
	}
	return len(claimed)
}

func (b *PermissionBroker) DrainAgent(ctx context.Context, agentID string) int {
	return b.ClaimAgent(agentID)(ctx)
}

// ClaimAgent atomically removes an Agent's pending permissions from the
// claimable set and returns their external continuation cleanup. Claiming is
// bounded and in-memory, so definition commits can make permission retirement
// visible before publishing a new generation without holding the snapshot lock
// across backend I/O.
func (b *PermissionBroker) ClaimAgent(agentID string) func(context.Context) int {
	if b == nil {
		return func(context.Context) int { return 0 }
	}
	var claimed []*permissionEntry
	b.mu.Lock()
	for id, e := range b.pending {
		if e.info.AgentID == agentID {
			claimed = append(claimed, e)
			delete(b.pending, id)
			b.claimed[id] = claimedPermissionFrom(e.info, b.now().UTC())
		}
	}
	b.mu.Unlock()
	return func(ctx context.Context) int {
		for _, e := range claimed {
			err := b.expireContinuation(ctx, e)
			result := "cancelled"
			if err != nil {
				result = continuationFailureResult(err)
			}
			b.audit(e.info.AgentID, e.info.RequestID, permissionSource(ctx), result)
		}
		return len(claimed)
	}
}

// DrainAll claims every pending permission and invokes its backend cleanup.
func (b *PermissionBroker) DrainAll(ctx context.Context) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	claimed := make([]*permissionEntry, 0, len(b.pending))
	now := b.now().UTC()
	for id, e := range b.pending {
		claimed = append(claimed, e)
		delete(b.pending, id)
		b.claimed[id] = claimedPermissionFrom(e.info, now)
	}
	b.mu.Unlock()
	for _, e := range claimed {
		err := b.expireContinuation(ctx, e)
		result := "cancelled"
		if err != nil {
			result = continuationFailureResult(err)
		}
		b.audit(e.info.AgentID, e.info.RequestID, permissionSource(ctx), result)
	}
	return len(claimed)
}

// Close rejects late publications, stops expiry scheduling, and drains all
// records through the same fail-closed continuation path. It is idempotent.
func (b *PermissionBroker) Close(ctx context.Context) {
	if b == nil {
		return
	}
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		close(b.done)
		b.wg.Wait()
		b.DrainAll(ctx)
	})
}

func (b *PermissionBroker) claim(agentID, requestID string, expiry bool, decision *PermissionDecision) (*permissionEntry, error) {
	if b == nil || requestID == "" {
		return nil, NewError(ErrorPermissionNotFound, "permission request not found")
	}
	now := b.now().UTC()
	b.mu.Lock()
	b.sweepExpiredLocked(now)
	e := b.pending[requestID]
	if e != nil && e.info.AgentID != agentID {
		e = nil
	}
	expiredNow := e != nil && !now.Before(e.info.ExpiresAt)
	if e != nil && !expiredNow && decision != nil {
		resolver := b.resolvers[e.info.RuntimeType]
		if resolver == nil {
			b.mu.Unlock()
			return nil, NewError(ErrorPermissionNotFound, "permission continuation not found")
		}
		if err := resolver.ValidateContinuationDecision(e.token, clonePendingPermission(e.info), *decision); err != nil {
			b.mu.Unlock()
			return nil, err
		}
	}
	if e != nil {
		delete(b.pending, requestID)
		b.claimed[requestID] = claimedPermissionFrom(e.info, now)
		if expiry || expiredNow {
			b.expired[requestID] = expiredPermission{agentID: e.info.AgentID, at: now}
		}
	}
	expired := b.expired[requestID]
	b.mu.Unlock()
	if e == nil {
		if expired.agentID == agentID {
			return nil, NewError(ErrorPermissionExpired, "permission request expired")
		}
		return nil, NewError(ErrorPermissionNotFound, "permission request not found")
	}
	if expiredNow {
		err := b.expireContinuation(context.Background(), e)
		result := "expired"
		if err != nil {
			result = continuationFailureResult(err)
		}
		b.audit(e.info.AgentID, e.info.RequestID, "expiry", result)
		return nil, NewError(ErrorPermissionExpired, "permission request expired")
	}
	return e, nil
}

func (b *PermissionBroker) resolver(runtimeType string) ContinuationResolver {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.resolvers[runtimeType]
}

func (b *PermissionBroker) expireContinuation(ctx context.Context, e *permissionEntry) error {
	resolver := b.resolver(e.info.RuntimeType)
	if resolver == nil {
		return NewError(ErrorPermissionNotFound, "permission continuation not found")
	}
	return resolver.ExpireContinuation(ctx, e.token)
}

func continuationFailureResult(err error) string {
	if code, ok := ErrorCodeOf(err); ok && code == ErrorPermissionNotFound {
		return "continuation_lost"
	}
	return "continuation_failed"
}

func (b *PermissionBroker) signalExpiryLoop() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *PermissionBroker) expiryLoop() {
	defer b.wg.Done()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	for {
		wait, ok := b.nextExpiryDelay()
		if ok {
			timer.Reset(wait)
		}
		select {
		case <-b.done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-b.wake:
			if ok && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			b.expireDue()
		}
	}
}

func (b *PermissionBroker) nextExpiryDelay() (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || len(b.pending) == 0 {
		return 0, false
	}
	var next time.Time
	for _, e := range b.pending {
		if next.IsZero() || e.info.ExpiresAt.Before(next) {
			next = e.info.ExpiresAt
		}
	}
	d := next.Sub(b.now().UTC())
	if d < 0 {
		d = 0
	}
	return d, true
}

func (b *PermissionBroker) expireDue() {
	now := b.now().UTC()
	var due []*permissionEntry
	b.mu.Lock()
	for id, e := range b.pending {
		if !now.Before(e.info.ExpiresAt) {
			due = append(due, e)
			delete(b.pending, id)
			b.claimed[id] = claimedPermissionFrom(e.info, now)
			b.expired[id] = expiredPermission{agentID: e.info.AgentID, at: now}
		}
	}
	b.mu.Unlock()
	for _, e := range due {
		err := b.expireContinuation(context.Background(), e)
		result := "expired"
		if err != nil {
			result = continuationFailureResult(err)
		}
		b.audit(e.info.AgentID, e.info.RequestID, "expiry", result)
	}
}

func (b *PermissionBroker) sweepExpiredLocked(now time.Time) {
	for id, e := range b.claimed {
		if now.Sub(e.at) >= 10*time.Minute {
			delete(b.claimed, id)
		}
	}
	for id, e := range b.expired {
		if now.Sub(e.at) >= 10*time.Minute {
			delete(b.expired, id)
		}
	}
}

func opaquePermissionID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw), nil
}
func clonePendingPermission(in PendingPermission) PendingPermission {
	in.Actions = append([]PermissionAction(nil), in.Actions...)
	in.Options = append([]PermissionOption(nil), in.Options...)
	return in
}
