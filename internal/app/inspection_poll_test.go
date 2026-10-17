package app

import (
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
)

func TestStagedAuxiliaryRecoveryPreservesPendingBeforeRaceFallback(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	writeInspectionPollTestStatus(t, statusPath, operation, false)
	observation, err := bound.ReconcileInspection(t.Context(), operation.Request().SupervisorIdentity())
	if err != nil || !observation.Matched {
		t.Fatalf("prepare exact queue observation: %+v %v", observation, err)
	}
	observation.Job.State = pueue.StateStashed
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	defer release()
	var statuses atomic.Int32
	client := newObservedInspectionTestClient(t, bound, logPath, 50*time.Millisecond, func(event pueue.CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "status") && statuses.Add(1) == 1 {
			<-released
		}
	})
	readbacks := 0
	_, err = supervisor.RecoverStagedAuxiliaryObservation(t.Context(), client, store.Root, observation,
		func(current pueue.InspectionObservation) error {
			return operation.RecordReceiptContext(t.Context(), current.Job.ID)
		},
		func(int64) error { readbacks++; return nil }, nil)
	pending := assertRetainedInspectionPending(t, err)
	if readbacks != 0 {
		t.Errorf("pending command reached completed-race receipt fallback %d times", readbacks)
	}
	if err = client.CloseAfterPending(err); err != nil {
		t.Errorf("pending client cleanup failed: %v", err)
	}
	release()
	drainInspectionPending(t, pending)
	if got := statuses.Load(); got != 1 {
		t.Fatalf("pending recovery started %d observations, want one", got)
	}
}
