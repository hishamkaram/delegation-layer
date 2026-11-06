package supervisor

import (
	"errors"
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
