package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestPrivateSuccessfulCleanupRejectsUnsafeInspectionObservation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
	}{
		{name: "saved failure conflicts with observed success"},
		{name: "running row cannot establish worker exit", running: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertUnsafeInspectionCleanupRejected(t, tc.running)
		})
	}
}

func TestPrivateMigrationRejectsDuplicateInspectionLabelsBeforeObservation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		matchingFirst bool
	}{
		{name: "matching receipt first", matchingFirst: true},
		{name: "duplicate first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertDuplicateInspectionMigrationRejected(t, tc.matchingFirst)
		})
	}
}

func TestFailedManagedInspectionWithoutCompletionAuthorizesCleanupAndMigration(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		name := "cleanup"
		if migrate {
			name = "migration"
		}
		t.Run(name, func(t *testing.T) {
			assertFailedManagedInspectionWithoutCompletionAuthorized(t, migrate)
		})
	}
}

func assertFailedManagedInspectionWithoutCompletionAuthorized(t *testing.T, migrate bool) {
	t.Helper()
	store, operation, job := newCleanupInspectionTestOperationWithOwnership(
		t, inspection.ResultUnavailable, false, false, task.RunnerOwnershipManaged,
	)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	operationDirectory := filepath.Join(store.Root, "inspections", operation.Request().TaskID)
	for _, record := range []string{"completion.json", "result.json"} {
		if err := os.Remove(filepath.Join(operationDirectory, record)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := operation.ReadCompletedContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture retained local completion: %v", err)
	}

	binding := operation.Request().Binding.Supervisor
	if err := preparePrivateTerminalEvidence(t.Context(), store.Root, binding, []pueue.Job{job}); err != nil {
		t.Fatal(err)
	}
	if migrate {
		if err := authorizePrivateMigrationQueue(t.Context(), store.Root, binding, []pueue.Job{job}); err != nil {
			t.Fatalf("proven failed inspection blocked migration: %v", err)
		}
	} else {
		plan, err := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, binding, []pueue.Job{job})
		if err != nil || len(plan.RemoveIDs) != 1 || plan.RemoveIDs[0] != job.ID || plan.Settling {
			t.Fatalf("proven failed inspection cleanup plan=%+v err=%v", plan, err)
		}
	}
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if err != nil || succeeded || result != nil {
		t.Fatalf("failed inspection result=%+v succeeded=%t err=%v", result, succeeded, err)
	}
}

func TestManagedInspectionCleanupUsesGuardianEvidenceOverQueueResult(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		name := "cleanup"
		if migrate {
			name = "migration"
		}
		t.Run(name, func(t *testing.T) {
			store, operation, job := newCleanupInspectionTestOperationWithOwnership(
				t, inspection.ResultEligible, false, false, task.RunnerOwnershipManaged,
			)
			completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
			binding := operation.Request().Binding.Supervisor
			if migrate {
				if err := authorizePrivateMigrationQueue(t.Context(), store.Root, binding, []pueue.Job{job}); err != nil {
					t.Fatalf("guardian queue failure blocked child-proven migration: %v", err)
				}
			} else {
				plan, err := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, binding, []pueue.Job{job})
				if err != nil || len(plan.RemoveIDs) != 1 || plan.RemoveIDs[0] != job.ID || plan.Settling {
					t.Fatalf("guardian queue failure cleanup plan=%+v err=%v", plan, err)
				}
			}
			result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
			if err != nil || !succeeded || result == nil || result.Reason != inspection.ResultEligible {
				t.Fatalf("guardian result=%+v succeeded=%t err=%v", result, succeeded, err)
			}
		})
	}
}

func assertUnsafeInspectionCleanupRejected(t *testing.T, running bool) {
	t.Helper()
	store, operation, job := newCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	if running {
		job.State = pueue.StateRunning
		job.Succeeded = false
	} else if recordErr := operation.RecordWorkerExitContext(t.Context(), job.ID, false); recordErr != nil {
		t.Fatalf("seed independently observed worker failure: %v", recordErr)
	}
	plan, cleanupErr := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, operation.Request().Binding.Supervisor, []pueue.Job{job})
	if cleanupErr != nil || len(plan.RemoveIDs) != 0 || plan.Settling {
		t.Fatalf("unsafe inspection row was authorized for cleanup: plan=%+v err=%v", plan, cleanupErr)
	}
	result, succeeded, resultErr := operation.ReadWorkerResultContext(t.Context())
	if running {
		if !errors.Is(resultErr, os.ErrNotExist) {
			t.Fatalf("running row published worker-exit authority: result=%+v succeeded=%t err=%v", result, succeeded, resultErr)
		}
		return
	}
	if resultErr != nil || result.Reason != inspection.ResultEligible || succeeded {
		t.Fatalf("conflicting queue success replaced saved worker failure: result=%+v succeeded=%t err=%v", result, succeeded, resultErr)
	}
	assertCleanupInspectionTestProof(t, operation, false)
}

