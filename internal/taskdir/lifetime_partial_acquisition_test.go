package taskdir

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

type partialOwnerFault struct {
	BaseFaultInjector
	maintenance bool
	owner       bool
	cancel      context.CancelFunc
	cause       error
}

func (*partialOwnerFault) OnLockFileBarrier(string) error      { return nil }
func (*partialOwnerFault) OnLockParentDirBarrier(string) error { return nil }
func (f *partialOwnerFault) OnFlock(path string, operation int) error {
	name := filepath.Base(path)
	if name == controlLifetimeOwnerLockName || name == lifetimeOwnerLockName {
		if operation&unix.LOCK_EX != 0 && f.cancel != nil {
			f.cancel()
		}
		if operation == unix.LOCK_UN && f.owner {
			return f.cause
		}
	}
	if name == ".maintenance.lock" && operation == unix.LOCK_UN && f.maintenance {
		return f.cause
	}
	return nil
}

func partialLifetimeSubject(t *testing.T, kind string) (*rootAdmissionFixture, func(context.Context, bool) (*LifetimeOwnerLease, error), *LifetimeOwnerLease) {
	t.Helper()
	fixture := newRootAdmissionFixture(t)
	other := fixture.other(t)
	var controls []*ControlDir
	var tasks []*TaskDir
	var held *LifetimeOwnerLease
	t.Cleanup(func() {
		var closeErr error
		closeErr = errors.Join(closeErr, held.Release())
		for _, td := range tasks {
			closeErr = errors.Join(closeErr, td.Close())
		}
		for _, control := range controls {
			closeErr = errors.Join(closeErr, control.Close())
		}
		if closeErr != nil {
			fixture.failed = true
			t.Error(closeErr)
		}
	})
	if kind == "control" {
		control, err := fixture.store.OpenInfrastructureContext(t.Context(), true)
		if control != nil {
			controls = append(controls, control)
		}
		must(t, err)
		peer, err := other.OpenInfrastructureContext(t.Context(), false)
		if peer != nil {
			controls = append(controls, peer)
		}
		must(t, err)
		held, err = peer.AcquireLifetimeOwner(t.Context())
		must(t, err)
		return fixture, func(ctx context.Context, nonblocking bool) (*LifetimeOwnerLease, error) {
			return control.acquireLifetimeOwner(ctx, nonblocking)
		}, held
	}
	req, meta, brief := preparedInput(t, fixture.store)
	td, err := fixture.store.CreateTask(req.TaskID, req, brief, meta)
	if td != nil {
		tasks = append(tasks, td)
	}
	must(t, err)
	peer, err := other.OpenTask(req.TaskID)
	if peer != nil {
		tasks = append(tasks, peer)
	}
	must(t, err)
	held, err = peer.AcquireLifetimeOwner(t.Context())
	must(t, err)
	return fixture, func(ctx context.Context, nonblocking bool) (*LifetimeOwnerLease, error) {
		if nonblocking {
			return td.TryAcquireLifetimeOwnerContext(ctx)
		}
		return td.AcquireLifetimeOwner(ctx)
	}, held
}

func TestLifetimePartialAcquisitionRetainsMaintenanceOnBusy(t *testing.T) {
	for _, kind := range []string{"control", "task"} {
		for _, nonblocking := range []bool{true, false} {
			name := kind + "-blocking"
			if nonblocking {
				name = kind + "-try"
			}
			t.Run(name, func(t *testing.T) {
				assertPartialMaintenanceBusy(t, kind, nonblocking)
			})
		}
	}
}

func TestLifetimePartialAcquisitionRetainsCanceledOwnerClose(t *testing.T) {
	for _, kind := range []string{"control", "task"} {
		t.Run(kind, func(t *testing.T) {
			assertPartialCanceledOwner(t, kind)
		})
	}
}

func TestLifetimePartialConsumerPreservesBusyCleanup(t *testing.T) {
	for _, name := range []string{"stop-observation", "unlaunched-proof"} {
		t.Run(name, func(t *testing.T) {
			assertPartialBusyConsumer(t, name)
		})
	}
}

func TestLifetimePartialReleaseDoesNotRetryTypedFailure(t *testing.T) {
	fixture, acquire, held := partialLifetimeSubject(t, "control")
	must(t, held.Release())
	lease, err := acquire(t.Context(), true)
	must(t, err)
	fault := &partialOwnerFault{maintenance: true, cause: task.ErrLockBusy}
	fixture.store.SetFaultInjector(fault)
	t.Cleanup(func() {
		fault.maintenance = false
		if closeErr := lease.Release(); closeErr != nil {
			fixture.failed = true
			t.Error(closeErr)
		}
	})
	primary := errors.New("work failed")
	failed := lease.ReleaseWithError(primary)
	fault.maintenance = false
	preserved := lease.ReleaseWithError(failed)
	var cleanup *LifetimeOwnerCleanupError
	if !errors.As(preserved, &cleanup) || cleanup.Lease() != lease || !lease.CleanupPending() {
		t.Fatal("typed failure triggered an implicit cleanup retry")
	}
	if !errors.Is(preserved, primary) || !errors.Is(preserved, task.ErrLockBusy) {
		t.Fatal("original work/release causes lost")
	}
	must(t, lease.Release())
	if !errors.Is(lease.ReleaseWithError(failed), primary) {
		t.Fatal("explicit cleanup erased historical work failure")
	}
}

