package host

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	acptransport "github.com/agent-guide/agent-gateway/pkg/acp/transport"
)

// ErrPermissionNotFound reports a decision for a permission request that is
// unknown, already answered, or expired (the in-agent wait timed out and
// failed closed).
var ErrPermissionNotFound = fmt.Errorf("acp permission request not found")

// pendingPermission is one in-flight interactive session/request_permission
// waiting for a client decision.
type pendingPermission struct {
	info     PendingPermissionInfo
	decision chan acptransport.PermissionResponse
}

// permissionBroker tracks in-flight interactive permission requests so the
// north-side decision endpoint can resolve them by request id. Every entry is
// one-shot: resolving removes it, and the waiting handler removes it on
// timeout or turn teardown (fail closed).
type permissionBroker struct {
	mu      sync.Mutex
	pending map[string]*pendingPermission
}

func newPermissionBroker() *permissionBroker {
	return &permissionBroker{pending: map[string]*pendingPermission{}}
}

func (b *permissionBroker) create(ownerID, sessionID string, data json.RawMessage) (*pendingPermission, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate permission request id: %w", err)
	}
	entry := &pendingPermission{
		info: PendingPermissionInfo{
			RequestID: "perm-" + hex.EncodeToString(raw),
			OwnerID:   ownerID,
			SessionID: sessionID,
			CreatedAt: time.Now().UTC(),
			Data:      data,
		},
		decision: make(chan acptransport.PermissionResponse, 1),
	}
	b.mu.Lock()
	b.pending[entry.info.RequestID] = entry
	b.mu.Unlock()
	return entry, nil
}

func (b *permissionBroker) remove(requestID string) {
	b.mu.Lock()
	delete(b.pending, requestID)
	b.mu.Unlock()
}

// resolve delivers a decision to the waiting permission handler. The entry is
// removed before delivery, so each request resolves at most once.
func (b *permissionBroker) resolve(requestID string, resp acptransport.PermissionResponse) error {
	requestID = strings.TrimSpace(requestID)
	b.mu.Lock()
	entry := b.pending[requestID]
	delete(b.pending, requestID)
	b.mu.Unlock()
	if entry == nil {
		return ErrPermissionNotFound
	}
	entry.decision <- resp
	return nil
}

func (b *permissionBroker) list() []PendingPermissionInfo {
	b.mu.Lock()
	out := make([]PendingPermissionInfo, 0, len(b.pending))
	for _, entry := range b.pending {
		out = append(out, entry.info)
	}
	b.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].RequestID < out[b].RequestID })
	return out
}
