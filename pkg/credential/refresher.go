package credential

import (
	"context"
	"time"
)

const (
	MetadataRefreshExpiryDeltaKey = "refresh_expiry_delta"
	refreshExpiryLeeway           = 30 * time.Second
	// RefreshFailureCooldown prevents concurrent and immediately subsequent
	// requests from repeatedly invoking a failing external refresh command.
	RefreshFailureCooldown = 30 * time.Second
)

// Refresher delegates a named credential refresh implementation. The gateway
// owns expiry detection and persistence, while an external tool owns the
// provider-specific refresh behavior.
type Refresher interface {
	Refresh(ctx context.Context, cred *Credential) (*Credential, error)
}

func credentialNeedsRefresh(cred *Credential, now time.Time) bool {
	if cred == nil {
		return false
	}
	expiresAt, ok := cred.ExpirationTime()
	if !ok || expiresAt.IsZero() {
		return false
	}
	delta, ok := cred.RefreshExpiryDelta()
	leeway := refreshExpiryLeeway
	if ok && delta > 0 {
		leeway = delta
	}
	return !expiresAt.After(now.Add(leeway))
}
