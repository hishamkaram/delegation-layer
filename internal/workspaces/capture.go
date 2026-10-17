package workspaces

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// reconcileWorkspaceUse is the only use -> capture -> reusable transition.
// Its caller holds the resource use-owner lease. The same path serves success,
// timeout, verification and recovery; no provider lifetime is reopened.
func ReconcileUse(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps OperationOptions) (resultError error) {
	// The existing use owner carries this bounded transaction through receipt
	// publication and reservation closure, independently of a short watcher.
	control, finish := workspaceSchedulingControl(ctx, deps)
	defer finish()
	ctx = control
	reservation, err := resource.Reservation(ctx)
	if workspace.OperationAbsent(err) {
		return reconcileClosedWorkspaceUse(ctx, store, resource, deps)
	}
	if err != nil {
		return err
	}
	obligation, err := resource.CaptureObligation(ctx, reservation.Sequence)
	if workspace.OperationAbsent(err) {
		if reservation.Subject.Kind == workspace.SubjectAdmission {
			return ReconcileUnlaunchedAdmission(ctx, store, resource, reservation, deps.RecoverInspection)
		}
		if err = restoreWorkspaceCaptureObligation(ctx, store, resource, reservation); err != nil {
			return err
		}
		obligation, err = resource.CaptureObligation(ctx, reservation.Sequence)
	}
	if err != nil {
		return err
	}
	if obligation.Predecessor != nil {
		err = workspaceCapturePredecessorClosed(ctx, store, resource, reservation, obligation, deps)
		if workspace.OperationAbsent(err) && obligation.Predecessor.Kind == workspace.SubjectTask {
			return ReconcileUnlaunchedAdmission(ctx, store, resource, reservation, deps.RecoverInspection)
		}
	} else {
		err = resource.ReleaseCaptureAuthorized(ctx, obligation)
	}
	if err != nil {
		return err
	}
	return runWorkspaceCapture(ctx, store, resource, deps, reservation, obligation)
}

// The use-owner lease excludes a live publisher. Reconstruct only from the
// original typed authorization; a missing follow-up write is not new work.
func restoreWorkspaceCaptureObligation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, reservation workspace.Reservation) (resultErr error) {
	if reservation.Subject.Kind != workspace.SubjectOperation {
		return task.ErrEvidenceFault
	}
	operation, err := workspace.LoadOperation(ctx, store, reservation.Subject.ID)
	if workspace.OperationAbsent(err) {
		if _, err = resource.ReleaseAction(ctx, reservation.Subject); err != nil {
			return errors.Join(task.ErrEvidenceFault, err)
		}
		return resource.RecordReleaseCapture(ctx, reservation, reservation.Subject)
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	return RecordVerificationUse(ctx, resource, reservation, operation)
}

func RecordVerificationUse(ctx context.Context, resource *workspace.Resource, reservation workspace.Reservation, operation *workspace.Operation) error {
	request := operation.Request()
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: request.OperationID, SHA256: operation.Digest()}
	if request.WorkspaceID != resource.Request().WorkspaceID || request.Kind != workspace.OperationVerify || reservation.Subject != subject {
		return task.ErrEvidenceFault
	}
	if err := resource.RecordOperation(ctx, request); err != nil {
		return err
	}
	return resource.RecordCaptureObligation(ctx, reservation, subject)
}

func runWorkspaceCapture(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps OperationOptions, reservation workspace.Reservation, obligation workspace.CaptureObligation) (resultError error) {
	spec := OperationSpec{
		Purpose: workspace.CapturePurpose(reservation.Sequence),
		Kind:    workspace.OperationCapture, Subject: obligation.Subject,
		Budget: time.Duration(obligation.BudgetNanos),
	}
	operation, err := ScheduleOperation(ctx, store, resource, deps, spec, nil)
	defer func() { resultError = errors.Join(resultError, CloseOperation(operation)) }()
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return errors.Join(workspace.ErrPending, err)
	}
	result, proof, resultErr := AcceptedOperation(ctx, store, operation)
	request := operation.Request()
	receipt, err := recordWorkspaceCapture(ctx, resource, request, reservation, result, proof, resultErr)
	if err != nil {
		return err
	}
	if obligation.Predecessor == nil {
		return completeWorkspaceReleaseCapture(ctx, store, resource, deps, receipt)
	}
	return nil
}

