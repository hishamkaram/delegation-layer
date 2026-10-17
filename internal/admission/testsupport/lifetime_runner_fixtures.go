package testsupport

import (
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func RecordManagedInspectionLifetimeExit(t *testing.T, operation *inspection.Operation, result task.LifetimeWorkerWaitResult, observedAt time.Time) {
	RecordManagedInspectionLifetimeExitWithCause(t, operation, result, observedAt, task.LifetimeTerminationCompleted)
}

func RecordManagedInspectionLifetimeExitWithCause(t *testing.T, operation *inspection.Operation, result task.LifetimeWorkerWaitResult, observedAt time.Time, cause string) {
	t.Helper()
	deadline := operation.Deadline()
	_, err := operation.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "pg:test-managed-inspection",
		BootID:    "test-boot",
		Deadline:  deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = operation.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "pg:test-managed-inspection",
		BootID:    "test-boot",
		Deadline:  deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := operation.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{
		PID: 303, ProcessStart: "worker-start", BootID: "test-boot",
	})
	if err != nil {
		t.Fatal(err)
	}
	workerBytes, err := task.MarshalCanonical(worker)
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	if result != task.LifetimeWorkerWaitSuccess {
		exitCode = 1
	}
	_, err = operation.RecordLifetimeWorkerWait(taskdir.LifetimeWorkerWaitFacts{
		WorkerSHA256: task.ComputeSHA256(workerBytes),
		ChildPID:     worker.PID,
		Result:       result,
		ExitCode:     &exitCode,
		ObservedAt:   observedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = operation.RecordLifetimeTermination(cause); err != nil {
		t.Fatal(err)
	}
}

func RecordManagedInspectionNotStarted(t *testing.T, operation *inspection.Operation, cause string, observedAt time.Time) {
	t.Helper()
	deadline := operation.Deadline()
	_, err := operation.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "pg:test-managed-inspection-not-started",
		BootID:    "test-boot",
		Deadline:  deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = operation.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "pg:test-managed-inspection-not-started",
		BootID:    "test-boot",
		Deadline:  deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = operation.RecordLifetimeTermination(cause); err != nil {
		t.Fatal(err)
	}
	if _, err = operation.RecordLifetimeWorkerWait(taskdir.LifetimeWorkerWaitFacts{
		Result: task.LifetimeWorkerWaitNotStarted, ObservedAt: observedAt,
	}); err != nil {
		t.Fatal(err)
	}
}

func RecordManagedExactInspectionUnqualifiedExit(t *testing.T, operation *inspection.Operation, result task.LifetimeWorkerWaitResult, cause string, observedAt time.Time) {
	t.Helper()
	deadline := operation.Deadline()
	_, err := operation.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeCgroupV2,
		Assurance: task.LifetimeAssuranceExact,
		ScopePlan: "cgroup:test-managed-inspection-unqualified",
		BootID:    "test-boot",
		Deadline:  deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = operation.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeCgroupV2,
		Assurance: task.LifetimeAssuranceExact,
		ScopeID:   "cgroup:test-managed-inspection-unqualified",
		BootID:    "test-boot",
		Deadline:  deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 1
	if result == task.LifetimeWorkerWaitSuccess {
		exitCode = 0
	}
	if _, err = operation.RecordLifetimeWorkerWait(taskdir.LifetimeWorkerWaitFacts{
		ChildPID: 303, Result: result, ExitCode: &exitCode, ObservedAt: observedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = operation.RecordLifetimeTermination(cause); err != nil {
		t.Fatal(err)
	}
}
