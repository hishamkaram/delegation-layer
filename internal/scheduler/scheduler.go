// Package scheduler constructs the recurring-schedule scheduler's storage for
// one state root. It is unrelated to the pueue supervisor that runs admitted
// tasks. Ordinary task commands do not open this optional storage.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/internal/sqlitestore"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const ownerLockName = "owner.lock"

// Open opens the current scheduler store of a state root. It never creates or
// migrates the database and does not take the owner lease, so it works while
// another process holds that lease. A missing or uninitialized database
// returns an fs.ErrNotExist error.
func Open(ctx context.Context, store *taskdir.Store) (contract.Store, error) {
	if store == nil {
		return nil, errors.New("scheduler open requires a state store")
	}
	opened, err := sqlitestore.Open(ctx, store.Root, store.RootID)
	if err != nil {
		return nil, err
	}
	return opened, nil
}

// OpenReadOnly opens existing scheduler metadata for passive commands without
// acquiring execution capabilities or running task-store filesystem probes.
// SQLite retains its normal bounded transaction and contention semantics.
func OpenReadOnly(ctx context.Context, store *taskdir.ReadOnlyStore) (contract.Store, error) {
	if store == nil {
		return nil, errors.New("scheduler read requires a state reader")
	}
	return sqlitestore.OpenReadOnly(ctx, store.Root(), store.RootID())
}

// OwnerLease is the exclusive scheduler owner of one state root. Acquire it
// before any task or supervisor lease and never while holding one; taskdir
// does not enforce that order at runtime. It is the only authority to create
// or migrate the scheduler database.
type OwnerLease struct {
	mu     sync.Mutex
	root   string
	rootID string
	// lock is the held lease. It stays set until its Close succeeds, so a
	// failed close can be retried; closing refuses Migrate from the first
	// Close on.
	lock    ownerLock
	closing bool
}

// ownerLock is the part of the held taskdir lease this facade releases.
type ownerLock interface {
	Close() error
}

// AcquireOwnerLease takes the root's scheduler owner lease without waiting.
// A lease held elsewhere returns task.ErrLockBusy.
func AcquireOwnerLease(ctx context.Context, store *taskdir.Store) (*OwnerLease, error) {
	if store == nil {
		return nil, errors.New("scheduler owner lease requires a state store")
	}
	lock, err := taskdir.OpenLockFileContext(ctx, filepath.Join(sqlitestore.Directory(store.Root), ownerLockName), taskdir.LockLevelNone)
	if err != nil {
		return nil, fmt.Errorf("opening scheduler owner lease: %w", err)
	}
	if err := lock.LockEXNonblockingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("acquiring scheduler owner lease: %w", err), lock.Close())
	}
	return &OwnerLease{root: store.Root, rootID: store.RootID, lock: lock}, nil
}

// Migrate creates or migrates the scheduler database while this lease is
// held. It uses the held lease and never acquires another.
func (l *OwnerLease) Migrate(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing || l.lock == nil {
		return fmt.Errorf("scheduler owner lease: %w", os.ErrClosed)
	}
	return sqlitestore.Initialize(ctx, l.root, l.rootID)
}

// Close releases the lease; Migrate refuses from the first call on. If the
// release fails, the lease keeps its handle and a later Close retries it.
func (l *OwnerLease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closing = true
	if l.lock == nil {
		return nil
	}
	if err := l.lock.Close(); err != nil {
		return fmt.Errorf("releasing scheduler owner lease: %w", err)
	}
	l.lock = nil
	return nil
}
