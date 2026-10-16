package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func newAppWorkspaceOperation(t *testing.T) (*taskdir.Store, *workspace.Operation) {
	t.Helper()
	return newAppWorkspaceOperationDeadline(t, time.Now().Add(time.Hour))
}

func newAppWorkspaceOperationDeadline(t *testing.T, deadline time.Time) (*taskdir.Store, *workspace.Operation) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(canonicalAppTestTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	digest := task.ComputeSHA256([]byte("workspace-operation"))
	request := workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID,
		WorkspaceID: strings.Repeat("b", 32), OperationID: lifetimeGuardianTestTaskID,
		Kind: workspace.OperationCapture, Subject: workspace.Subject{Kind: workspace.SubjectTask, ID: strings.Repeat("c", 32), SHA256: digest},
		Deadline: deadline.UTC().Format(time.RFC3339Nano), WorkerExecutable: "/fixture/delegate-run", WorkerSHA256: digest,
		Supervisor: continuationSupervisorRef(store.Root, digest, "unix:/fixture/pueue.sock"),
	}
	operation, err := workspace.OpenOperation(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := operation.Close(); err != nil {
			t.Error(err)
		}
	})
	return store, operation
}

func TestExpiredWorkspaceOperationSealsUnlaunchedWithoutNativeEvidence(t *testing.T) {
	store, operation := newAppWorkspaceOperationDeadline(t, time.Now().Add(-time.Second))
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
	proof, err := workspaceOperationClosed(t.Context(), operation)
	if err != nil || proof.unlaunchedSHA256 == "" || proof.terminationSHA256 != "" || proof.workerWaitSHA256 != "" {
		t.Fatalf("unlaunched operation received synthetic native evidence: %+v, %v", proof, err)
	}
	if err = operation.RecordReceipt(t.Context(), 1); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("late receipt bypassed launch fence: %v", err)
	}
	if _, err = operation.SubmissionPermit(t.Context()); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("closed operation granted another submission: %v", err)
	}
	if _, _, err = acceptedWorkspaceOperation(t.Context(), store, operation); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("unlaunched operation became successful worker output: %v", err)
	}
	requestAfter, err := task.MarshalCanonical(operation.Request())
	if err != nil || string(requestBefore) != string(requestAfter) {
		t.Fatalf("fencing renewed operation request/deadline: %v", err)
	}
}

func TestWorkspaceCleanupAuthorizationDoesNotSealExpiredOperation(t *testing.T) {
	_, operation := newAppWorkspaceOperationDeadline(t, time.Now().Add(-time.Second))
	if err := reconcileEndedWorkspaceOperation(t.Context(), operation, pueue.Job{}); !errors.Is(err, workspace.ErrPending) {
		t.Fatalf("cleanup inspection created its own closure authority: %v", err)
	}
	if _, err := operation.ReadLifetimeUnlaunchedContext(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("cleanup inspection changed the operation journal: %v", err)
	}
	proof, err := workspaceOperationClosed(t.Context(), operation)
	if err != nil || proof.unlaunchedSHA256 == "" {
		t.Fatalf("coordinator could not seal the same expired operation: %+v, %v", proof, err)
	}
	if err = reconcileEndedWorkspaceOperation(t.Context(), operation, pueue.Job{}); err != nil {
		t.Fatalf("cleanup did not accept an already recorded no-launch proof: %v", err)
	}
}

func TestWorkspaceReceiptReplayAfterFenceDoesNotAuthorizeNewWork(t *testing.T) {
	_, operation := newAppWorkspaceOperationDeadline(t, time.Now().Add(-time.Second))
	if err := operation.RecordReceipt(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceOperationClosed(t.Context(), operation); err != nil {
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

func workspaceGuardianWithRealRecords(t *testing.T, store *taskdir.Store, operation *workspace.Operation) *lifetimeGuardianTestFixture {
	t.Helper()
	return newLifetimeGuardianTestFixtureWithOptions(t, nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative, nil, nil, func(options *lifetimeGuardianOptions) {
		options.rootID, options.taskID = store.RootID, operation.Request().OperationID
		options.ownerKind, options.records, options.deadline = task.LifetimeOwnerWorkspace, operation, operation.Deadline()
		options.acquireOwner = func(ctx context.Context) (lifetimeGuardianOwner, error) { return operation.AcquireLifetimeOwner(ctx) }
		options.reconcileOwned = func(ctx context.Context) error { return reconcileControlLifetimeOwned(ctx, operation) }
	})
}

func TestWorkspaceGuardianRequiresExactWorkerWaitBeforeAcceptance(t *testing.T) {
	store, operation := newAppWorkspaceOperation(t)
	fixture := workspaceGuardianWithRealRecords(t, store, operation)
	fixture.callMust(t, "hello", fixture.message(lifetimeMessageHello, 0, 0))
	fixture.callMust(t, "begin", fixture.message(lifetimeMessageBegin, 1, 0))
	fixture.callMust(t, "started", fixture.message(lifetimeMessageStarted, 1, 101))
	fixture.callMust(t, "finish", fixture.message(lifetimeMessageFinish, 1, 0))
	fixture.callMust(t, "close", fixture.message(lifetimeMessageClose, 0, 0))
	if _, err := workspaceOperationClosed(t.Context(), operation); !errors.Is(err, workspace.ErrPending) {
		t.Fatalf("accepted scope closure before managed worker Wait: %v", err)
	}
	_, permitErr := operation.SubmissionPermit(t.Context())
	if !errors.Is(permitErr, task.ErrTerminalTask) || !errors.Is(workspaceLaunchPermissionError(permitErr), workspace.ErrPending) {
		t.Fatalf("scope-close scheduling race was not pending: %v", permitErr)
	}
	receiptErr := operation.RecordReceipt(t.Context(), 1)
	if !errors.Is(receiptErr, task.ErrTerminalTask) || !errors.Is(workspaceLaunchPermissionError(receiptErr), workspace.ErrPending) {
		t.Fatalf("scope-close receipt race was not pending: %v", receiptErr)
	}
	fixture.worker.release()
	if err := fixture.waitRun(t); err != nil {
		t.Fatal(err)
	}
	proof, err := workspaceOperationClosed(t.Context(), operation)
	if err != nil {
		t.Fatal(err)
	}
	if proof.terminationSHA256 == "" || proof.workerWaitSHA256 == "" {
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
	store, operation := newAppWorkspaceOperation(t)
	artifacts := []workspace.Artifact{{Name: "diff.patch", SHA256: task.ComputeSHA256(nil)}, {Name: "name-status.z", SHA256: task.ComputeSHA256(nil)}}
	if err := operation.RecordResult(t.Context(), workspace.OperationResult{SchemaVersion: task.SchemaVersion, OperationID: operation.Request().OperationID, RequestSHA256: operation.Digest(), Status: "succeeded", Artifacts: artifacts}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptedWorkspaceOperation(t.Context(), store, operation); !errors.Is(err, workspace.ErrPending) {
		t.Fatalf("worker candidate replaced guardian authority: %v", err)
	}
}

func TestWorkspaceOperationGuardNeverGrantsASecondSubmission(t *testing.T) {
	_, operation := newAppWorkspaceOperation(t)
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
