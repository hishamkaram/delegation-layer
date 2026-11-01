package host

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Lease owns H only. Compatibility run takes it before E and releases it after
// E; the managed host retains it through all writes, controls, readers and Wait.
type Lease struct {
	mu     sync.Mutex
	lock   *taskdir.LockFile
	rootID string
	root   string
}

func AcquireHostLease(ctx context.Context, store *taskdir.Store) (*Lease, error) {
	if err := validateStore(ctx, store); err != nil {
		return nil, err
	}
	lock, err := taskdir.OpenLockFileContext(ctx, filepath.Join(store.Root, ".scheduler-host.lock"), taskdir.LockLevelNone)
	if err != nil {
		return nil, err
	}
	if err = lock.LockEXNonblockingContext(ctx); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return &Lease{lock: lock, rootID: store.RootID, root: store.Root}, nil
}

func (l *Lease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lock == nil {
		return nil
	}
	if err := l.lock.Close(); err != nil {
		return err
	}
	l.lock = nil
	return nil
}

func acquireServeLease(ctx context.Context, store *taskdir.Store) (*Lease, error) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		lease, err := AcquireHostLease(bounded, store)
		if err == nil || !lockBusyOnly(err) {
			return lease, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-bounded.Done():
			timer.Stop()
			return nil, errors.Join(ErrPending, err, bounded.Err())
		case <-timer.C:
		}
	}
}

func lockBusyOnly(err error) bool {
	return task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, task.ErrLockBusy) || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)
	}) && errors.Is(err, task.ErrLockBusy)
}

func validateStore(ctx context.Context, store *taskdir.Store) (resultErr error) {
	if ctx == nil || store == nil {
		return fmt.Errorf("scheduler host requires a context and existing store")
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, store.Root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	if reader.RootID() != store.RootID {
		return ErrIdentity
	}
	return nil
}
