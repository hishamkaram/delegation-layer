package supervisor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func joinedLifetimeFailure(t *testing.T) (*helperAdmissionFixture, *taskdir.LifetimeOwnerLease, error) {
	t.Helper()
	fixture := newHelperAdmissionFixture(t)
	control := openHelperControl(t, fixture, fixture.store)
	lease, err := control.AcquireLifetimeOwner(t.Context())
	fixture.owners = append(fixture.owners, lease)
	if err != nil {
		t.Fatal(err)
	}
	fault := &helperMaintenanceCleanupFault{cause: errors.New("joined child maintenance fault")}
	fault.enabled.Store(true)
	fixture.store.SetFaultInjector(fault)
	t.Cleanup(func() { fault.enabled.Store(false) })
	cause := lease.ReleaseWithError(errors.New("work failed"))
	if cause == nil || !lease.CleanupPending() {
		t.Fatal("fixture did not retain native child")
	}
	fault.enabled.Store(false)
	return fixture, lease, cause
}

func TestJoinedRootAdmissionWaitsForLaterPendingChild(t *testing.T) {
	_, historicalLease, historical := joinedLifetimeFailure(t)
	if err := historicalLease.Release(); err != nil {
		t.Fatal(err)
	}
	fixture, child, pending := joinedLifetimeFailure(t)
	root, err := fixture.store.AcquireRootAdmissionContext(t.Context())
	fixture.own(root)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.Join(historical, fmt.Errorf("later: %w", pending))
	retained := ReleaseOwnedRootAdmission(root, cause)
	if !root.CleanupPending() {
		t.Fatal("historical first child hid later pending child and released parent")
	}
	var owner *RootAdmissionCleanupError
	if !errors.As(retained, &owner) || owner.Lease() != root || !errors.Is(retained, cause) {
		t.Fatal("whole original cause or exact root owner lost")
	}
	requireHelperRootBusy(t, fixture.store)
	if err = child.Release(); err != nil {
		t.Fatal(err)
	}
	if err = root.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestJoinedRootAdmissionFindsLaterRetainedSameLease(t *testing.T) {
	for _, operation := range []struct {
		name  string
		apply func(*taskdir.RootAdmissionLease, error) error
	}{{"release", ReleaseOwnedRootAdmission}, {"retain", RetainOwnedRootAdmission}} {
		t.Run(operation.name, func(t *testing.T) {
			fixture := newHelperAdmissionFixture(t)
			first, err := fixture.store.AcquireRootAdmissionContext(t.Context())
			fixture.own(first)
			if err != nil {
				t.Fatal(err)
			}
			second, err := fixture.store.AcquireRootAdmissionContext(t.Context())
			fixture.own(second)
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.Join(RetainOwnedRootAdmission(first, errors.New("first")), fmt.Errorf("later: %w", RetainOwnedRootAdmission(second, errors.New("second"))))
			var expectedFirst, gotFirst *RootAdmissionCleanupError
			if !errors.As(cause, &expectedFirst) {
				t.Fatal("missing first retained owner")
			}
			got := operation.apply(second, cause)
			if !errors.As(got, &gotFirst) || gotFirst != expectedFirst || !second.CleanupPending() {
				t.Fatal("later exact retained owner was retried or wrapped again")
			}
		})
	}
}
