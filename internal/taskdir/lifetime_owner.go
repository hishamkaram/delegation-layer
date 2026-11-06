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
		mu                  sync.Mutex
		store               *Store // Immutable original borrow; it must outlive unresolved release.
		maintenance         *LockFile
		owner               *LockFile
		ownerClosed         bool
		maintenanceUnlocked bool
		released            bool
		releaseErr          error
	}
)

// AcquireLifetimeOwner serializes the only process-scope transition owner for
// a task. It also retains the root maintenance lease so cleanup cannot remove
// the durable scope while the guardian is active. Any nonnil returned lease,
// including on error, owns cleanup and requires Release before its Store closes.
func (td *TaskDir) AcquireLifetimeOwner(ctx context.Context) (*LifetimeOwnerLease, error) {
	return td.acquireLifetimeOwnerContext(ctx, false, true)
}

// TryAcquireLifetimeOwner is the nonblocking recovery probe. A busy result
// means the current guardian still owns the transition.
func (td *TaskDir) TryAcquireLifetimeOwner() (_ *LifetimeOwnerLease, resultErr error) {
	return td.tryAcquireLifetimeOwner(context.Background(), true)
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
	return td.tryAcquireLifetimeOwner(ctx, true)
}

// create is true for transition/recovery owners. Observation borrows only an
// existing inode; a missing owner lock is not permission to create one.
func (td *TaskDir) tryAcquireLifetimeOwner(ctx context.Context, create bool) (*LifetimeOwnerLease, error) {
	return td.acquireLifetimeOwnerContext(ctx, true, create)
}

func (td *TaskDir) acquireLifetimeOwnerContext(ctx context.Context, nonblocking, create bool) (*LifetimeOwnerLease, error) {
	if td == nil || td.store == nil || ctx == nil {
		return nil, task.ErrInvalidPermit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lockMaintenance := td.store.maintLock.LockSH
	if nonblocking {
		lockMaintenance = td.store.maintLock.LockSHNonblockingContext
	}
	if err := lockMaintenance(ctx); err != nil {
		return nil, err
	}
	lease := newPartialLifetimeOwner(td.store)
	if err := ctx.Err(); err != nil {
		return finishLifetimeOwnerAcquisition(lease, err)
	}
	dir := filepath.Join(td.Dir, lifetimeDirectory)
	open := td.store.openExistingLock
	if create {
		if err := td.store.mkdirContext(ctx, dir, td.store.faultInjector); err != nil {
			return finishLifetimeOwnerAcquisition(lease, err)
		}
		open = td.store.openLock
	}
	owner, err := open(filepath.Join(dir, lifetimeOwnerLockName), LockLevelRun)
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

func newPartialLifetimeOwner(store *Store) *LifetimeOwnerLease {
	return &LifetimeOwnerLease{store: store, maintenance: store.maintLock, ownerClosed: true}
}

// LifetimeOwnerCleanupError retains the exact lease whose cleanup is unresolved.
// It grants no transition permission. The original borrowed Store must remain
// open until Lease().Release succeeds; native Close errors may remain sticky.
type LifetimeOwnerCleanupError struct {
	lease *LifetimeOwnerLease
	cause error
}

func (e *LifetimeOwnerCleanupError) Error() string {
	if e == nil || e.cause == nil {
		return "lifetime owner acquisition cleanup unresolved"
	}
	return "lifetime owner acquisition cleanup unresolved: " + e.cause.Error()
}

func (e *LifetimeOwnerCleanupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *LifetimeOwnerCleanupError) Lease() *LifetimeOwnerLease {
	if e == nil {
		return nil
	}
	return e.lease
}

func finishLifetimeOwnerAcquisition(lease *LifetimeOwnerLease, cause error) (*LifetimeOwnerLease, error) {
	result := lease.ReleaseWithError(cause)
	if lease.CleanupPending() {
		return lease, result
	}
	return nil, result
}

// ReleaseWithError preserves the work failure and retains this exact cleanup
// owner on release failure. Its original borrowed Store must outlive cleanup.
// An already-pending typed failure for this lease is not retried implicitly.
func (l *LifetimeOwnerLease) ReleaseWithError(cause error) error {
	if l.CleanupPending() && task.AnyErrorNode(cause, func(node error) bool {
		var prior *LifetimeOwnerCleanupError
		return errors.As(node, &prior) && prior.Lease() == l
	}) {
		return cause
	}
	if releaseErr := l.Release(); releaseErr != nil {
		return &LifetimeOwnerCleanupError{lease: l, cause: errors.Join(cause, releaseErr)}
	}
	return cause
}

// RetainsStore is an ownership view, never acquisition or transition authority.
// Only the original borrowed Store is retained while native cleanup is unproved.
func (l *LifetimeOwnerLease) RetainsStore(store *Store) bool {
	if l == nil || store == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.store == store && (!l.ownerClosed || !l.maintenanceUnlocked)
}

// CleanupPending reports unresolved native resources, never transition authority.
func (l *LifetimeOwnerLease) CleanupPending() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.ownerClosed || !l.maintenanceUnlocked
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
		maintenanceErr = l.maintenance.Unlock()
		l.maintenanceUnlocked = maintenanceErr == nil
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
	if l.releaseErr != nil || l.released || l.ownerClosed || l.maintenanceUnlocked {
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

func lifetimeOwnerCleanupFault(err error) bool {
	var cleanup *LifetimeOwnerCleanupError
	return errors.As(err, &cleanup)
}
