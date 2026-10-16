package taskdir

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const controlLifetimeOwnerLockName = ".lifetime-owner.lock"

// AcquireLifetimeOwner serializes the only process-scope transition owner for
// a control directory. The maintenance lease remains held by the returned
// LifetimeOwnerLease until Release, so cleanup cannot remove the journal while
// its guardian is active. Close waits for an acquisition already in progress;
// the returned lease remains independently releasable after Close.
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
	releaseMaintenance := true
	defer func() {
		if releaseMaintenance {
			resultErr = errors.Join(resultErr, d.store.maintLock.Unlock())
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	owner, err := d.store.openLock(filepath.Join(d.dir, controlLifetimeOwnerLockName), LockLevelRun)
	if err != nil {
		return nil, err
	}
	lockOwner := owner.LockEX
	if nonblocking {
		lockOwner = owner.LockEXNonblockingContext
	}
	if err = lockOwner(ctx); err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.Join(err, owner.Close())
	}

	releaseMaintenance = false
	return &LifetimeOwnerLease{maintenance: d.store.maintLock, owner: owner}, nil
}
