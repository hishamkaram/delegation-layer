package app

import (
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/workspaces"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestWorkspaceVerificationRecoversObligationAfterReservationCrash(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	request, subject, deps := workspaceVerificationRecoveryAuthorization(t, fixture)
	reservation, err := fixture.Resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	fixture.Restart(t)
	workspacefixture.RecoveryUseOwner(t, fixture)
	if err = workspaces.ReconcileUse(t.Context(), fixture.Store, fixture.Resource, workspaceRuntimeOptions(deps)); !errors.Is(err, workspace.ErrPending) {
		t.Fatalf("interrupted verification recovery = %v, want pending original operation", err)
	}
	obligation, err := fixture.Resource.CaptureObligation(t.Context(), reservation.Sequence)
	if err != nil || obligation.Predecessor == nil || *obligation.Predecessor != subject || obligation.BudgetNanos != fixture.Resource.Request().BudgetNanos {
		t.Fatalf("recovered verification obligation = %+v, error = %v", obligation, err)
	}
	loaded, err := workspace.LoadOperation(t.Context(), fixture.Store, request.OperationID)
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

func workspaceVerificationRecoveryAuthorization(t *testing.T, fixture *workspacefixture.RecoveryFixture) (workspace.OperationRequest, workspace.Subject, Dependencies) {
	t.Helper()
	supervisor, _ := newTerminalEventInspectionSupervisor(t, fixture.Store)
	request := workspacefixture.RecoveryOperationRequest(t, fixture, strings.Repeat("d", 32), workspace.OperationVerify,
		workspace.Subject{Kind: workspace.SubjectAdmission, ID: strings.Repeat("e", 32), SHA256: task.ComputeSHA256([]byte("verification request"))})
	request.Arguments = []string{"/fixture/verify"}
	request.Supervisor = supervisor.Binding()
	request.WorkerExecutable = executablePath(t)
	request.WorkerSHA256 = ""
	request.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	operation, err := workspace.OpenOperation(t.Context(), fixture.Store, request)
	if err != nil {
		t.Fatal(err)
	}
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: request.OperationID, SHA256: operation.Digest()}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{ResourceChecker: fixtureResources, InitialSupervisorExecutable: supervisor.Binding().ClientExecutable, SupervisorOptions: pueue.Options{}, Environment: os.Environ()}
	return request, subject, deps
}

func TestWorkspaceVerificationRecoveryRejectsAnotherResourceOrReservation(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "mismatched-reservation", true: "foreign-workspace"}[foreign], func(t *testing.T) { verifyWorkspaceVerificationRecoveryBinding(t, foreign) })
	}
}

func verifyWorkspaceVerificationRecoveryBinding(t *testing.T, foreign bool) {
	t.Helper()
	fixture := workspacefixture.NewRecoveryFixture(t)
	request := workspacefixture.RecoveryOperationRequest(t, fixture, strings.Repeat("d", 32), workspace.OperationVerify,
		workspace.Subject{Kind: workspace.SubjectAdmission, ID: strings.Repeat("e", 32), SHA256: task.ComputeSHA256([]byte("verification request"))})
	request.Arguments = []string{"/fixture/verify"}
	if foreign {
		request.WorkspaceID = strings.Repeat("f", 32)
	}
	operation, err := workspace.OpenOperation(t.Context(), fixture.Store, request)
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
	reservation, err := fixture.Resource.Reserve(t.Context(), reserved)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.Resource.RecordCaptureObligation(t.Context(), reservation, subject); err != nil {
		t.Fatal(err)
	}
	fixture.Restart(t)
	workspacefixture.RecoveryUseOwner(t, fixture)
	var commands atomic.Int32
	deps := Dependencies{ResourceChecker: fixtureResources, SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { commands.Add(1) }}}
	err = workspaces.ReconcileUse(t.Context(), fixture.Store, fixture.Resource, workspaceRuntimeOptions(deps))
	if (!errors.Is(err, task.ErrEvidenceFault) && !errors.Is(err, task.ErrIdentityMismatch)) || commands.Load() != 0 {
		t.Fatalf("invalid verification binding reached scheduling: commands=%d err=%v", commands.Load(), err)
	}
}

func TestWorkspaceObligationRecoveryRejectsUnknownAuthorization(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: strings.Repeat("e", 32), SHA256: task.ComputeSHA256([]byte("unknown authorization"))}
	reservation, err := fixture.Resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	fixture.Restart(t)
	workspacefixture.RecoveryUseOwner(t, fixture)
	if err = workspaces.ReconcileUse(t.Context(), fixture.Store, fixture.Resource, workspaceRuntimeOptions(Dependencies{ResourceChecker: fixtureResources})); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unknown authorization = %v, want evidence fault", err)
	}
	if _, err = fixture.Resource.CaptureObligation(t.Context(), reservation.Sequence); !workspace.OperationAbsent(err) {
		t.Fatalf("unknown authorization created an obligation: %v", err)
	}
}
