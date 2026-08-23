package usage

import (
	"context"
	"sync/atomic"
)

type finishOwnershipKey struct{}

// FinishOwnership coordinates the single owner allowed to finish an
// interaction span. The dispatcher owns it initially; a response lifecycle
// transfers ownership immediately before provider or local execution.
type FinishOwnership struct {
	transferred atomic.Bool
}

// ContextWithFinishOwnership installs a new dispatcher-owned finish token.
func ContextWithFinishOwnership(ctx context.Context) (context.Context, *FinishOwnership) {
	owner := &FinishOwnership{}
	return context.WithValue(ctx, finishOwnershipKey{}, owner), owner
}

// TransferFinishOwnership transfers terminal responsibility to the response
// lifecycle. It returns false when no dispatcher token exists or ownership was
// already transferred.
func TransferFinishOwnership(ctx context.Context) bool {
	owner, _ := ctx.Value(finishOwnershipKey{}).(*FinishOwnership)
	return owner != nil && owner.transferred.CompareAndSwap(false, true)
}

// Transferred reports whether the dispatcher must suppress its normal defer.
func (o *FinishOwnership) Transferred() bool {
	return o != nil && o.transferred.Load()
}
