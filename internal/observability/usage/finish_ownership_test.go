package usage

import (
	"context"
	"testing"
)

func TestFinishOwnershipTransfersExactlyOnce(t *testing.T) {
	ctx, owner := ContextWithFinishOwnership(context.Background())
	if owner.Transferred() {
		t.Fatal("new ownership token is already transferred")
	}
	if !TransferFinishOwnership(ctx) {
		t.Fatal("first transfer failed")
	}
	if TransferFinishOwnership(ctx) {
		t.Fatal("second transfer succeeded")
	}
	if !owner.Transferred() {
		t.Fatal("transfer was not visible to dispatcher")
	}
}
