package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"golang.org/x/sys/unix"
)

func prepareRecoveredCapture(t *testing.T, kind task.LifetimeClosureKind) (*workspaceRecoveryFixture, workspace.Reservation, workspace.CaptureObligation, string) {
	t.Helper()
	fixture := newWorkspaceRecoveryFixture(t)
	subject := workspace.Subject{Kind: workspace.SubjectTask, ID: fixture.resource.Request().TaskID, SHA256: fixture.resource.Request().RequestSHA256}
	reservation, err := fixture.resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.resource.RecordCaptureObligation(t.Context(), reservation, subject); err != nil {
		t.Fatal(err)
	}
	obligation, err := fixture.resource.CaptureObligation(t.Context(), reservation.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	operationID := strings.Repeat("d", 32)
	operation := openRecoveryOperation(t, fixture, operationID, workspace.OperationCapture, subject)
	defer func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if _, err = fixture.resource.AssociateOperation(t.Context(), workspace.CapturePurpose(reservation.Sequence), operation.Request()); err != nil {
		t.Fatal(err)
	}
	seedRecoveredCaptureClosure(t, operation, kind)
	return fixture, reservation, obligation, operationID
}

func seedRecoveredCaptureClosure(t *testing.T, operation *workspace.Operation, kind task.LifetimeClosureKind) {
	t.Helper()
	owner, err := operation.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	facts := taskdir.LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "recovered-capture", BootID: "recovered-capture-boot", Deadline: operation.Deadline()}
	if _, err = operation.RecordLifetimeIntentContext(t.Context(), facts); err != nil {
		t.Fatal(err)
	}
	if kind == task.LifetimeClosureUnregisteredRecovered {
		_, err = operation.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsenceUnregisteredDisposed, facts.BootID)
	} else {
		_, err = operation.RecordLifetimeRegistrationContext(t.Context(), taskdir.LifetimeRegistrationFacts{ScopeKind: facts.ScopeKind, Assurance: facts.Assurance, ScopeID: "recovered-capture-scope", BootID: facts.BootID, Deadline: facts.Deadline})
		if err == nil {
			_, err = operation.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationPreviousBoot)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceCaptureFinalizesRecoveredClosureWithoutNativeWait(t *testing.T) {
	for _, kind := range []task.LifetimeClosureKind{task.LifetimeClosureUnregisteredRecovered, task.LifetimeClosureLegacyPreviousBoot} {
		t.Run(string(kind), func(t *testing.T) {
			fixture, reservation, obligation, operationID := prepareRecoveredCapture(t, kind)
			fixture.restart(t)
			workspaceRecoveryUseOwner(t, fixture)
			if err := runWorkspaceCapture(t.Context(), fixture.store, fixture.resource, Dependencies{}, reservation, obligation); err != nil {
				t.Fatal(err)
			}
			receipt, err := fixture.resource.CaptureReceipt(t.Context(), operationID)
			if err != nil {
				t.Fatal(err)
			}
			wantReason := "capture_result_unavailable"
			if kind == task.LifetimeClosureUnregisteredRecovered {
				wantReason = "capture_not_launched"
			}
			if receipt.Status != "unavailable" || receipt.ReasonCode != wantReason || receipt.LifetimeClosure != kind {
				t.Fatalf("recovered receipt = %+v", receipt)
			}
			if _, err = fixture.resource.Reservation(t.Context()); !workspace.OperationAbsent(err) {
				t.Fatalf("closed capture still holds the reservation: %v", err)
			}
		})
	}
}

func TestWorkspaceCaptureReadFailureKeepsObligationRetryable(t *testing.T) {
	for _, injected := range []error{context.Canceled, context.DeadlineExceeded, errors.New("temporary result read failure"), task.ErrEvidenceFault, task.ErrIdentityMismatch, workspace.ErrPending, errors.Join(&workspaceOperationError{reason: "capture_failed"}, context.Canceled), errors.Join(errWorkspaceOperationResultUnavailable, errors.New("unlock failed"))} {
		t.Run(injected.Error(), func(t *testing.T) { verifyCaptureReadFailure(t, injected) })
	}
}

func verifyCaptureReadFailure(t *testing.T, injected error) {
	t.Helper()
	fixture, reservation, _, operationID := prepareRecoveredCapture(t, task.LifetimeClosureUnregisteredRecovered)
	operation, err := workspace.LoadOperation(t.Context(), fixture.store, operationID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	proof, err := workspaceOperationClosed(t.Context(), operation)
	if err != nil {
		t.Fatal(err)
	}
	_, err = recordWorkspaceCapture(t.Context(), fixture.resource, operation.Request(), reservation, workspace.OperationResult{}, proof, injected)
	if !errors.Is(err, injected) {
		t.Fatalf("result read error changed: %v", err)
	}
	if _, err = fixture.resource.CaptureReceipt(t.Context(), operationID); !workspace.OperationAbsent(err) {
		t.Fatalf("read failure committed a terminal receipt: %v", err)
	}
	current, err := fixture.resource.Reservation(t.Context())
	if err != nil || current != reservation {
		t.Fatalf("read failure lost the obligation: %+v, %v", current, err)
	}
}

type workspaceResultUnlockFault struct {
	taskdir.BaseFaultInjector
	path    string
	failure error
	fired   bool
}

func (f *workspaceResultUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (f *workspaceResultUnlockFault) OnLockParentDirBarrier(string) error { return nil }
func (f *workspaceResultUnlockFault) OnFlock(path string, operation int) error {
	if path == f.path && operation == unix.LOCK_UN && !f.fired {
		f.fired = true
		return f.failure
	}
	return nil
}

func TestWorkspaceMissingResultDoesNotHideTransactionFailure(t *testing.T) {
	store, operation := newAppWorkspaceOperation(t)
	injected := errors.New("result transaction unlock failed")
	fault := &workspaceResultUnlockFault{path: filepath.Join(store.Root, "workspace-operations", operation.Request().OperationID, ".operation.lock"), failure: injected}
	store.SetFaultInjector(fault)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	_, err := operation.Result(t.Context())
	if !fault.fired || !errors.Is(err, injected) || workspace.OperationAbsent(err) {
		t.Fatalf("transaction failure was classified as result absence: %v", err)
	}
}

func recordRecoveredCaptureCandidate(t *testing.T, fixture *workspaceRecoveryFixture, operation *workspace.Operation) {
	t.Helper()
	source := recoverySource(fixture)
	if err := fixture.resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	candidate := workspace.CaptureCandidate{
		SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.resource.Request().WorkspaceID, OperationID: operation.Request().OperationID, Subject: operation.Request().Subject,
		BaseOID: source.BaseOID, ObservedHEAD: strings.Repeat("e", 40), TreeOID: strings.Repeat("f", 40), ObjectFormat: source.ObjectFormat,
		SnapshotRef: "refs/delegate/capture-" + operation.Request().OperationID,
	}
	if err := fixture.resource.RecordCaptureCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordResult(t.Context(), successfulCaptureResult(operation)); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspacePreviousBootCaptureUsesValidatedResultAcrossRecoveryGap(t *testing.T) {
	for _, stage := range []string{"termination-only", "absence-published", "receipt-before-absence"} {
		t.Run(stage, func(t *testing.T) { verifyPreviousBootCaptureRecovery(t, stage) })
	}
}

func verifyPreviousBootCaptureRecovery(t *testing.T, stage string) {
	t.Helper()
	fixture, reservation, obligation, operationID := prepareRecoveredCapture(t, task.LifetimeClosureLegacyPreviousBoot)
	operation, err := workspace.LoadOperation(t.Context(), fixture.store, operationID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	recordRecoveredCaptureCandidate(t, fixture, operation)
	seedPreviousBootCaptureGap(t, fixture, operation, stage)
	if err = runWorkspaceCapture(t.Context(), fixture.store, fixture.resource, Dependencies{}, reservation, obligation); err != nil {
		t.Fatal(err)
	}
	receipt, err := fixture.resource.CaptureReceipt(t.Context(), operationID)
	if err != nil || receipt.Status != "captured" || receipt.Candidate == nil {
		t.Fatalf("previous-boot capture lost a valid result: %+v, %v", receipt, err)
	}
	if stage == "receipt-before-absence" && (receipt.LifetimeClosure != task.LifetimeClosureLegacyPreviousBoot || receipt.WorkerWaitSHA256 != "") {
		t.Fatal("receipt replay replaced its originally committed closure facts")
	}
	if _, err = fixture.resource.Reservation(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("captured result retained its reservation: %v", err)
	}
}

func seedPreviousBootCaptureGap(t *testing.T, fixture *workspaceRecoveryFixture, operation *workspace.Operation, stage string) {
	t.Helper()
	if stage == "receipt-before-absence" {
		result, proof, resultErr := acceptedWorkspaceOperation(t.Context(), fixture.store, operation)
		receipt, buildErr := buildWorkspaceCaptureReceipt(t.Context(), fixture.resource, operation.Request(), result, proof, resultErr)
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		if err := fixture.resource.RecordCaptureReceipt(t.Context(), receipt); err != nil {
			t.Fatal(err)
		}
	}
	if stage != "termination-only" {
		owner, ownerErr := operation.AcquireLifetimeOwner(t.Context())
		if ownerErr != nil {
			t.Fatal(ownerErr)
		}
		_, absenceErr := operation.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsencePreviousBoot, "new-capture-boot")
		if err := errors.Join(absenceErr, owner.Release()); err != nil {
			t.Fatal(err)
		}
	}
}
