package taskdir

import (
	"errors"
	"testing"
)

func TestRootAdmissionCleanupRetainsOnlyOriginalUnresolvedStore(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	other := fixture.other(t)
	lease := fixture.shared(t, fixture.store)
	var absent *RootAdmissionLease
	if absent.CleanupPending() || absent.RetainsStore(fixture.store) || lease.RetainsStore(nil) || lease.RetainsStore(other) {
		t.Fatal("cleanup custody extended beyond the original borrowed Store")
	}
	if !lease.RetainsStore(fixture.store) || !lease.CleanupPending() {
		t.Fatal("held resource lost the original Store dependency")
	}
	fault := &rootAdmissionUnlockFault{name: rootAdmissionLockName, failures: 1, fault: errors.New("cleanup custody unlock fault")}
	fixture.store.SetFaultInjector(fault)
	defer fault.clear()
	if err := lease.Release(); !errors.Is(err, fault.fault) {
		t.Fatalf("expected unresolved native unlock: %v", err)
	}
	if !lease.RetainsStore(fixture.store) || !lease.CleanupPending() {
		t.Fatal("failed release dropped cleanup custody")
	}
	must(t, lease.Release())
	if lease.RetainsStore(fixture.store) || lease.CleanupPending() {
		t.Fatal("successful release retained resolved Store custody")
	}
}

func TestRootAdmissionTransferredCleanupViewEndsWithParentRelease(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	lease := fixture.shared(t, fixture.store)
	duplicate := duplicateAdmission(t, fixture, lease)
	if !lease.RetainsStore(fixture.store) {
		t.Fatal("transfer lost the parent's cleanup dependency")
	}
	must(t, lease.Release())
	if lease.RetainsStore(fixture.store) {
		t.Fatal("the independent duplicate extended parent Store custody")
	}
	requireRootAdmissionBusy(t, fixture, fixture.store)
	must(t, duplicate.Close())
}