// Only a resolved helper result may close a capture obligation. Storage and
// observer errors leave it retryable, even when lifetime closure is durable.
func recordWorkspaceCapture(ctx context.Context, resource *workspace.Resource, request workspace.OperationRequest, reservation workspace.Reservation, result workspace.OperationResult, proof guardian.ClosureProof, resultErr error) (workspace.CaptureReceipt, error) {
	committed, err := resource.CaptureReceipt(ctx, request.OperationID)
	if err == nil {
		if committed.Subject != request.Subject {
			return committed, task.ErrIdentityMismatch
		}
		return committed, resource.CloseReservation(ctx, reservation, committed.Status, request.OperationID)
	}
	if !workspace.OperationAbsent(err) {
		return committed, err
	}
	if !resolvedWorkspaceCaptureResult(resultErr) {
		return workspace.CaptureReceipt{}, resultErr
	}
	if !proof.Closed() {
		return workspace.CaptureReceipt{}, errors.Join(workspace.ErrPending, resultErr)
	}
	receipt, err := BuildCaptureReceipt(ctx, resource, request, result, proof, resultErr)
	if err != nil {
		return receipt, err
	}
	if err = resource.RecordCaptureReceipt(ctx, receipt); err != nil {
		return receipt, err
	}
	return receipt, resource.CloseReservation(ctx, reservation, receipt.Status, request.OperationID)
}

func BuildCaptureReceipt(ctx context.Context, resource *workspace.Resource, request workspace.OperationRequest, result workspace.OperationResult, proof guardian.ClosureProof, resultErr error) (workspace.CaptureReceipt, error) {
	receipt := workspace.CaptureReceipt{
		SchemaVersion: task.SchemaVersion, WorkspaceID: request.WorkspaceID, OperationID: request.OperationID,
		Subject: request.Subject, Status: "unavailable", ReasonCode: "capture_failed",
		TerminationSHA256: proof.TerminationSHA256(), WorkerWaitSHA256: proof.WorkerWaitSHA256(),
		UnlaunchedSHA256: proof.UnlaunchedSHA256(),
	}
	if proof.Kind() == task.LifetimeClosureUnregisteredRecovered || proof.Kind() == task.LifetimeClosureLegacyPreviousBoot {
		receipt.LifetimeClosure = proof.Kind()
	}
	if proof.UnlaunchedSHA256() != "" || proof.Kind() == task.LifetimeClosureUnregisteredRecovered {
		receipt.ReasonCode = "capture_not_launched"
	} else if errors.Is(resultErr, ErrOperationResultUnavailable) {
		receipt.ReasonCode = "capture_result_unavailable"
	}
	if resultErr == nil {
		candidate, candidateErr := resource.CaptureCandidate(ctx, request.OperationID)
		if candidateErr == nil {
			receipt.Status, receipt.ReasonCode, receipt.Candidate, receipt.Artifacts = "captured", "", &candidate, result.Artifacts
		} else if !workspace.OperationAbsent(candidateErr) {
			return receipt, candidateErr
		}
	}
	return receipt, nil
}

func reconcileClosedWorkspaceUse(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps OperationOptions) error {
	latest, err := resource.LatestReservation(ctx)
	if err != nil && !workspace.OperationAbsent(err) {
		return err
	}
	if err == nil {
		if err = reconcileClosedReleaseCapture(ctx, store, resource, deps, latest.Sequence); err != nil {
			return err
		}
	}
	if err = ReconcileRemoval(ctx, store, resource, deps, false); err != nil {
		return err
	}
	_, _, err = ReconcileForgetOperation(ctx, store, resource, deps, false, 0)
	return err
}

func workspaceCapturePredecessorClosed(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, reservation workspace.Reservation, obligation workspace.CaptureObligation, deps OperationOptions) (resultErr error) {
	if obligation.Predecessor == nil {
		return task.ErrEvidenceFault
	}
	subject := *obligation.Predecessor
	switch subject.Kind {
	case workspace.SubjectTask:
		return workspaceProviderUseClosed(ctx, store, subject)
	case workspace.SubjectOperation:
		if reservation.Subject != subject || obligation.Subject != subject {
			return task.ErrEvidenceFault
		}
		return workspaceAuxiliaryUseClosed(ctx, store, resource, reservation, subject, deps)
	case workspace.SubjectAdmission:
		return task.ErrInvalidPermit
	default:
		return task.ErrIdentityMismatch
	}
}

