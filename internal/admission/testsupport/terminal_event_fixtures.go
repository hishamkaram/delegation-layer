package testsupport

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func WaitForPrivateTerminalEventRetirement(t *testing.T, ctx context.Context, supervisor *pueue.Client, id int64) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, found, err := supervisor.ReadPrivateTerminalEvent(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			WaitForPrivateQueueMaintenanceIdle(t, ctx, supervisor)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("accepted terminal event was not retired: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func WaitForPrivateQueueMaintenanceIdle(t *testing.T, ctx context.Context, supervisor *pueue.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stateRoot := filepath.Dir(filepath.Dir(supervisor.Binding().ConfigPath))
	journalPath := filepath.Join(stateRoot, ".supervisor", "queue-maintenance.json")
	failMaintenanceWait := func(stage string, waitErr error) {
		journal, journalErr := os.ReadFile(journalPath)
		t.Fatalf("private queue maintenance %s: %v journal=%q journal_error=%v", stage, waitErr, journal, journalErr)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		pending, err := pueue.PrivateQueueMaintenanceRetryNeededContext(ctx, stateRoot)
		if err != nil {
			failMaintenanceWait("retry-state check failed", err)
		}
		if !pending {
			if err = pueue.WaitForPrivateQueueMaintenanceIdle(ctx, stateRoot); err != nil {
				failMaintenanceWait("owner handoff failed", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			failMaintenanceWait("did not become idle", ctx.Err())
		case <-ticker.C:
		}
	}
}

func NewRecordedTerminalCleanupFixture(t *testing.T, evidence supervisorcore.InspectionEvidence) (*taskdir.Store, *inspection.Operation, *pueue.Client, pueue.Job) {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	supervisor, _, _ := NewTerminalEventInspectionSupervisorWithEnvironment(t, store, evidence)
	operation, job := NewTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordReceiptContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	WriteTerminalEventStatus(t, store, job, TerminalEventDoneState(true))
	if err := supervisor.RegisterPrivateTerminalEvent(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	return store, operation, supervisor, job
}