func assertDuplicateInspectionMigrationRejected(t *testing.T, matchingFirst bool) {
	t.Helper()
	store, operation, job := newCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	binding := operation.Request().Binding.Supervisor
	group, groupErr := inspection.OpenGroupContext(t.Context(), store, binding)
	if groupErr != nil {
		t.Fatal(groupErr)
	}
	if closeErr := group.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	duplicate := job
	duplicate.ID = 8
	jobs := []pueue.Job{job, duplicate}
	if !matchingFirst {
		jobs[0], jobs[1] = jobs[1], jobs[0]
	}
	migrationErr := authorizePrivateMigrationQueue(t.Context(), store.Root, binding, jobs)
	if !errors.Is(migrationErr, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("migration accepted duplicate inspection labels: err=%v", migrationErr)
	}
	result, succeeded, resultErr := operation.ReadWorkerResultContext(t.Context())
	if !errors.Is(resultErr, os.ErrNotExist) {
		t.Fatalf("ambiguous migration published worker observation before rejecting the queue: result=%+v succeeded=%t err=%v", result, succeeded, resultErr)
	}
}

func newCleanupInspectionTestOperation(t *testing.T, reason inspection.ResultReason, succeeded bool) (*taskdir.Store, *inspection.Operation, pueue.Job) {
	t.Helper()
	return admissionfixture.NewCleanupInspectionTestOperation(t, reason, succeeded)
}

func newCleanupInspectionTestOperationInQueue(t *testing.T, reason inspection.ResultReason, succeeded, concurrent bool) (*taskdir.Store, *inspection.Operation, pueue.Job) {
	t.Helper()
	return newCleanupInspectionTestOperationWithOwnership(t, reason, succeeded, concurrent, "")
}

func newCleanupInspectionTestOperationWithOwnership(t *testing.T, reason inspection.ResultReason, succeeded, concurrent bool, ownership string) (*taskdir.Store, *inspection.Operation, pueue.Job) {
	t.Helper()
	return admissionfixture.NewCleanupInspectionTestOperationWithOwnership(t, reason, succeeded, concurrent, ownership)
}

func TestPrivateDefaultQueueInspectionCleanupAndMigrationNeedNoLegacyGroup(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		name := "cleanup"
		if migrate {
			name = "migration"
		}
		t.Run(name, func(t *testing.T) {
			assertDefaultQueueCleanupAndMigration(t, migrate)
		})
	}
}

func TestPrivateCleanupReportsGuardianTerminalRunningRowAsSettling(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, _, _ := newTerminalInspectionCleanupSupervisor(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordWorkerSuccessContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	job.State = pueue.StateRunning
	job.Succeeded = false

	plan, err := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, operation.Request().Binding.Supervisor, []pueue.Job{job})
	if err != nil || len(plan.RemoveIDs) != 0 || !plan.Settling {
		t.Fatalf("guardian-terminal running row cleanup plan=%+v err=%v", plan, err)
	}
}

func newTerminalInspectionCleanupSupervisor(t *testing.T, store *taskdir.Store) (*pueue.Client, string, string) {
	t.Helper()
	return admissionfixture.NewTerminalInspectionCleanupSupervisor(t, store, admissioncore.TerminalEvidence{})
}

func newTerminalInspectionCleanupOperation(t *testing.T, store *taskdir.Store, supervisor *pueue.Client, reason inspection.ResultReason, succeeded bool) (*inspection.Operation, pueue.Job) {
	t.Helper()
	return admissionfixture.NewTerminalInspectionCleanupOperation(t, store, supervisor, reason, succeeded)
}

func assertDefaultQueueCleanupAndMigration(t *testing.T, migrate bool) {
	t.Helper()
	store, operation, job := newCleanupInspectionTestOperationInQueue(t, inspection.ResultEligible, true, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if job.Group != "default" {
		t.Fatalf("fixture used group %q", job.Group)
	}
	if _, _, err := inspection.ReadGroupHead(t.Context(), store); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture unexpectedly has a legacy group journal: %v", err)
	}
	binding := operation.Request().Binding.Supervisor
	if err := preparePrivateTerminalEvidence(t.Context(), store.Root, binding, []pueue.Job{job}); err != nil {
		t.Fatal(err)
	}
	if migrate {
		if err := authorizePrivateMigrationQueue(t.Context(), store.Root, binding, []pueue.Job{job}); err != nil {
			t.Fatalf("default-only inspection blocked migration: %v", err)
		}
	} else {
		plan, err := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, binding, []pueue.Job{job})
		if err != nil || len(plan.RemoveIDs) != 1 || plan.RemoveIDs[0] != job.ID || plan.Settling {
			t.Fatalf("default inspection was not selected: plan=%+v err=%v", plan, err)
		}
	}
	assertCleanupInspectionTestProof(t, operation, true)
	facts, err := admissioncore.AwaitInspection(t.Context(), operation, nil)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("admission lost removed default worker evidence: facts=%s err=%v", facts, err)
	}
}

func assertCleanupInspectionTestProof(t *testing.T, operation *inspection.Operation, succeeded bool) {
	t.Helper()
	admissionfixture.AssertCleanupInspectionTestProof(t, operation, succeeded)
}
