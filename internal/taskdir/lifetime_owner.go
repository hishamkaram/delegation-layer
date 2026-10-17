package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type (
	LifetimeOwnerLease struct {
		mu                      sync.Mutex
		maintenance             *LockFile
		owner                   *LockFile
		ownerClosed             bool
		maintenanceUnlockFailed bool
		maintenanceUnlocked     bool
		released                bool
		releaseErr              error
	}
)

// AcquireLifetimeOwner serializes the only process-scope transition owner for
// a task. It also retains the root maintenance lease so cleanup cannot remove
// the durable scope while the guardian is active.
func (td *TaskDir) AcquireLifetimeOwner(ctx context.Context) (_ *LifetimeOwnerLease, resultErr error) {
	if td == nil || ctx == nil {
		return nil, task.ErrInvalidPermit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := td.store.maintLock.LockSH(ctx); err != nil {
		return nil, err
	}
	releaseMaintenance := true
	defer func() {
		if releaseMaintenance {
			resultErr = errors.Join(resultErr, td.store.maintLock.Unlock())
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(td.Dir, lifetimeDirectory)
	if err := td.store.mkdirContext(ctx, dir, td.store.faultInjector); err != nil {
		return nil, err
	}
	owner, err := td.store.openLock(filepath.Join(dir, lifetimeOwnerLockName), LockLevelRun)
	if err != nil {
		return nil, err
	}
	if err = owner.LockEX(ctx); err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	releaseMaintenance = false
	return &LifetimeOwnerLease{maintenance: td.store.maintLock, owner: owner}, nil
}

// TryAcquireLifetimeOwner is the nonblocking recovery probe. A busy result
// means the current guardian still owns the transition.
func (td *TaskDir) TryAcquireLifetimeOwner() (_ *LifetimeOwnerLease, resultErr error) {
	return td.tryAcquireLifetimeOwner(context.Background())
}

// TryAcquireLifetimeOwnerContext is the context-aware nonblocking recovery
// probe. Cancellation is checked before any lock or filesystem state changes.
func (td *TaskDir) TryAcquireLifetimeOwnerContext(ctx context.Context) (_ *LifetimeOwnerLease, resultErr error) {
	if ctx == nil {
		return nil, task.ErrInvalidPermit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return td.tryAcquireLifetimeOwner(ctx)
}

func (td *TaskDir) tryAcquireLifetimeOwner(ctx context.Context) (_ *LifetimeOwnerLease, resultErr error) {
	if td == nil || ctx == nil {
		return nil, task.ErrInvalidPermit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := td.store.maintLock.LockSHNonblockingContext(ctx); err != nil {
		return nil, err
	}
	releaseMaintenance := true
	defer func() {
		if releaseMaintenance {
			resultErr = errors.Join(resultErr, td.store.maintLock.Unlock())
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(td.Dir, lifetimeDirectory)
	if err := td.store.mkdirContext(ctx, dir, td.store.faultInjector); err != nil {
		return nil, err
	}
	owner, err := td.store.openLock(filepath.Join(dir, lifetimeOwnerLockName), LockLevelRun)
	if err != nil {
		return nil, err
	}
	if err = owner.LockEXNonblockingContext(ctx); err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.Join(err, owner.Close())
	}
	releaseMaintenance = false
	return &LifetimeOwnerLease{maintenance: td.store.maintLock, owner: owner}, nil
}

func (l *LifetimeOwnerLease) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return l.releaseErr
	}
	var ownerErr error
	if !l.ownerClosed {
		ownerErr = l.owner.Close()
		if ownerErr == nil {
			l.ownerClosed = true
		}
	}

	var maintenanceErr error
	if !l.maintenanceUnlocked {
		if l.maintenanceUnlockFailed {
			// LockFile.Unlock decrements its local shared count before the
			// underlying flock operation. Restore that reference before retrying
			// an unlock that failed, so the kernel lease remains authoritative.
			maintenanceErr = l.maintenance.LockSHNonblocking()
		}
		if maintenanceErr == nil {
			maintenanceErr = l.maintenance.Unlock()
		}
		if maintenanceErr == nil {
			l.maintenanceUnlocked = true
			l.maintenanceUnlockFailed = false
		} else {
			l.maintenanceUnlockFailed = true
		}
	}

	l.releaseErr = errors.Join(ownerErr, maintenanceErr)
	l.released = l.ownerClosed && l.maintenanceUnlocked && l.releaseErr == nil
	return l.releaseErr
}

// DuplicateControlLeaseFiles gives the one-shot supervisor control owner
// inherited copies of the maintenance and lifetime-owner locks. The caller
// retains normal release ownership and waits for the control owner to finish;
// the owner-held copies protect the same transition if the caller exits first.
func (l *LifetimeOwnerLease) DuplicateControlLeaseFiles() ([]*os.File, error) {
	if l == nil {
		return nil, task.ErrInvalidPermit
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.ownerClosed || l.maintenanceUnlocked {
		return nil, task.ErrPermitAlreadyUsed
	}
	maintenance, err := l.maintenance.DuplicateHeldFile("delegate-task-maintenance-lease")
	if err != nil {
		return nil, err
	}
	owner, err := l.owner.DuplicateHeldFile("delegate-task-lifetime-owner")
	if err != nil {
		return nil, errors.Join(err, maintenance.Close())
	}
	return []*os.File{maintenance, owner}, nil
}
