package testsupport

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func NewCleanupInspectionTestOperation(t *testing.T, reason inspection.ResultReason, succeeded bool) (*taskdir.Store, *inspection.Operation, pueue.Job) {
	t.Helper()
	return NewCleanupInspectionTestOperationWithOwnership(t, reason, succeeded, false, "")
}

func NewCleanupInspectionTestOperationWithOwnership(t *testing.T, reason inspection.ResultReason, succeeded, concurrent bool, ownership string) (*taskdir.Store, *inspection.Operation, pueue.Job) {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	digest := strings.Repeat("a", 64)
	supervisor := ContinuationSupervisorRef(store.Root, digest, "unix:"+filepath.Join(store.Root, ".supervisor", "run", "pueue.sock"))
	openedAt := time.Now().UTC()
	binding := inspection.Binding{
		DefinitionRevision: "inspection-v1",
		DefinitionSHA256:   digest,
		HelperExecutable:   "/usr/bin/true",
		HelperSHA256:       digest,
		WorkerExecutable:   "/usr/bin/true",
		WorkerSHA256:       digest,
		RunnerOwnership:    ownership,
		Supervisor:         supervisor,
	}
	if ownership == task.RunnerOwnershipManaged {
		binding.WorkerExitProof = inspection.WorkerExitProofGuardianWaitV1
	}
	openOperation := inspection.OpenOperation
	if concurrent {
		binding.DefinitionRevision = commonprovider.RuntimeInspectionRevision
		binding.CapabilitySHA256 = digest
		openOperation = inspection.OpenConcurrentOperation
	}
	operation, err := openOperation(store, request, binding, openedAt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	submission, err := operation.ClaimSubmission(openedAt.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = submission.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	const workerID = int64(7)
	if err = operation.RecordReceiptContext(t.Context(), workerID); err != nil {
		t.Fatal(err)
	}
	start, err := operation.ClaimStart(openedAt.Add(2 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	var facts []byte
	if reason == inspection.ResultEligible {
		facts = []byte(`{"eligible":true}`)
	}
	if err = operation.Complete(reason, facts, openedAt.Add(3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	label := (pueue.InspectionIdentity{RootID: store.RootID, TaskID: request.TaskID}).Label()
	return store, operation, pueue.Job{
		ID: workerID, Label: &label, Group: operation.Request().Group,
		State: pueue.StateEnded, Succeeded: succeeded,
	}
}

func NewTerminalInspectionCleanupOperation(t *testing.T, store *taskdir.Store, supervisor *pueue.Client, reason inspection.ResultReason, succeeded bool) (*inspection.Operation, pueue.Job) {
	return NewTerminalInspectionCleanupOperationForTask(t, store, supervisor, reason, succeeded, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 7)
}

func NewTerminalInspectionCleanupOperationForTask(t *testing.T, store *taskdir.Store, supervisor *pueue.Client, reason inspection.ResultReason, succeeded bool, taskID string, jobID int64) (*inspection.Operation, pueue.Job) {
	t.Helper()
	request := supervisorfixture.NewInspectionRequest(t, store)
	request.TaskID = taskID
	openedAt := time.Now().UTC()
	digest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: "/usr/bin/true", WorkerSHA256: digest,
		Supervisor: supervisor.Binding(),
	}, openedAt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	submission, err := operation.ClaimSubmission(openedAt.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = submission.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceiptContext(t.Context(), jobID); err != nil {
		t.Fatal(err)
	}
	start, err := operation.ClaimStart(openedAt.Add(2 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	var facts []byte
	if reason == inspection.ResultEligible {
		facts = []byte(`{"eligible":true}`)
	}
	if err = operation.Complete(reason, facts, openedAt.Add(3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	identity := operation.Request().SupervisorIdentity()
	label := identity.Label()
	job := pueue.Job{ID: jobID, Label: &label, Group: identity.Group(), State: pueue.StateEnded, Succeeded: succeeded}
	status, err := json.Marshal(map[string]any{
		"tasks": map[string]any{strconv.FormatInt(jobID, 10): map[string]any{
			"id": jobID, "created_at": "2026-10-10T00:00:00Z", "original_command": "inspection-worker", "command": "inspection-worker",
			"path": "/tmp", "envs": map[string]string{}, "group": job.Group, "dependencies": []int64{}, "priority": 0,
			"label": label, "status": CleanupTestTerminalState(succeeded),
		}},
		"groups": map[string]any{job.Group: map[string]any{"status": "Running", "parallel_tasks": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, ".supervisor", "cleanup-status.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}
	return operation, job
}

func CleanupTestTerminalState(succeeded bool) map[string]any {
	result := "Killed"
	if succeeded {
		result = "Success"
	}
	return map[string]any{"Done": map[string]any{
		"enqueued_at": "2026-10-10T00:00:00Z", "start": "2026-10-10T00:00:00Z", "end": "2026-10-10T00:00:00Z", "result": result,
	}}
}

func AssertCleanupInspectionTestProof(t *testing.T, operation *inspection.Operation, succeeded bool) {
	t.Helper()
	proof, proofErr := operation.ReadProofContext(t.Context())
	if !succeeded {
		if !errors.Is(proofErr, task.ErrEvidenceFault) {
			t.Fatalf("failed worker was promoted to eligible admission proof: proof=%s err=%v", proof, proofErr)
		}
		return
	}
	if proofErr != nil || string(proof) != "{\"eligible\":true}\n" {
		t.Fatalf("successful worker lacks canonical admission proof: proof=%s err=%v", proof, proofErr)
	}
}
