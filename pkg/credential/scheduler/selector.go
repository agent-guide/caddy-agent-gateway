package scheduler

import (
	"fmt"
	"time"
)

// CredentialSelector picks a credential from a pre-filtered, priority-sorted ReadyBucket.
// Implement this interface to provide a custom selection algorithm.
type CredentialSelector interface {
	PickFromBucket(bucket *ReadyBucket, predicate func(*ManagedCredential) bool) *ManagedCredential
}

// ReadyBucket holds credentials at one priority level that are ready for selection.
type ReadyBucket struct {
	creds  []*ManagedCredential
	cursor int
}

// RoundRobinSelector distributes requests evenly across available credentials.
type RoundRobinSelector struct{}

// FillFirstSelector exhausts one credential before moving to the next, useful
// for staggering rolling-window subscription caps.
type FillFirstSelector struct{}

// PickFromBucket picks the next credential using round-robin within the bucket.
func (s *RoundRobinSelector) PickFromBucket(bucket *ReadyBucket, predicate func(*ManagedCredential) bool) *ManagedCredential {
	n := len(bucket.creds)
	if n == 0 {
		return nil
	}
	start := bucket.cursor % n
	for offset := 0; offset < n; offset++ {
		index := (start + offset) % n
		cred := bucket.creds[index]
		if predicate != nil && !predicate(cred) {
			continue
		}
		bucket.cursor = index + 1
		return cred
	}
	return nil
}

// PickFromBucket picks the first matching credential in the bucket.
func (s *FillFirstSelector) PickFromBucket(bucket *ReadyBucket, predicate func(*ManagedCredential) bool) *ManagedCredential {
	for _, cred := range bucket.creds {
		if predicate == nil || predicate(cred) {
			return cred
		}
	}
	return nil
}

type blockReason int

const (
	blockReasonNone     blockReason = iota
	blockReasonCooldown             // quota exhausted with known reset time
	blockReasonDisabled             // intentionally disabled
	blockReasonOther                // temporarily unavailable but not cooldown
)

// isCredentialBlockedForModel reports whether a credential is blocked for the given model.
// Returns (blocked, reason, nextRetry).
func isCredentialBlockedForModel(cred *ManagedCredential, model string, now time.Time) (bool, blockReason, time.Time) {
	if cred == nil {
		return true, blockReasonOther, time.Time{}
	}
	if cred.Disabled || cred.AuthInvalid {
		return true, blockReasonDisabled, time.Time{}
	}
	if cred.CredentialWideUnavailable && cred.CredentialWideNextRetryAfter.After(now) {
		return true, blockReasonOther, cred.CredentialWideNextRetryAfter
	}

	// Check per-model state first.
	if model != "" && len(cred.ModelStates) > 0 {
		state, ok := cred.ModelStates[model]
		if ok && state != nil {
			if state.AuthInvalid {
				return true, blockReasonDisabled, time.Time{}
			}
			if state.Unavailable && !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now) {
				next := state.NextRetryAfter
				if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(next) {
					next = state.Quota.NextRecoverAt
				}
				if state.Quota.Exceeded {
					return true, blockReasonCooldown, next
				}
				return true, blockReasonOther, next
			}
			return false, blockReasonNone, time.Time{}
		}
		// No model state entry; fall through to credential-level check.
		return false, blockReasonNone, time.Time{}
	}

	// Credential-level availability check.
	if cred.Unavailable && cred.NextRetryAfter.After(now) {
		next := cred.NextRetryAfter
		if !cred.Quota.NextRecoverAt.IsZero() && cred.Quota.NextRecoverAt.After(next) {
			next = cred.Quota.NextRecoverAt
		}
		if cred.Quota.Exceeded {
			return true, blockReasonCooldown, next
		}
		return true, blockReasonOther, next
	}
	return false, blockReasonNone, time.Time{}
}

// cooldownError is returned when all credentials for a model are in cooldown.
type cooldownError struct {
	model    string
	scopeKey string
	resetIn  string // formatted duration
}

func (e *cooldownError) Error() string {
	if e == nil {
		return ""
	}
	msg := fmt.Sprintf("all credentials for model %s are cooling down", e.model)
	if e.scopeKey != "" {
		msg += fmt.Sprintf(" in scope %s", e.scopeKey)
	}
	if e.resetIn != "" {
		msg += fmt.Sprintf(", retry after %s", e.resetIn)
	}
	return msg
}

func (e *cooldownError) StatusCode() int {
	return 429
}
