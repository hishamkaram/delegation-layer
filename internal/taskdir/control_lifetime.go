package taskdir

import (
	"context"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const controlLifetimeOwnerLockName = ".lifetime-owner.lock"

// AcquireLifetimeOwner serializes the only process-scope transition owner for
// a control directory. The maintenance lease remains held by the returned
// LifetimeOwnerLease until Release, so cleanup cannot remove the journal while
// its guardian is active. Close waits for an acquisition already in progress;
// the returned lease remains independently releasable after Close. Any nonnil
// lease returned with an error must be retained and released before Store.Close.
func (d *ControlDir) AcquireLifetimeOwner(ctx context.Context) (_ *LifetimeOwnerLease, resultErr error) {
	return d.acquireLifetimeOwner(ctx, false)
}

// TryAcquireLifetimeOwnerContext is the observation/recovery edge. A live
// owner is left untouched; no deadline is spent waiting for that owner.
func (d *ControlDir) TryAcquireLifetimeOwnerContext(ctx context.Context) (*LifetimeOwnerLease, error) {
	return d.acquireLifetimeOwner(ctx, true)
}

func (d *ControlDir) acquireLifetimeOwner(ctx context.Context, nonblocking bool) (_ *LifetimeOwnerLease, resultErr error) {
	if d == nil || d.store == nil || ctx == nil {
		return nil, task.ErrInvalidPermit
	}
	if err := d.begin(); err != nil {
		return nil, err
	}
	defer d.end()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	lockMaintenance := d.store.maintLock.LockSH
	if nonblocking {
		lockMaintenance = d.store.maintLock.LockSHNonblockingContext
	}
	if err := lockMaintenance(ctx); err != nil {
		return nil, err
	}
	lease := newPartialLifetimeOwner(d.store)
	if err := ctx.Err(); err != nil {
		return finishLifetimeOwnerAcquisition(lease, err)
	}

	owner, err := d.store.openLock(filepath.Join(d.dir, controlLifetimeOwnerLockName), LockLevelRun)
	if err != nil {
		return finishLifetimeOwnerAcquisition(lease, err)
	}
	lease.owner, lease.ownerClosed = owner, false
	lockOwner := owner.LockEX
	if nonblocking {
		lockOwner = owner.LockEXNonblockingContext
	}
	if err = lockOwner(ctx); err != nil {
		return finishLifetimeOwnerAcquisition(lease, err)
	}
	if err = ctx.Err(); err != nil {
		return finishLifetimeOwnerAcquisition(lease, err)
	}

	return lease, nil
}

// TryAcquireExistingLifetimeOwnerContext observes only an existing owner inode.
// It never creates directories or locks. With nil error, present=false means
// whole absence, and present=true returns the acquired lease. On error, false
// is not absence; true means the existing inode was opened (including busy).
// Any nonnil error handle remains cleanup-owned and requires Release. ControlDir
// and its borrowed Store must outlive that release. Free/absent is not closure
// evidence or native Start/Stop permission; callers own that policy.
func (d *ControlDir) TryAcquireExistingLifetimeOwnerContext(ctx context.Context) (*LifetimeOwnerLease, bool, error) {
	if d == nil || d.store == nil || ctx == nil {
		return nil, false, task.ErrInvalidPermit
	}
	if err := d.begin(); err != nil {
		return nil, false, err
	}
	defer d.end()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := d.store.maintLock.LockSHNonblockingContext(ctx); err != nil {
		return nil, false, err
	}
	lease := newPartialLifetimeOwner(d.store)
	if err := ctx.Err(); err != nil {
		return finishExistingControlOwner(lease, false, err)
	}
	owner, err := d.store.openExistingLock(filepath.Join(d.dir, controlLifetimeOwnerLockName), LockLevelRun)
	if err != nil {
		return finishExistingControlOwner(lease, false, err)
	}
	lease.owner, lease.ownerClosed = owner, false
	if err = owner.LockEXNonblockingContext(ctx); err != nil {
		return finishExistingControlOwner(lease, true, err)
	}
	if err = ctx.Err(); err != nil {
		return finishExistingControlOwner(lease, true, err)
	}
	return lease, true, nil
}

func finishExistingControlOwner(lease *LifetimeOwnerLease, present bool, cause error) (*LifetimeOwnerLease, bool, error) {
	remaining, joined := finishLifetimeOwnerAcquisition(lease, cause)
	if remaining != nil {
		return remaining, present, joined
	}
	if task.RecordAbsent(joined) {
		return nil, false, nil
	}
	return nil, present, joined
}