func assertPartialMaintenanceBusy(t *testing.T, kind string, nonblocking bool) {
	t.Helper()
	fixture, acquire, _ := partialLifetimeSubject(t, kind)
	fault := &partialOwnerFault{maintenance: true, cause: errors.New("maintenance unwind fault")}
	fixture.store.SetFaultInjector(fault)
	var partial *LifetimeOwnerLease
	t.Cleanup(func() {
		fault.maintenance = false
		var cleanupErr error
		if partial != nil {
			cleanupErr = partial.Release()
		} else {
			// Pre-fix/counterfactual rescue: this attempt acquired exactly one SH
			// on this Store, while the busy owner uses a separate Store/OFD.
			cleanupErr = fixture.store.maintLock.Unlock()
		}
		if cleanupErr != nil {
			fixture.failed = true
			t.Error(cleanupErr)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if !nonblocking {
		fault.cancel = cancel
	}
	var err error
	partial, err = acquire(ctx, nonblocking)
	expected := error(context.Canceled)
	if nonblocking {
		expected = task.ErrLockBusy
	}
	if !errors.Is(err, expected) || !errors.Is(err, fault.cause) {
		t.Fatalf("primary/unwind error lost: %v", err)
	}
	if partial == nil {
		t.Fatal("maintenance SH cleanup owner discarded")
	}
	var cleanup *LifetimeOwnerCleanupError
	if !errors.As(err, &cleanup) || cleanup.Lease() != partial {
		t.Fatal("error did not retain exact partial handle")
	}
	if !partial.RetainsStore(fixture.store) || partial.RetainsStore(fixture.stores[1]) {
		t.Fatal("original borrowed Store lifetime lost")
	}
	assertPartialCannotExport(t, partial)

	fault.maintenance = false
	must(t, partial.Release())
	if partial.RetainsStore(fixture.store) {
		t.Fatal("released partial still retains original Store")
	}
	// Fixture cleanup closes subject handles first, then proves Store.Close.
}

func assertPartialCanceledOwner(t *testing.T, kind string) {
	t.Helper()
	fixture, acquire, held := partialLifetimeSubject(t, kind)
	must(t, held.Release())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fault := &partialOwnerFault{maintenance: true, owner: true, cancel: cancel, cause: errors.New("partial owner unwind fault")}
	fixture.store.SetFaultInjector(fault)
	var partial *LifetimeOwnerLease
	t.Cleanup(func() {
		fault.owner = false
		fault.maintenance = false
		if partial != nil {
			if err := partial.Release(); err != nil {
				fixture.failed = true
				t.Error(err)
			}
		}
	})
	var err error
	partial, err = acquire(ctx, true)
	var cleanup *LifetimeOwnerCleanupError
	if partial == nil || !errors.As(err, &cleanup) || cleanup.Lease() != partial || !errors.Is(err, context.Canceled) || !errors.Is(err, fault.cause) {
		t.Fatalf("owner-close failure discarded resource/causes: %v", err)
	}
	assertPartialCannotExport(t, partial)
	fault.owner = false
	fault.maintenance = false
	blocked, busyErr := acquire(t.Context(), true)
	if blocked != nil {
		t.Cleanup(func() {
			if closeErr := blocked.Release(); closeErr != nil {
				fixture.failed = true
				t.Error(closeErr)
			}
		})
	}
	if !errors.Is(busyErr, task.ErrLockBusy) {
		t.Fatalf("partial lost native EX before retry: %v", busyErr)
	}
	must(t, partial.Release())
	next, err := acquire(t.Context(), true)
	if next != nil {
		t.Cleanup(func() {
			if closeErr := next.Release(); closeErr != nil {
				fixture.failed = true
				t.Error(closeErr)
			}
		})
	}
	must(t, err)
	must(t, next.Release())
}

func assertPartialBusyConsumer(t *testing.T, name string) {
	t.Helper()
	fixture := newRootAdmissionFixture(t)
	peerStore := fixture.other(t)
	req, meta, brief := preparedInput(t, fixture.store)
	td, err := fixture.store.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	peer, err := peerStore.OpenTask(req.TaskID)
	must(t, err)
	held, err := peer.AcquireLifetimeOwner(t.Context())
	must(t, err)
	fault := &partialOwnerFault{maintenance: true, cause: errors.New("consumer cleanup fault")}
	fixture.store.SetFaultInjector(fault)
	var cleanup *LifetimeOwnerCleanupError
	t.Cleanup(func() {
		fault.maintenance = false
		var closeErr error
		if cleanup != nil {
			closeErr = cleanup.Lease().Release()
		}
		closeErr = errors.Join(closeErr, held.Release(), peer.Close(), td.Close())
		if closeErr != nil {
			fixture.failed = true
			t.Error(closeErr)
		}
	})
	if name == "stop-observation" {
		err = td.RecordStopObservation("unused", task.StopObservationFacts{Terminated: true, State: "ended"})
	} else {
		_, _, err = td.ProveUnlaunchedTaskReferenceContext(t.Context())
	}
	if !errors.As(err, &cleanup) || !errors.Is(err, task.ErrLockBusy) || !errors.Is(err, fault.cause) {
		t.Fatalf("busy partial cleanup was hidden: %v", err)
	}
	if !cleanup.Lease().CleanupPending() || !cleanup.Lease().RetainsStore(fixture.store) {
		t.Fatal("consumer lost original Store/lease")
	}
	fault.maintenance = false
	must(t, cleanup.Lease().Release())
	if cleanup.Lease().CleanupPending() {
		t.Fatal("completed cleanup still pending")
	}
}

func assertPartialCannotExport(t *testing.T, partial *LifetimeOwnerLease) {
	t.Helper()
	files, dupErr := partial.DuplicateControlLeaseFiles()
	for _, file := range files {
		must(t, file.Close())
	}
	if dupErr == nil || len(files) != 0 {
		t.Fatal("failed-release handle exported native locks")
	}
}
