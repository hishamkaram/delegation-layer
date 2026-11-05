package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type helperAdmissionFixture struct {
	store       *taskdir.Store
	leases      []*taskdir.RootAdmissionLease
	owners      []*taskdir.LifetimeOwnerLease
	controls    []*taskdir.ControlDir
	otherStores []*taskdir.Store
}

func newHelperAdmissionFixture(t *testing.T) *helperAdmissionFixture {
	t.Helper()
	parent, err := os.MkdirTemp("", "admission-helper-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &helperAdmissionFixture{}
	t.Cleanup(func() {
		if closeErr := fixture.closeOwned(); closeErr != nil {
			t.Error(closeErr)
			t.Logf("retained unresolved admission helper fixture: %s", parent)
			return
		}
		if removeErr := os.RemoveAll(parent); removeErr != nil {
			t.Error(removeErr)
		}
	})
	fixture.store, err = taskdir.InitStore(filepath.Join(parent, "state"))
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *helperAdmissionFixture) closeOwned() error {
	var resultErr error
	for index := len(f.owners) - 1; index >= 0; index-- {
		resultErr = errors.Join(resultErr, f.owners[index].Release())
	}
	if resultErr != nil {
		return resultErr
	}
	for index := len(f.controls) - 1; index >= 0; index-- {
		resultErr = errors.Join(resultErr, f.controls[index].Close())
	}
	if resultErr != nil {
		return resultErr
	}
	for _, lease := range f.leases {
		resultErr = errors.Join(resultErr, lease.Release())
	}
	if resultErr != nil {
		return resultErr
	}
	for _, store := range f.otherStores {
		resultErr = errors.Join(resultErr, store.Close())
	}
	if resultErr != nil {
		return resultErr
	}
	if f.store != nil {
		return f.store.Close()
	}
	return nil
}

func (f *helperAdmissionFixture) own(lease *taskdir.RootAdmissionLease) {
	if lease != nil {
		f.leases = append(f.leases, lease)
	}
}

func TestAcquireOrBorrowRootAdmissionUsesExactSharedResource(t *testing.T) {
	fixture := newHelperAdmissionFixture(t)
	store := fixture.store
	lease, err := AcquireOrBorrowRootAdmissionContext(t.Context(), store, nil, pueue.PrivateControlTimeout(pueue.Options{}))
	fixture.own(lease)
	if err != nil {
		t.Fatal(err)
	}
	borrowed, err := AcquireOrBorrowRootAdmissionContext(t.Context(), store, lease, pueue.PrivateControlTimeout(pueue.Options{}))
	if err != nil || borrowed != lease {
		t.Fatalf("borrowed another native resource: %v", err)
	}
	if err = lease.CheckRootContext(t.Context(), store); err != nil {
		t.Fatalf("acquisition cancellation escaped into work: %v", err)
	}
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(exclusive)
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("borrow released outer SH: %v", err)
	}
	if err = lease.Release(); err != nil {
		t.Fatal(err)
	}
	exclusive, err = store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(exclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = AcquireOrBorrowRootAdmissionContext(t.Context(), store, exclusive, pueue.PrivateControlTimeout(pueue.Options{})); err == nil {
		t.Fatal("EX masqueraded as shared admission")
	}
}

func TestAcquireOrBorrowRootAdmissionRefusals(t *testing.T) {
	fixture := newHelperAdmissionFixture(t)
	store := fixture.store
	other := newHelperAdmissionFixture(t).store
	lease, err := AcquireOrBorrowRootAdmissionContext(t.Context(), store, nil, pueue.PrivateControlTimeout(pueue.Options{}))
	fixture.own(lease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = AcquireOrBorrowRootAdmissionContext(t.Context(), other, lease, pueue.PrivateControlTimeout(pueue.Options{})); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("foreign root borrowed: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = AcquireOrBorrowRootAdmissionContext(ctx, store, lease, pueue.PrivateControlTimeout(pueue.Options{})); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled borrow: %v", err)
	}
	if err = lease.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err = AcquireOrBorrowRootAdmissionContext(t.Context(), store, lease, pueue.PrivateControlTimeout(pueue.Options{})); err == nil {
		t.Fatal("closed borrow accepted")
	}
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(exclusive)
	if err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	blocked, err := AcquireOrBorrowRootAdmissionContext(bounded, store, nil, pueue.PrivateControlTimeout(pueue.Options{}))
	fixture.own(blocked)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy EX did not prevent admission: %v", err)
	}
}

type helperAdmissionReleaseFault struct {
	taskdir.BaseFaultInjector
	enabled atomic.Bool
	cause   error
}

func (*helperAdmissionReleaseFault) OnLockFileBarrier(string) error      { return nil }
func (*helperAdmissionReleaseFault) OnLockParentDirBarrier(string) error { return nil }
func (f *helperAdmissionReleaseFault) OnFlock(path string, operation int) error {
	if f.enabled.Load() && filepath.Base(path) == taskdir.RootAdmissionLockName && operation == unix.LOCK_UN {
		return f.cause
	}
	return nil
}

func TestReleaseOwnedRootAdmissionRetainsExactFailedResource(t *testing.T) {
	fixture := newHelperAdmissionFixture(t)
	lease, err := AcquireOrBorrowRootAdmissionContext(t.Context(), fixture.store, nil, pueue.PrivateControlTimeout(pueue.Options{}))
	fixture.own(lease)
	if err != nil {
		t.Fatal(err)
	}
	fault := &helperAdmissionReleaseFault{cause: errors.New("native root unlock fault")}
	fault.enabled.Store(true)
	fixture.store.SetFaultInjector(fault)
	defer fault.enabled.Store(false)
	workErr := errors.New("original work failed")
	err = ReleaseOwnedRootAdmission(lease, workErr)
	var pending *RootAdmissionCleanupError
	if !errors.As(err, &pending) || pending.Lease() != lease || !errors.Is(err, workErr) || !errors.Is(err, fault.cause) {
		t.Fatalf("failed release lost resource/causes: %v", err)
	}
	if err = lease.CheckRootContext(t.Context(), fixture.store); err != nil {
		t.Fatalf("failed release lost actual hold: %v", err)
	}
	fault.enabled.Store(false)
	err = ReleaseOwnedRootAdmission(pending.Lease(), workErr)
	if !errors.Is(err, workErr) {
		t.Fatalf("retry changed original work error: %v", err)
	}
	if err = lease.CheckRootContext(t.Context(), fixture.store); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("retry did not close native resource: %v", err)
	}
}
