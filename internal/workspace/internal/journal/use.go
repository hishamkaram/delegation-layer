package journal

import (
	"context"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

// AcquireUseOwner excludes another admission or coordinator transition while
// this caller performs external work. It does not replace the durable usage
// reservation, which survives caller death through task closure and capture.
// Helpers use their own operation guardian lease and never acquire this lease.
// A nonnil lease returned on error is cleanup-only and must remain reachable
// with its original Store until explicit Release proves closure.
func (r *Resource) AcquireUseOwner(ctx context.Context) (*taskdir.LifetimeOwnerLease, error) {
	return r.control.AcquireLifetimeOwner(ctx)
}

func (r *Resource) TryAcquireUseOwner(ctx context.Context) (*taskdir.LifetimeOwnerLease, error) {
	return r.control.TryAcquireLifetimeOwnerContext(ctx)
}

// TryAcquireExistingUseOwner probes only an existing use-owner inode. This
// custody evidence never grants permission from a free or absent lease. Any
// nonnil result, including on error, must be released before closing Resource
// or its borrowed Store; the canonical control owner retains failure details.
func (r *Resource) TryAcquireExistingUseOwner(ctx context.Context) (*taskdir.LifetimeOwnerLease, bool, error) {
	if r == nil {
		return nil, false, os.ErrInvalid
	}
	return r.control.TryAcquireExistingLifetimeOwnerContext(ctx)
}

// StateRoot returns the root whose pinned store owns this resource journal.
func (r *Resource) StateRoot() string { return r.control.StateRoot() }

// LatestReservation returns the latest durable usage identity even after its
// separate closure record exists. No mutable last-use pointer is introduced.
func (r *Resource) LatestReservation(ctx context.Context) (reservation contract.Reservation, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		sequence, present, err := latestReservationSequence(r.control, ctx)
		if err != nil {
			return err
		}
		if !present {
			return os.ErrNotExist
		}
		reservation, err = readReservationTx(tx, r.request.WorkspaceID, sequence)
		return err
	})
	return reservation, resultErr
}
