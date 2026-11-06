package host

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
)

func TestForegroundRunFencesOlderManagedHostReceipt(t *testing.T) {
	config := testConfig(t, "hold")
	process := startHostFixture(t, config)
	query := queryFor(t, config)
	old := eventuallyStatus(t, query, func(status Status) bool { return status.Ready })
	stopped, err := Stop(t.Context(), query)
	if err != nil || !stopped.Stopped {
		t.Fatalf("initial host stop: %+v %v", stopped, err)
	}
	receive(t, process.done)
	lease, err := AcquireHostLease(t.Context(), config.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	foreground, err := lease.AnnounceForeground(t.Context(), config.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := foreground.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	execution, err := scheduler.AcquireOwnerLease(t.Context(), config.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := execution.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	assertForegroundActive(t, query, old.Epoch)
	if err = execution.Close(); err != nil {
		t.Fatal(err)
	}
	if err = foreground.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := Observe(t.Context(), query)
	if err != nil || after.State != "no_host_observed" {
		t.Fatalf("finished foreground run remained active: %+v %v", after, err)
	}
}

func assertForegroundActive(t *testing.T, query Query, oldEpoch string) {
	t.Helper()
	status, err := Observe(t.Context(), query)
	if err != nil || status.State != "foreground" || status.Epoch == oldEpoch {
		t.Fatalf("foreground did not replace the old receipt: %+v %v", status, err)
	}
	stopped, err := Stop(t.Context(), query)
	if !errors.Is(err, ErrPending) || stopped.Accepted || stopped.Stopped || stopped.Status.State != "foreground" {
		t.Fatalf("old receipt falsely stopped foreground run: %+v %v", stopped, err)
	}
}
