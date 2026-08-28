package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

const runIDPrefix = "run-"

// Identities contains the runtime-neutral correlation data carried through a
// logical Agent run. Trace fields describe one transport/execution segment;
// RunID remains stable when a later segment gets a new trace.
type Identities struct {
	AgentID      string
	RuntimeType  string
	RunID        string
	SessionID    string
	RequestID    string
	TraceID      string
	SpanID       string
	ParentSpanID string
	SegmentIndex uint32
}

type identitiesContextKey struct{}

// WithIdentities stores a copy of ids in ctx. Empty fields intentionally
// remain empty; callers can use MergeIdentities to add run data without
// discarding trace data already attached by the transport.
func WithIdentities(ctx context.Context, ids Identities) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, identitiesContextKey{}, ids)
}

func IdentitiesFromContext(ctx context.Context) (Identities, bool) {
	if ctx == nil {
		return Identities{}, false
	}
	ids, ok := ctx.Value(identitiesContextKey{}).(Identities)
	return ids, ok
}

// MergeIdentities overlays non-empty correlation fields on any identities
// already in ctx. SegmentIndex is always taken from overlay.
func MergeIdentities(ctx context.Context, overlay Identities) context.Context {
	base, _ := IdentitiesFromContext(ctx)
	if overlay.AgentID != "" {
		base.AgentID = overlay.AgentID
	}
	if overlay.RuntimeType != "" {
		base.RuntimeType = overlay.RuntimeType
	}
	if overlay.RunID != "" {
		base.RunID = overlay.RunID
	}
	if overlay.SessionID != "" {
		base.SessionID = overlay.SessionID
	}
	if overlay.RequestID != "" {
		base.RequestID = overlay.RequestID
	}
	if overlay.TraceID != "" {
		base.TraceID = overlay.TraceID
	}
	if overlay.SpanID != "" {
		base.SpanID = overlay.SpanID
	}
	if overlay.ParentSpanID != "" {
		base.ParentSpanID = overlay.ParentSpanID
	}
	base.SegmentIndex = overlay.SegmentIndex
	return WithIdentities(ctx, base)
}

// NewRunID returns an opaque, process-independent logical execution id.
func NewRunID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return runIDPrefix + hex.EncodeToString(raw), nil
}

func ValidRunID(id string) bool {
	if !strings.HasPrefix(id, runIDPrefix) || len(id) != len(runIDPrefix)+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, runIDPrefix))
	return err == nil
}
