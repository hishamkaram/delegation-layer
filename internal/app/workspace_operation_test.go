package app

import (
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestExpiredWorkspaceOperationSealsUnlaunchedWithoutNativeEvidence(t *testing.T) {
	store, operation := workspacefixture.NewOperationDeadline(t, time.Now().Add(-time.Second))
	requestBefore, err := task.MarshalCanonical(operation.Request())
	if err != nil {
		t.Fatal(err)
	}
	permit, err := operation.SubmissionPermit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	proof, err := workspaces.OperationClosed(t.Context(), operation)
	if err != nil || proof.UnlaunchedSHA256() == "" || proof.TerminationSHA256() != "" || proof.WorkerWaitSHA256() != "" {
		t.Fatalf("unlaunched operation received synthetic native evidence: %+v, %v", proof, err)
	}
	if err = operation.RecordReceipt(t.Context(), 1); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("late receipt bypassed launch fence: %v", err)
	}
	if _, err = operation.SubmissionPermit(t.Context()); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("closed operation granted another submission: %v", err)
	}
	if _, _, err = workspaces.AcceptedOperation(t.Context(), store, operation); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("unlaunched operation became successful worker output: %v", err)
	}
	requestAfter, err := task.MarshalCanonical(operation.Request())
	if err != nil || string(requestBefore) != string(requestAfter) {
		t.Fatalf("fencing renewed operation request/deadline: %v", err)
	}
}

func TestWorkspaceCleanupAuthorizationDoesNotSealExpiredOperation(t *testing.T) {
	_, operation := workspacefixture.NewOperationDeadline(t, time.Now().Add(-time.Second))
	if err := workspaces.ReconcileEndedWorkspaceOperation(t.Context(), operation, pueue.Job{}); !errors.Is(err, workspace.ErrPending) {
		t.Fatalf("cleanup inspection created its own closure authority: %v", err)
	}
	if _, err := operation.ReadLifetimeUnlaunchedContext(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("cleanup inspection changed the operation journal: %v", err)
	}
	proof, err := workspaces.OperationClosed(t.Context(), operation)
	if err != nil || proof.UnlaunchedSHA256() == "" {
		t.Fatalf("coordinator could not seal the same expired operation: %+v, %v", proof, err)
	}
	if err = workspaces.ReconcileEndedWorkspaceOperation(t.Context(), operation, pueue.Job{}); err != nil {
		t.Fatalf("cleanup did not accept an already recorded no-launch proof: %v", err)
	}
}

func TestWorkspaceReceiptReplayAfterFenceDoesNotAuthorizeNewWork(t *testing.T) {
	_, operation := workspacefixture.NewOperationDeadline(t, time.Now().Add(-time.Second))
	if err := operation.RecordReceipt(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.OperationClosed(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordReceipt(t.Context(), 7); err != nil {
		t.Fatalf("exact receipt readback was rejected after closure: %v", err)
	}
	if err := operation.RecordReceipt(t.Context(), 8); err == nil {
		t.Fatal("closed operation changed its numeric receipt")
	}
	if _, err := operation.SubmissionPermit(t.Context()); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("receipt replay authorized a new submission: %v", err)
	}
	receipt, err := operation.Receipt(t.Context())
	if err != nil || receipt.NumericTaskID != 7 {
		t.Fatalf("receipt replay replaced the immutable receipt: %+v %v", receipt, err)
	}
}

func TestWorkspaceGuardianRequiresExactWorkerWaitBeforeAcceptance(t *testing.T) {
	store, operation := workspacefixture.NewOperation(t)
	fixture := workspacefixture.GuardianWithRealRecords(t, store, operation)
	fixture.Hello(t)
	fixture.RunCommand(t)
	fixture.CloseTask(t)
	if _, err := workspaces.OperationClosed(t.Context(), operation); !errors.Is(err, workspace.ErrPending) {
		t.Fatalf("accepted scope closure before managed worker Wait: %v", err)
	}
	_, permitErr := operation.SubmissionPermit(t.Context())
	if !errors.Is(permitErr, task.ErrTerminalTask) || !errors.Is(workspaces.LaunchPermissionError(permitErr), workspace.ErrPending) {
		t.Fatalf("scope-close scheduling race was not pending: %v", permitErr)
	}
	receiptErr := operation.RecordReceipt(t.Context(), 1)
	if !errors.Is(receiptErr, task.ErrTerminalTask) || !errors.Is(workspaces.LaunchPermissionError(receiptErr), workspace.ErrPending) {
		t.Fatalf("scope-close receipt race was not pending: %v", receiptErr)
	}
	fixture.ReleaseWorker()
	if err := fixture.WaitRun(t); err != nil {
		t.Fatal(err)
	}
	proof, err := workspaces.OperationClosed(t.Context(), operation)
	if err != nil {
		t.Fatal(err)
	}
	if proof.TerminationSHA256() == "" || proof.WorkerWaitSHA256() == "" {
		t.Fatal("closure did not bind both canonical proofs")
	}
	registration, err := operation.ReadLifetimeRegistrationContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if registration.OwnerKind != task.LifetimeOwnerWorkspace || registration.OwnerSHA256 != operation.Digest() {
		t.Fatalf("guardian used wrong state owner: %+v", registration)
	}
}

func TestWorkspaceOperationResultCannotGrantLifetimeClosure(t *testing.T) {
	store, operation := workspacefixture.NewOperation(t)
	artifacts := []workspace.Artifact{{Name: "diff.patch", SHA256: task.ComputeSHA256(nil)}, {Name: "name-status.z", SHA256: task.ComputeSHA256(nil)}}
	if err := operation.RecordResult(t.Context(), workspace.OperationResult{SchemaVersion: task.SchemaVersion, OperationID: operation.Request().OperationID, RequestSHA256: operation.Digest(), Status: "succeeded", Artifacts: artifacts}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workspaces.AcceptedOperation(t.Context(), store, operation); !errors.Is(err, workspace.ErrPending) {
		t.Fatalf("worker candidate replaced guardian authority: %v", err)
	}
}

func TestWorkspaceOperationGuardNeverGrantsASecondSubmission(t *testing.T) {
	_, operation := workspacefixture.NewOperation(t)
	permit, err := operation.SubmissionPermit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	if err = permit.ConsumeInspectionSubmission(); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("permit consumed twice: %v", err)
	}
	if _, err = operation.SubmissionPermit(t.Context()); err == nil {
		t.Fatal("existing guard granted another submit")
	}
}