func workspaceProviderUseClosed(ctx context.Context, store *taskdir.Store, subject workspace.Subject) (resultErr error) {
	td, err := store.OpenTask(subject.ID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	req, _, err := td.PreparedRecords()
	if err != nil {
		return err
	}
	data, err := task.MarshalCanonical(req)
	if err != nil {
		return err
	}
	if task.ComputeSHA256(data) != subject.SHA256 {
		return task.ErrIdentityMismatch
	}
	_, err = ReadLifetimeProof(ctx, td)
	if errors.Is(err, workspace.ErrPending) {
		proven, proofErr := td.ProveUnlaunchedTaskContext(ctx)
		if proofErr != nil {
			return proofErr
		}
		if proven {
			return nil
		}
	}
	return err
}

func workspaceAuxiliaryUseClosed(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, reservation workspace.Reservation, subject workspace.Subject, deps OperationOptions) (resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, subject.ID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if operation.Digest() != subject.SHA256 {
		return task.ErrIdentityMismatch
	}
	if err = RecordVerificationUse(ctx, resource, reservation, operation); err != nil {
		return err
	}
	_, err = OperationClosed(ctx, operation)
	if PendingError(err) {
		return errors.Join(err, ScheduleExistingOperation(ctx, store, operation, deps))
	}
	return err
}

func ReconcileUnlaunchedAdmission(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, reservation workspace.Reservation, recoverInspection InspectionRecovery) (resultErr error) {
	if reservation.Subject.Kind != workspace.SubjectAdmission {
		return task.ErrEvidenceFault
	}
	if err := workspaceAdmissionTaskAbsent(ctx, store, resource, reservation); err != nil {
		return err
	}
	ids, err := resource.OperationIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		operation, loadErr := workspace.LoadOperation(ctx, store, id)
		if loadErr != nil {
			return loadErr
		}
		var closeErr error
		if operation.Request().Subject == reservation.Subject {
			_, closeErr = OperationClosed(ctx, operation)
		}
		if err = errors.Join(closeErr, operation.Close()); err != nil {
			return err
		}
	}
	if err = AdmissionInspectionClosed(ctx, store, resource, reservation, recoverInspection); err != nil {
		return err
	}
	return resource.RecordAdmissionAborted(ctx, reservation)
}

func workspaceAdmissionTaskAbsent(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, reservation workspace.Reservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	td, err := store.OpenTask(reservation.Subject.ID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	request, _, readErr := td.PreparedRecords()
	if err = errors.Join(readErr, td.Close()); err != nil {
		return err
	}
	if request.Workspace != nil && request.Workspace.ID == resource.Request().WorkspaceID && request.Workspace.Reservation == reservation.Sequence && request.Workspace.RequestSHA256 == reservation.Subject.SHA256 {
		return workspace.ErrPending
	}
	return nil
}

func AdmissionInspectionClosed(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, reservation workspace.Reservation, recoverInspection InspectionRecovery) (resultErr error) {
	attempt, err := resource.ResolveAdmissionAttempt(ctx, reservation.Sequence, reservation.Subject)
	if err != nil {
		return err
	}
	identity, err := attempt.InspectionOperationID()
	if err != nil {
		return err
	}
	operation, err := inspection.LoadOperationContext(ctx, store, identity)
	if task.RecordAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	binding := operation.Request().Task.Workspace
	if binding == nil || binding.ID != resource.Request().WorkspaceID || binding.Reservation != reservation.Sequence || binding.RequestSHA256 != reservation.Subject.SHA256 {
		return task.ErrIdentityMismatch
	}
	if recoverInspection == nil {
		return task.ErrEvidenceFault
	}
	if err = recoverInspection(ctx, operation); err != nil {
		return err
	}
	_, err = ReadAuxiliaryProof(ctx, operation)
	return err
}
