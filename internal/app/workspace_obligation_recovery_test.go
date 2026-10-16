package app

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestWorkspaceVerificationRecoversObligationAfterReservationCrash(t *testing.T) {
	fixture := newWorkspaceRecoveryFixture(t)
	request, subject, deps := workspaceVerificationRecoveryAuthorization(t, fixture)
	reservation, err := fixture.resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	fixture.restart(t)
	workspaceRecoveryUseOwner(t, fixture)
	if err = reconcileWorkspaceUse(t.Context(), fixture.store, fixture.resource, deps); !errors.Is(err, workspace.ErrPending) {
		t.Fatalf("interrupted verification recovery = %v, want pending original operation", err)
	}
	obligation, err := fixture.resource.CaptureObligation(t.Context(), reservation.Sequence)
	if err != nil || obligation.Predecessor == nil || *obligation.Predecessor != subject || obligation.BudgetNanos != fixture.resource.Request().BudgetNanos {
		t.Fatalf("recovered verification obligation = %+v, error = %v", obligation, err)
	}
	loaded, err := workspace.LoadOperation(t.Context(), fixture.store, request.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := loaded.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if loaded.Request().Deadline != request.Deadline || loaded.Digest() != subject.SHA256 {
		t.Fatal("recovery replaced the original verification authorization")
	}
	if receipt, receiptErr := loaded.Receipt(t.Context()); receiptErr != nil || receipt.NumericTaskID != 9 {
		t.Fatalf("recovery did not submit the exact authorized verification: %+v, %v", receipt, receiptErr)
	}
}

func workspaceVerificationRecoveryAuthorization(t *testing.T, fixture *workspaceRecoveryFixture) (workspace.OperationRequest, workspace.Subject, Dependencies) {
	t.Helper()
	supervisor, _ := newTerminalEventInspectionSupervisor(t, fixture.store)
	request := recoveryOperationRequest(t, fixture, strings.Repeat("d", 32), workspace.OperationVerify,
		workspace.Subject{Kind: workspace.SubjectAdmission, ID: strings.Repeat("e", 32), SHA256: task.ComputeSHA256([]byte("verification request"))})
	request.Arguments = []string{"/fixture/verify"}
	request.Supervisor = supervisor.Binding()
	request.WorkerExecutable = executablePath(t)
	digest, err := commonprovider.FingerprintExecutable(request.WorkerExecutable)
	if err != nil {
		t.Fatal(err)
	}
	request.WorkerSHA256 = digest
	operation, err := workspace.OpenOperation(t.Context(), fixture.store, request)
	if err != nil {
		t.Fatal(err)
	}
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: request.OperationID, SHA256: operation.Digest()}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{InitialSupervisorExecutable: supervisor.Binding().ClientExecutable, SupervisorOptions: pueue.Options{}}
	return request, subject, deps
}

func TestWorkspaceVerificationRecoveryRejectsAnotherResourceOrReservation(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "mismatched-reservation", true: "foreign-workspace"}[foreign], func(t *testing.T) { verifyWorkspaceVerificationRecoveryBinding(t, foreign) })
	}
}

func verifyWorkspaceVerificationRecoveryBinding(t *testing.T, foreign bool) {
	t.Helper()
	fixture := newWorkspaceRecoveryFixture(t)
	request := recoveryOperationRequest(t, fixture, strings.Repeat("d", 32), workspace.OperationVerify,
		workspace.Subject{Kind: workspace.SubjectAdmission, ID: strings.Repeat("e", 32), SHA256: task.ComputeSHA256([]byte("verification request"))})
	request.Arguments = []string{"/fixture/verify"}
	if foreign {
		request.WorkspaceID = strings.Repeat("f", 32)
	}
	operation, err := workspace.OpenOperation(t.Context(), fixture.store, request)
	if err != nil {
		t.Fatal(err)
	}
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: request.OperationID, SHA256: operation.Digest()}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	reserved := subject
	if !foreign {
		reserved.ID = strings.Repeat("e", 32)
	}
	reservation, err := fixture.resource.Reserve(t.Context(), reserved)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.resource.RecordCaptureObligation(t.Context(), reservation, subject); err != nil {
		t.Fatal(err)
	}
	fixture.restart(t)
	workspaceRecoveryUseOwner(t, fixture)
	var commands atomic.Int32
	deps := Dependencies{SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { commands.Add(1) }}}
	err = reconcileWorkspaceUse(t.Context(), fixture.store, fixture.resource, deps)
	if (!errors.Is(err, task.ErrEvidenceFault) && !errors.Is(err, task.ErrIdentityMismatch)) || commands.Load() != 0 {
		t.Fatalf("invalid verification binding reached scheduling: commands=%d err=%v", commands.Load(), err)
	}
}

func TestWorkspaceReleaseRecoversObligationAfterReservationCrash(t *testing.T) {
	for _, discard := range []bool{false, true} {
		t.Run(map[bool]string{false: "retain-on-capture-failure", true: "discard-authorized"}[discard], func(t *testing.T) {
			fixture := newWorkspaceRecoveryFixture(t)
			action := workspace.ReleaseAction{
				SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.resource.Request().WorkspaceID,
				ID: strings.Repeat("e", 32), Discard: discard, BudgetNanos: int64(37 * time.Second),
			}
			subject, err := fixture.resource.RecordReleaseAction(t.Context(), action)
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := fixture.resource.Reserve(t.Context(), subject)
			if err != nil {
				t.Fatal(err)
			}
			fixture.restart(t)
			workspaceRecoveryUseOwner(t, fixture)
			if err = restoreWorkspaceCaptureObligation(t.Context(), fixture.store, fixture.resource, reservation); err != nil {
				t.Fatal(err)
			}
			obligation, err := fixture.resource.CaptureObligation(t.Context(), reservation.Sequence)
			if err != nil || obligation.Subject != subject || obligation.Predecessor != nil || obligation.BudgetNanos != action.BudgetNanos {
				t.Fatalf("recovered release obligation = %+v, error = %v", obligation, err)
			}
			if err = fixture.resource.ReleaseCaptureAuthorized(t.Context(), obligation); err != nil {
				t.Fatalf("recovered release lost original authorization: %v", err)
			}
		})
	}
}

func TestWorkspaceObligationRecoveryRejectsUnknownAuthorization(t *testing.T) {
	fixture := newWorkspaceRecoveryFixture(t)
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: strings.Repeat("e", 32), SHA256: task.ComputeSHA256([]byte("unknown authorization"))}
	reservation, err := fixture.resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	fixture.restart(t)
	workspaceRecoveryUseOwner(t, fixture)
	if err = reconcileWorkspaceUse(t.Context(), fixture.store, fixture.resource, Dependencies{}); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unknown authorization = %v, want evidence fault", err)
	}
	if _, err = fixture.resource.CaptureObligation(t.Context(), reservation.Sequence); !workspace.OperationAbsent(err) {
		t.Fatalf("unknown authorization created an obligation: %v", err)
	}
}

func workspaceRecoveryUseOwner(t *testing.T, fixture *workspaceRecoveryFixture) {
	t.Helper()
	owner, err := fixture.resource.TryAcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
}
