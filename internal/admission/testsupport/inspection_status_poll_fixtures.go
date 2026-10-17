package testsupport

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func NewInspectionPollTestOperation(t *testing.T, store *taskdir.Store, client *pueue.Client) *inspection.Operation {
	return NewInspectionPollTestOperationAt(t, store, client, time.Now().UTC())
}

func NewInspectionPollTestOperationAt(t *testing.T, store *taskdir.Store, client *pueue.Client, createdAt time.Time) *inspection.Operation {
	return NewInspectionPollTestOperationAtWithOwnership(t, store, client, createdAt, "")
}

func NewInspectionPollTestOperationAtWithOwnership(t *testing.T, store *taskdir.Store, client *pueue.Client, createdAt time.Time, ownership string) *inspection.Operation {
	t.Helper()
	digest := strings.Repeat("a", 64)
	workerExitProof := ""
	if ownership == task.RunnerOwnershipManaged {
		workerExitProof = inspection.WorkerExitProofGuardianWaitV1
	}
	operation, openErr := inspection.OpenOperation(store, supervisorfixture.NewInspectionRequest(t, store), inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: "/usr/bin/true", WorkerSHA256: digest,
		RunnerOwnership: ownership, WorkerExitProof: workerExitProof,
		Supervisor: client.Binding(),
	}, createdAt)
	if openErr != nil {
		t.Fatal(openErr)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return operation
}

func CompleteInspectionPollTestLifetime(t *testing.T, operation *inspection.Operation, cause string) {
	t.Helper()
	const bootID = "inspection-poll-boot"
	_, err := operation.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "inspection-poll-process-group",
		BootID:    bootID,
		Deadline:  operation.Deadline(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = operation.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "inspection-poll-process-group",
		BootID:    bootID,
		Deadline:  operation.Deadline(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if operation.Request().Binding.WorkerExitProof == inspection.WorkerExitProofGuardianWaitV1 {
		worker, workerErr := operation.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{
			PID: 307, ProcessStart: "inspection-poll-worker", BootID: bootID,
		})
		if workerErr != nil {
			t.Fatal(workerErr)
		}
		workerBytes, marshalErr := task.MarshalCanonical(worker)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		result, _, resultErr := operation.ReadCompletedContext(t.Context())
		if resultErr != nil {
			t.Fatal(resultErr)
		}
		waitResult := task.LifetimeWorkerWaitSuccess
		exitCode := 0
		if result.Reason != inspection.ResultEligible {
			waitResult = task.LifetimeWorkerWaitExitError
			exitCode = 1
		}
		if _, waitErr := operation.RecordLifetimeWorkerWait(taskdir.LifetimeWorkerWaitFacts{
			WorkerSHA256: task.ComputeSHA256(workerBytes),
			ChildPID:     worker.PID,
			Result:       waitResult,
			ExitCode:     &exitCode,
			ObservedAt:   time.Now().UTC(),
		}); waitErr != nil {
			t.Fatal(waitErr)
		}
	}
	if _, err = operation.RecordLifetimeTermination(cause); err != nil {
		t.Fatal(err)
	}
}

func WriteInspectionPollTestStatus(t *testing.T, path string, operation *inspection.Operation, failed bool) {
	t.Helper()
	state := map[string]any{"Running": map[string]any{"enqueued_at": InspectionPollStamp, "start": InspectionPollStamp}}
	if failed {
		state = map[string]any{"Done": map[string]any{"enqueued_at": InspectionPollStamp, "start": InspectionPollStamp, "end": InspectionPollStamp, "result": "Killed"}}
	}
	WriteInspectionPollTestState(t, path, operation, state)
}

const InspectionPollStamp = "2026-10-09T00:00:00Z"

func WriteInspectionPollTestState(t *testing.T, path string, operation *inspection.Operation, state map[string]any) {
	t.Helper()
	identity := operation.Request().SupervisorIdentity()
	data, marshalErr := json.Marshal(map[string]any{
		"tasks": map[string]any{"7": map[string]any{
			"id": 7, "created_at": InspectionPollStamp, "original_command": "inspection-worker", "command": "inspection-worker",
			"path": "/tmp", "envs": map[string]string{}, "group": identity.Group(),
			"dependencies": []int64{}, "priority": 0, "label": identity.Label(), "status": state,
		}},
		"groups": map[string]any{identity.Group(): map[string]any{"status": "Running", "parallel_tasks": 1}},
	})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
}
