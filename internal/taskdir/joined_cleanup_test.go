package taskdir

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestJoinedLifetimeCleanupDoesNotRetryLaterMatchingLease(t *testing.T) {
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
	pending := lease.ReleaseWithError(errors.New("pending work"))
	priorFixture, priorAcquire, priorHeld := partialLifetimeSubject(t, "control")
	must(t, priorHeld.Release())
	priorLease, err := priorAcquire(t.Context(), true)
	must(t, err)
	priorFixture.store.SetFaultInjector(fault)
	historical := priorLease.ReleaseWithError(errors.New("historical work"))
	fault.maintenance = false
	must(t, priorLease.Release())
	joined := errors.Join(historical, fmt.Errorf("nested: %w", pending))
	if got := lease.ReleaseWithError(joined); reflect.TypeOf(got) != reflect.TypeOf(joined) || !errors.Is(got, joined) || !lease.CleanupPending() {
		t.Fatal("later matching cleanup lease was implicitly retried")
	}
	must(t, lease.Release())
	if got := lease.ReleaseWithError(joined); !errors.Is(got, joined) || !errors.Is(got, task.ErrLockBusy) {
		t.Fatal("explicit settlement erased historical typed failure")
	}
}
