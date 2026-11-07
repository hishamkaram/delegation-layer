package supervisor_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

func recordMigrationClosure(t *testing.T, td *taskdir.TaskDir, cause string, wait bool) {
	t.Helper()
	lifecyclefixture.RecordRecoverableLifetime(t, td, "migration-test-boot")
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := owner.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if wait {
		worker, readErr := td.ReadLifetimeWorkerContext(t.Context())
		if readErr != nil {
			t.Fatal(readErr)
		}
		data, marshalErr := task.MarshalCanonical(worker)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		code := 1
		if _, err = td.RecordLifetimeWorkerWaitContext(t.Context(), taskdir.LifetimeWorkerWaitFacts{
			WorkerSHA256: task.ComputeSHA256(data), ChildPID: worker.PID,
			Result: task.LifetimeWorkerWaitExitError, ExitCode: &code,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = td.RecordLifetimeTerminationContext(t.Context(), cause); err != nil {
		t.Fatal(err)
	}
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationAcceptsClosedUnpublishedTaskButCleanupRetainsRow(t *testing.T) {
	for _, cause := range []string{task.LifetimeTerminationBudget, task.LifetimeTerminationRequested} {
		t.Run(cause, func(t *testing.T) {
			store, td, _ := lifecyclefixture.NewAppTestTask(t, false)
			t.Cleanup(func() { lifecyclefixture.CloseAppTestTask(t, store, td) })
			meta := lifecyclefixture.PrepareAppBudgetRecords(t, td)
			recordMigrationClosure(t, td, cause, true)
			label := "delegate:" + store.RootID + ":" + td.TaskID
			job := pueue.Job{ID: 7, Label: &label, State: pueue.StateEnded, Group: "default"}
			group := supervisor.InspectionGroup(store.RootID, meta.SupervisorConfig)
			cleanup := supervisor.NewCleanup(nil, nil)
			if err := cleanup.InspectEndedJob(t.Context(), store, group, job); err != nil {
				t.Fatalf("closed unpublished task blocks migration: %v", err)
			}
			plan, err := cleanup.AuthorizeSuccessfulCleanup(t.Context(), store.Root, meta.SupervisorConfig, []pueue.Job{job})
			if err != nil || len(plan.RemoveIDs) != 0 {
				t.Fatalf("unpublished task row removed: %+v %v", plan, err)
			}
			result, err := td.Inspect()
			if err != nil || result.Publication != task.PublicationUnknown || result.SealExists || result.OutcomeExists {
				t.Fatalf("migration fabricated publication: %+v %v", result, err)
			}
		})
	}
}

func TestMigrationRefusesIncompleteUnpublishedTaskEvidence(t *testing.T) {
	for _, mode := range []string{"no-closure", "no-worker-wait", "runner-held", "pending-payload", "bad-receipt", "corrupt-closure"} {
		t.Run(mode, func(t *testing.T) {
			store, td, _ := lifecyclefixture.NewAppTestTask(t, false)
			t.Cleanup(func() { lifecyclefixture.CloseAppTestTask(t, store, td) })
			meta := lifecyclefixture.PrepareAppBudgetRecords(t, td)
			prepareIncompleteMigrationEvidence(t, td, mode)
			label := "delegate:" + store.RootID + ":" + td.TaskID
			job := pueue.Job{ID: 7, Label: &label, State: pueue.StateEnded, Group: "default"}
			if mode == "bad-receipt" {
				job.ID = 8
			}
			group := supervisor.InspectionGroup(store.RootID, meta.SupervisorConfig)
			err := supervisor.NewCleanup(nil, nil).InspectEndedJob(t.Context(), store, group, job)
			if err == nil {
				t.Fatal("incomplete or conflicting task evidence authorized migration")
			}
			if mode == "bad-receipt" && !errors.Is(err, inspection.ErrGroupMigrationRequired) {
				t.Fatalf("receipt mismatch lost error: %v", err)
			}
		})
	}
}

func prepareIncompleteMigrationEvidence(t *testing.T, td *taskdir.TaskDir, mode string) {
	t.Helper()
	if mode != "no-closure" {
		recordMigrationClosure(t, td, task.LifetimeTerminationBudget, mode != "no-worker-wait")
	}
	switch mode {
	case "runner-held":
		holdMigrationRunner(t, td)
	case "pending-payload":
		if err := os.WriteFile(filepath.Join(td.Dir, "result.txt"), []byte("not selected"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "corrupt-closure":
		if err := os.WriteFile(filepath.Join(td.Dir, "lifetime", "terminated.json"), []byte("not JSON"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func holdMigrationRunner(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	lease, err := taskdir.OpenLockFile(filepath.Join(td.Dir, ".run.lock"), taskdir.LockLevelRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = lease.LockEX(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if unlockErr := lease.Unlock(); unlockErr != nil {
			t.Error(unlockErr)
		}
	})
}
