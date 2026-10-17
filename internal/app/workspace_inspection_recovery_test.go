package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestWorkspaceMissingInspectionRecoveryRetainsAdmission(t *testing.T) {
	fixture, reservation := newWorkspaceInspectionReservation(t)
	operation := newWorkspaceAdmissionRecoveryOperation(t, fixture, reservation)
	ctx := t.Context()
	if err := workspacescore.ReconcileUse(ctx, fixture.Store, fixture.Resource, workspacescore.OperationOptions{}); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("missing recovery port = %v, want evidence fault", err)
	}
	if _, err := fixture.Resource.ReservationClosed(ctx, reservation.Sequence); !workspace.OperationAbsent(err) {
		t.Fatalf("missing recovery port committed an aborted reservation: %v", err)
	}
	active, err := fixture.Resource.Reservation(ctx)
	if err != nil || active != reservation {
		t.Fatalf("missing recovery port changed the active reservation: %+v, %v", active, err)
	}
	if _, err = operation.ReadLifetimeUnlaunchedContext(ctx); !task.RecordAbsent(err) {
		t.Fatalf("missing recovery port fabricated unlaunched authority: %v", err)
	}

	// Enter through actual app composition: admission must fence this expired
	// inspection before the workspace coordinator can commit its abort.
	if err = reconcileWorkspaceUse(ctx, fixture.Store, fixture.Resource, Dependencies{}); err != nil {
		t.Fatal(err)
	}
	if _, err = operation.ReadLifetimeUnlaunchedContext(ctx); err != nil {
		t.Fatalf("app-composed recovery did not publish the inspection fence: %v", err)
	}
	closed, err := fixture.Resource.ReservationClosed(ctx, reservation.Sequence)
	if err != nil || closed.Disposition != "aborted-not-launched" {
		t.Fatalf("app-composed recovery did not commit the abort: %+v, %v", closed, err)
	}
	if _, err = operation.ReadLifetimeTerminationContext(ctx); !task.RecordAbsent(err) {
		t.Fatalf("unlaunched inspection acquired false native termination: %v", err)
	}
}

func TestWorkspaceAbsentInspectionDoesNotRequireRecoveryPort(t *testing.T) {
	fixture, reservation := newWorkspaceInspectionReservation(t)
	if err := workspacescore.ReconcileUnlaunchedAdmission(t.Context(), fixture.Store, fixture.Resource, reservation, nil); err != nil {
		t.Fatal(err)
	}
	closed, err := fixture.Resource.ReservationClosed(t.Context(), reservation.Sequence)
	if err != nil || closed.Disposition != "aborted-not-launched" {
		t.Fatalf("absent inspection changed admission abort semantics: %+v, %v", closed, err)
	}
}

func newWorkspaceInspectionReservation(t *testing.T) (*workspacefixture.RecoveryFixture, workspace.Reservation) {
	t.Helper()
	fixture := workspacefixture.NewRecoveryFixture(t)
	workspacefixture.RecoveryUseOwner(t, fixture)
	request := fixture.Resource.Request()
	reservation, err := fixture.Resource.Reserve(t.Context(), workspace.Subject{
		Kind: workspace.SubjectAdmission, ID: request.TaskID, SHA256: request.RequestSHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture, reservation
}

func newWorkspaceAdmissionRecoveryOperation(t *testing.T, fixture *workspacefixture.RecoveryFixture, reservation workspace.Reservation) *inspection.Operation {
	t.Helper()
	attempt, err := fixture.Resource.ResolveAdmissionAttempt(t.Context(), reservation.Sequence, reservation.Subject)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := attempt.InspectionOperationID()
	if err != nil {
		t.Fatal(err)
	}
	request := supervisorfixture.NewInspectionRequest(t, fixture.Store)
	request.TaskID = reservation.Subject.ID
	request.Workspace = &task.WorkspaceBinding{
		ID: fixture.Resource.Request().WorkspaceID, Reservation: reservation.Sequence,
		RequestSHA256: reservation.Subject.SHA256, BudgetNanos: int64(time.Minute),
	}
	digest := strings.Repeat("a", 64)
	binding := inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: "/usr/bin/true", WorkerSHA256: digest,
		RunnerOwnership: task.RunnerOwnershipManaged, WorkerExitProof: inspection.WorkerExitProofGuardianWaitV1,
		Supervisor: supervisorfixture.ContinuationSupervisorRef(fixture.Store.Root, digest, "/private/pueue.sock"),
	}
	created := time.Now().Add(-inspection.AdmissionTimeout - time.Second).UTC()
	operation, err := inspection.OpenOperationWithIdentityContext(t.Context(), fixture.Store, request, binding, identity, created)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return operation
}
