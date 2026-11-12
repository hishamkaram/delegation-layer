package taskdir

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func TestControlCloseRetriesFailedNativeUnlock(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenInspection(strings.Repeat("6", 32), true)
	must(t, err)
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		// Rescue the original owned descriptor even in the old-close
		// counterfactual, whose memoized error would otherwise retain it.
		must(t, control.lock.Close())
	})
	must(t, control.lock.LockEXNonblockingContext(t.Context()))
	failure := errors.New("injected operation unlock failure")
	store.SetFaultInjector(&connectorUnlockFailure{name: ".operation.lock", failure: failure})
	if err = control.Close(); !errors.Is(err, failure) {
		t.Fatalf("failed close returned %v", err)
	}
	store.SetFaultInjector(nil)
	if err = control.Close(); err != nil {
		t.Fatalf("close did not retry the exact retained descriptor: %v", err)
	}
	other, err := store.TryOpenInspectionContext(t.Context(), strings.Repeat("6", 32))
	must(t, err)
	t.Cleanup(func() { must(t, other.Close()) })
	must(t, other.TryWithLock(t.Context(), func(*ControlTransaction) error { return nil }))
}

type observationUnlockFault struct {
	BaseFaultInjector
	name    string
	failure error
	calls   int
}

func (*observationUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (*observationUnlockFault) OnLockParentDirBarrier(string) error { return nil }

func (f *observationUnlockFault) OnFlock(path string, operation int) error {
	if operation == unix.LOCK_UN && filepath.Base(path) == f.name {
		f.calls++
		return f.failure
	}
	return nil
}

func TestTryControlCleanupRetainsExactHandleAndStore(t *testing.T) {
	for _, name := range []string{".operation.lock", ".maintenance.lock"} {
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			control, err := store.OpenInspection(strings.Repeat("5", 32), true)
			must(t, err)
			var retained *ControlObservationCleanupError
			t.Cleanup(func() {
				store.SetFaultInjector(nil)
				if retained != nil {
					must(t, retained.Release())
				}
				must(t, control.Close())
			})
			fault := &observationUnlockFault{name: name, failure: errors.New("injected read cleanup failure")}
			store.SetFaultInjector(fault)
			ctx, cancel := context.WithCancel(t.Context())
			err = control.TryWithLock(ctx, func(*ControlTransaction) error {
				cancel()
				return ctx.Err()
			})
			cancel()
			if !errors.As(err, &retained) || !errors.Is(err, context.Canceled) || !errors.Is(err, fault.failure) || retained.RetainedStore() != store {
				t.Fatalf("cleanup ownership/cause lost: %v", err)
			}
			calls := fault.calls
			err = control.CloseWithError(err)
			if fault.calls != calls || retained.RetainedStore() != store {
				t.Fatal("close retried an already transferred cleanup")
			}
			if closeErr := store.Close(); !errors.Is(closeErr, task.ErrLockBusy) {
				t.Fatalf("store closed before its exact borrower settled: %v", closeErr)
			}
			store.SetFaultInjector(nil)
			must(t, retained.Release())
			if retained.RetainedStore() != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, fault.failure) {
				t.Fatal("release lost historical failure or retained settled authority")
			}
			must(t, control.Close())
			must(t, store.Close())
		})
	}
}

func TestTryControlCleanupRetainsNestedMaintenanceAncestor(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenInspection(strings.Repeat("4", 32), true)
	must(t, err)
	var retained *ControlObservationCleanupError
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if retained != nil {
			must(t, retained.Release())
		}
		must(t, control.Close())
	})
	failure := errors.New("nested operation release failed")
	store.SetFaultInjector(&connectorUnlockFailure{name: ".operation.lock", failure: failure})
	err = store.WithExistingReadObservation(t.Context(), func(ctx context.Context) error {
		return control.TryWithLock(ctx, func(*ControlTransaction) error { return context.Canceled })
	})
	if !errors.As(err, &retained) || !CleanupRetainsStore(err, store) {
		t.Fatalf("nested borrow was discarded: %v", err)
	}
	err = control.CloseWithError(err)
	store.SetFaultInjector(nil)
	must(t, retained.Release())
	if CleanupRetainsStore(err, store) {
		t.Fatal("settled nested borrow still retains Store")
	}
	must(t, store.maintLock.LockEXNonblockingContext(t.Context()))
	must(t, store.maintLock.Unlock())
}

