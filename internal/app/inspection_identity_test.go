package app

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

type admissionInspectionTestFixture struct {
	store    *taskdir.Store
	resource *workspace.Resource
	request  task.TaskRecord
	subject  workspace.Subject
	binding  inspection.Binding
}

func newAdmissionInspectionTestFixture(t *testing.T) admissionInspectionTestFixture {
	t.Helper()
	store, resource := newAppWorkspaceResource(t)
	owner, err := resource.AcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	request := newBareInspectionRequest(t, store)
	request.TaskID = resource.Request().TaskID
	digest := task.ComputeSHA256([]byte("attempt worker"))
	return admissionInspectionTestFixture{
		store: store, resource: resource, request: request,
		subject: workspace.Subject{Kind: workspace.SubjectAdmission, ID: request.TaskID, SHA256: resource.Request().RequestSHA256},
		binding: inspection.Binding{
			DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
			HelperExecutable: "/fixture/helper", HelperSHA256: digest, WorkerExecutable: "/fixture/worker", WorkerSHA256: digest,
			RunnerOwnership: task.RunnerOwnershipManaged, WorkerExitProof: inspection.WorkerExitProofGuardianWaitV1,
			Supervisor: continuationSupervisorRef(store.Root, digest, "unix:/fixture/pueue.sock"),
		},
	}
}

func openTestAdmissionAttempt(t *testing.T, fixture admissionInspectionTestFixture, reservation workspace.Reservation, created time.Time) (*inspection.Operation, task.TaskRecord) {
	t.Helper()
	request := fixture.request
	request.Workspace = &task.WorkspaceBinding{
		ID: fixture.resource.Request().WorkspaceID, Reservation: reservation.Sequence,
		RequestSHA256: fixture.subject.SHA256, BudgetNanos: int64(time.Minute),
	}
	operation, err := createAdmissionInspectionOperationContext(t.Context(), fixture.store, request, fixture.binding, created)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return operation, request
}

func TestWorkspaceAdmissionRetryResolvesFreshInspectionWithoutRevivingOldWorker(t *testing.T) {
	fixture := newAdmissionInspectionTestFixture(t)
	first, err := fixture.resource.Reserve(t.Context(), fixture.subject)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-2 * inspection.AdmissionTimeout)
	old, _ := openTestAdmissionAttempt(t, fixture, first, created)
	if err = workspaceAdmissionInspectionClosed(t.Context(), fixture.store, fixture.resource, first); err != nil {
		t.Fatal(err)
	}
	if err = fixture.resource.RecordAdmissionAborted(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	next, err := fixture.resource.Reserve(t.Context(), fixture.subject)
	if err != nil {
		t.Fatal(err)
	}
	current, request := openTestAdmissionAttempt(t, fixture, next, time.Now())
	if current.Request().IdentityID() == old.Request().IdentityID() || current.Expired(time.Now()) || !old.Expired(time.Now()) {
		t.Fatal("new admission reused the closed inspection or renewed its deadline")
	}
	assertAdmissionAttemptConsumers(t, fixture.store, current, request)
	if _, err = old.ClaimStartContext(t.Context(), created.Add(time.Millisecond)); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("new admission revived old worker launch authority: %v", err)
	}
	if _, err = fixture.store.OpenTask(request.TaskID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection recovery fabricated an ordinary task: %v", err)
	}
}

func assertAdmissionAttemptConsumers(t *testing.T, store *taskdir.Store, operation *inspection.Operation, request task.TaskRecord) {
	t.Helper()
	loaded, err := loadAdmissionInspectionOperationContext(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Digest() != operation.Digest() || loaded.Request().TaskID != request.TaskID {
		t.Fatal("logical-task consumer opened another inspection attempt")
	}
	if err = loaded.Close(); err != nil {
		t.Fatal(err)
	}
	worker, err := loadInspectionOperationForWorker(store, operation.Request().IdentityID())
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceProvisionReuseRequiresOriginalHelperClosure(t *testing.T) {
	fixture := newWorkspaceRecoveryFixture(t)
	subject := workspace.Subject{Kind: workspace.SubjectAdmission, ID: fixture.resource.Request().TaskID, SHA256: fixture.resource.Request().RequestSHA256}
	operation := openRecoveryOperation(t, fixture, strings.Repeat("a", 32), workspace.OperationProvision, subject)
	t.Cleanup(func() {
		if err := operation.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := requireWorkspaceProvisionHelpersClosed(t.Context(), fixture.store, fixture.resource); !errors.Is(err, workspace.ErrPending) {
		t.Fatalf("reused checkout bypassed unfinished provision helper: %v", err)
	}
	seedRealWorkspaceOperationClosure(t, fixture.store, operation)
	if err := requireWorkspaceProvisionHelpersClosed(t.Context(), fixture.store, fixture.resource); err != nil {
		t.Fatalf("closed provision helper prevented retained checkout reuse: %v", err)
	}
}
