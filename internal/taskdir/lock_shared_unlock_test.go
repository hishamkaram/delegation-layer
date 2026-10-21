package taskdir

import (
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const maintenanceLockBase = ".maintenance.lock"

func TestLastSharedUnlockFailureRetainsOwnershipUntilRetry(t *testing.T) {
	store := testStore(t)
	other := independentStore(t, store)
	injected := errors.New("injected last shared unlock failure")
	fault := ownMaintenanceUnlockFault(t, store, injected)

	must(t, store.maintLock.LockSHNonblocking())
	if err := store.maintLock.Unlock(); !errors.Is(err, injected) {
		t.Fatalf("last shared unlock error = %v, want injected failure", err)
	}
	requireMaintenanceExclusiveBusy(t, other, "after failed last shared unlock")

	must(t, store.maintLock.Unlock())
	if got := fault.unlockCalls(maintenanceLockBase); got != 2 {
		t.Fatalf("native unlock calls after retry = %d, want 2", got)
	}
	requireMaintenanceExclusiveFree(t, other)
}

func TestSharedUnlockReleasesKernelLeaseOnlyWithLastReference(t *testing.T) {
	store := testStore(t)
	other := independentStore(t, store)
	injected := errors.New("injected final shared unlock failure")
	fault := ownMaintenanceUnlockFault(t, store, injected)

	must(t, store.maintLock.LockSHNonblocking())
	must(t, store.maintLock.LockSHNonblocking())
	must(t, store.maintLock.Unlock())
	if got := fault.unlockCalls(maintenanceLockBase); got != 0 {
		t.Fatalf("native unlock calls with a remaining shared reference = %d, want 0", got)
	}
	requireMaintenanceExclusiveBusy(t, other, "with a remaining shared reference")

	if err := store.maintLock.Unlock(); !errors.Is(err, injected) {
		t.Fatalf("final shared unlock error = %v, want injected failure", err)
	}
	if got := fault.unlockCalls(maintenanceLockBase); got != 1 {
		t.Fatalf("native unlock calls after failed final unlock = %d, want 1", got)
	}
	requireMaintenanceExclusiveBusy(t, other, "after failed final shared unlock")

	must(t, store.maintLock.Unlock())
	if got := fault.unlockCalls(maintenanceLockBase); got != 2 {
		t.Fatalf("native unlock calls after final retry = %d, want 2", got)
	}
	requireMaintenanceExclusiveFree(t, other)
}

func TestCloseReleasesSharedLeaseRetainedByFailedUnlock(t *testing.T) {
	store := testStore(t)
	other := independentStore(t, store)
	injected := errors.New("injected shared unlock failure before close")
	fault := ownMaintenanceUnlockFault(t, store, injected)

	must(t, store.maintLock.LockSHNonblocking())
	if err := store.maintLock.Unlock(); !errors.Is(err, injected) {
		t.Fatalf("shared unlock error = %v, want injected failure", err)
	}
	requireMaintenanceExclusiveBusy(t, other, "after failed shared unlock")

	must(t, store.maintLock.Close())
	if got := fault.unlockCalls(maintenanceLockBase); got != 2 {
		t.Fatalf("native unlock calls after close = %d, want 2", got)
	}
	if err := store.maintLock.LockSHNonblocking(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("shared lock after close error = %v, want closed descriptor", err)
	}
	requireMaintenanceExclusiveFree(t, other)
}

func TestExclusiveUnlockFailureRetainsOwnershipUntilRetry(t *testing.T) {
	store := testStore(t)
	other := independentStore(t, store)
	injected := errors.New("injected exclusive unlock failure")
	fault := ownMaintenanceUnlockFault(t, store, injected)

	must(t, store.maintLock.LockEXNonblocking())
	if err := store.maintLock.Unlock(); !errors.Is(err, injected) {
		t.Fatalf("exclusive unlock error = %v, want injected failure", err)
	}
	requireMaintenanceExclusiveBusy(t, other, "after failed exclusive unlock")

	must(t, store.maintLock.Unlock())
	if got := fault.unlockCalls(maintenanceLockBase); got != 2 {
		t.Fatalf("native unlock calls after exclusive retry = %d, want 2", got)
	}
	requireMaintenanceExclusiveFree(t, other)
}

func TestProbeMaintenanceUnlockFailureKeepsStoreOwned(t *testing.T) {
	store := testStore(t)
	other := independentStore(t, store)
	injected := errors.New("injected probe maintenance unlock failure")
	fault := &lifetimeOwnerReleaseFault{failPath: maintenanceLockBase, failures: []error{injected}}
	store.SetFaultInjector(fault)
	t.Cleanup(func() {
		// Release the one reference the failed probe retained, if the body
		// stopped before its own retry, so the shared store cleanup can close.
		store.SetFaultInjector(nil)
		must(t, store.maintLock.Unlock())
	})

	if err := store.ProbeFilesystemSupport(); !errors.Is(err, injected) {
		t.Fatalf("probe error = %v, want injected maintenance unlock failure", err)
	}
	if err := store.Close(); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("store close after failed probe unlock = %v, want lock busy", err)
	}
	requireMaintenanceExclusiveBusy(t, other, "after failed probe maintenance unlock")

	store.SetFaultInjector(nil)
	must(t, store.maintLock.Unlock())
	if got := fault.unlockCalls(maintenanceLockBase); got != 1 {
		t.Fatalf("injected unlock calls = %d, want 1 before the fault was cleared", got)
	}
	requireMaintenanceExclusiveFree(t, other)
	must(t, store.Close())
}

// ownMaintenanceUnlockFault installs one injected maintenance unlock failure
// for a test that owns every maintenance reference on store. Cleanup clears the
// fault and closes that owned LockFile, releasing any reference a failed body
// left behind, before the shared store cleanup runs.
func ownMaintenanceUnlockFault(t *testing.T, store *Store, failure error) *lifetimeOwnerReleaseFault {
	t.Helper()
	fault := &lifetimeOwnerReleaseFault{failPath: maintenanceLockBase, failures: []error{failure}}
	store.SetFaultInjector(fault)
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		must(t, store.maintLock.Close())
	})
	return fault
}

func independentStore(t *testing.T, store *Store) *Store {
	t.Helper()
	other, err := OpenStore(store.Root)
	must(t, err)
	t.Cleanup(func() { must(t, other.Close()) })
	return other
}

func requireMaintenanceExclusiveBusy(t *testing.T, other *Store, state string) {
	t.Helper()
	err := other.maintLock.LockEXNonblocking()
	if err == nil {
		releaseErr := other.maintLock.Unlock()
		t.Fatalf("independent exclusive maintenance lock %s acquired, want lock busy (release: %v)", state, releaseErr)
	}
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("independent exclusive maintenance lock %s = %v, want lock busy", state, err)
	}
}

func requireMaintenanceExclusiveFree(t *testing.T, other *Store) {
	t.Helper()
	must(t, other.maintLock.LockEXNonblocking())
	must(t, other.maintLock.Unlock())
}