func TestControlCloseConcurrentRetriesJoinOneDescriptor(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenInspection(strings.Repeat("3", 32), true)
	must(t, err)
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		must(t, control.lock.Close())
	})
	must(t, control.lock.LockEXNonblockingContext(t.Context()))
	store.SetFaultInjector(&connectorUnlockFailure{name: ".operation.lock", failure: errors.New("first close fault")})
	if err = control.Close(); err == nil {
		t.Fatal("injected first close unexpectedly passed")
	}
	store.SetFaultInjector(nil)
	var group sync.WaitGroup
	results := make([]error, 2)
	for index := range results {
		group.Go(func() { results[index] = control.Close() })
	}
	group.Wait()
	for _, closeErr := range results {
		must(t, closeErr)
	}
	must(t, store.Close())
}

func TestTryControlTransactionFinishesBeforeConcurrentClose(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cancel      bool
		nestedFault bool
	}{
		{name: "successful callback"},
		{name: "canceled callback", cancel: true},
		{name: "nested canceled callback with unlock fault", cancel: true, nestedFault: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				testTryControlConcurrentClose(t, tc.cancel, tc.nestedFault)
			})
		})
	}
}

func testTryControlConcurrentClose(t *testing.T, canceled, nestedFault bool) {
	t.Helper()
	store := testStore(t)
	control, err := store.OpenInspection(strings.Repeat("2", 32), true)
	must(t, err)
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		must(t, control.Close())
	})
	failure := errors.New("concurrent transaction unlock fault")
	if nestedFault {
		store.SetFaultInjector(&connectorUnlockFailure{name: ".operation.lock", failure: failure})
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observe := func(entered, release chan struct{}) error {
		read := func(readContext context.Context) error {
			return control.TryWithLock(readContext, func(*ControlTransaction) error {
				close(entered)
				<-release
				if canceled {
					cancel()
				}
				return ctx.Err()
			})
		}
		if nestedFault {
			return store.WithExistingReadObservation(ctx, read)
		}
		return read(ctx)
	}
	err, closeErr := finishObservationDuringControlClose(t, control, observe)
	if canceled && !errors.Is(err, context.Canceled) {
		t.Fatalf("callback cancellation lost: %v", err)
	}
	if nestedFault {
		settleConcurrentObservationFault(t, control, err, closeErr, failure)
	} else {
		if !canceled {
			must(t, err)
		}
		must(t, closeErr)
	}
	must(t, control.Close())
	must(t, store.maintLock.LockEXNonblockingContext(t.Context()))
	must(t, store.maintLock.Unlock())
}

func finishObservationDuringControlClose(t *testing.T, control *ControlDir, observe func(entered, release chan struct{}) error) (error, error) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	transactionDone := make(chan error, 1)
	go func() { transactionDone <- observe(entered, release) }()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- control.Close() }()
	synctest.Wait()
	control.mu.Lock()
	closingWithTransaction := control.closed && control.active == 1
	control.mu.Unlock()
	if !closingWithTransaction {
		t.Error("Close did not reach the active-transaction ownership gate")
	}
	close(release)
	synctest.Wait()
	var err error
	select {
	case err = <-transactionDone:
	default:
		t.Error("transaction cleanup waited for Close instead of releasing its own operation lease")
		// Failure-only rescue for the old deadlock: transfer this fixture's
		// sole active count so Close can settle, join both goroutines, then
		// balance its deferred end. Green never uses this failed path.
		control.mu.Lock()
		control.active--
		control.idle.Broadcast()
		control.mu.Unlock()
		err = <-transactionDone
		control.mu.Lock()
		control.active++
		control.mu.Unlock()
	}
	return err, <-closeDone
}

func settleConcurrentObservationFault(t *testing.T, control *ControlDir, err, closeErr, failure error) {
	t.Helper()
	var retained *ControlObservationCleanupError
	if !errors.As(err, &retained) {
		t.Fatalf("concurrent cleanup lost exact owner: %v", err)
	}
	t.Cleanup(func() {
		control.store.SetFaultInjector(nil)
		must(t, retained.Release())
	})
	if !errors.Is(err, failure) || !errors.Is(closeErr, failure) || retained.RetainedStore() != control.store {
		t.Fatalf("concurrent cleanup lost cause/Store: transaction=%v close=%v", err, closeErr)
	}
	err = control.CloseWithError(err)
	control.store.SetFaultInjector(nil)
	must(t, retained.Release())
	if CleanupRetainsStore(err, control.store) || !errors.Is(err, failure) || !errors.Is(err, context.Canceled) {
		t.Fatal("settled concurrent cleanup lost history or retained authority")
	}
}
