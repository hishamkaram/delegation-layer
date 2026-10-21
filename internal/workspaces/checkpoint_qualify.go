package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// checkpointOutcome is the qualified verdict of one closed checkpoint helper.
type checkpointOutcome uint8

const (
	checkpointSucceeded checkpointOutcome = iota + 1
	checkpointFailed
	checkpointNotLaunched
	checkpointResultUnavailable
)

// checkpointUnavailableReasons names one record family's unavailable reasons.
type checkpointUnavailableReasons struct{ failed, notLaunched, resultUnavailable string }

var (
	checkpointCreationReasons = checkpointUnavailableReasons{
		failed: workspace.CheckpointFailed, notLaunched: workspace.CheckpointNotLaunched, resultUnavailable: workspace.CheckpointResultUnavailable,
	}
	checkpointRemovalReasons = checkpointUnavailableReasons{
		failed: workspace.CheckpointRemovalFailed, notLaunched: workspace.CheckpointRemovalNotLaunched, resultUnavailable: workspace.CheckpointRemovalResultUnavailable,
	}
)

func (r checkpointUnavailableReasons) reason(outcome checkpointOutcome) string {
	switch outcome {
	case checkpointFailed:
		return r.failed
	case checkpointNotLaunched:
		return r.notLaunched
	case checkpointResultUnavailable:
		return r.resultUnavailable
	case checkpointSucceeded:
		return ""
	}
	return ""
}

// checkpointClosureQualified is the strict checkpoint success predicate. Only
// a registered native closure observed the worker's exact Wait; recovered,
// legacy, unregistered and unlaunched closures never qualify a new success,
// whatever termination or worker hashes they carry.
func checkpointClosureQualified(proof guardian.ClosureProof) bool {
	return proof.Kind() == task.LifetimeClosureRegisteredNative
}

// classifyCheckpointCompletion interprets resolved generic acceptance. Its
// unlaunched verdict covers exactly the closures whose worker never ran.
func classifyCheckpointCompletion(proof guardian.ClosureProof, acceptErr error) checkpointOutcome {
	switch {
	case errors.Is(acceptErr, task.ErrLifetimeUnlaunched):
		return checkpointNotLaunched
	case acceptErr == nil && checkpointClosureQualified(proof):
		return checkpointSucceeded
	case acceptErr == nil || errors.Is(acceptErr, ErrOperationResultUnavailable):
		return checkpointResultUnavailable
	default:
		return checkpointFailed
	}
}

// acceptedCheckpointCompletion wraps generic acceptance. Only a resolved
// helper verdict after canonical closure is qualified; pending, storage and
// identity errors are returned unchanged and remain retryable.
func acceptedCheckpointCompletion(ctx context.Context, store *taskdir.Store, operation *workspace.Operation) (guardian.ClosureProof, checkpointOutcome, error) {
	_, proof, acceptErr := AcceptedOperation(ctx, store, operation)
	if !resolvedWorkspaceCaptureResult(acceptErr) {
		return proof, 0, acceptErr
	}
	if !proof.Closed() {
		return proof, 0, errors.Join(workspace.ErrPending, acceptErr)
	}
	return proof, classifyCheckpointCompletion(proof, acceptErr), nil
}

// receiptClosureEvidence is the canonical receipt encoding of a closure: only
// the recovery kinds without a native worker wait name their kind.
func receiptClosureEvidence(proof guardian.ClosureProof) workspace.ClosureEvidence {
	closure := workspace.ClosureEvidence{TerminationSHA256: proof.TerminationSHA256(), WorkerWaitSHA256: proof.WorkerWaitSHA256(), UnlaunchedSHA256: proof.UnlaunchedSHA256()}
	if proof.Kind() == task.LifetimeClosureUnregisteredRecovered || proof.Kind() == task.LifetimeClosureLegacyPreviousBoot {
		closure.Kind = proof.Kind()
	}
	return closure
}

// QualifyCheckpoint returns the creation's qualified disposition. A stored
// receipt is authoritative and is read before any closure fact is recomputed;
// only its clean absence computes one after canonical closure. A write error
// is returned whole; the next call reads the durable winner.
func QualifyCheckpoint(ctx context.Context, store *taskdir.Store, operation *workspace.Operation) (workspace.CheckpointReceipt, error) {
	if operation.Request().Kind != workspace.OperationCheckpoint {
		return workspace.CheckpointReceipt{}, task.ErrIdentityMismatch
	}
	stored, err := operation.CheckpointReceipt(ctx)
	if !workspace.OperationAbsent(err) {
		return stored, err
	}
	proof, outcome, err := acceptedCheckpointCompletion(ctx, store, operation)
	if err != nil {
		return workspace.CheckpointReceipt{}, err
	}
	closure := receiptClosureEvidence(proof)
	receipt := workspace.CheckpointReceipt{
		LifetimeClosure: closure.Kind, SchemaVersion: task.SchemaVersion, OperationID: operation.Request().OperationID,
		RequestSHA256: operation.Digest(), Status: workspace.CheckpointUnavailable, ReasonCode: checkpointCreationReasons.reason(outcome),
		TerminationSHA256: closure.TerminationSHA256, WorkerWaitSHA256: closure.WorkerWaitSHA256, UnlaunchedSHA256: closure.UnlaunchedSHA256,
	}
	if outcome == checkpointSucceeded {
		// The worker reports success only after its candidate is durable, so a
		// qualified success without one is inconsistent evidence.
		candidate, candidateErr := operation.CheckpointCandidate(ctx)
		if workspace.OperationAbsent(candidateErr) {
			return workspace.CheckpointReceipt{}, errors.Join(task.ErrEvidenceFault, candidateErr)
		}
		if candidateErr != nil {
			return workspace.CheckpointReceipt{}, candidateErr
		}
		if err = requireManagedCheckpointTarget(ctx, store, operation.Request(), candidate.Target); err != nil {
			return workspace.CheckpointReceipt{}, err
		}
		receipt.Status, receipt.Candidate = workspace.CheckpointCheckpointed, &candidate
	}
	if err = operation.RecordCheckpointReceipt(ctx, receipt); err != nil {
		return workspace.CheckpointReceipt{}, err
	}
	return receipt, nil
}

// requireManagedCheckpointTarget binds a managed candidate to its workspace's
// immutable source provenance and persisted checkout descriptor, which the
// resource cross-checks with its registration. It needs no physical checkout,
// so history stays qualifiable after release; topology is checked by the
// worker before any effect. An ordinary target is its own source.
func requireManagedCheckpointTarget(ctx context.Context, store *taskdir.Store, request workspace.OperationRequest, target workspace.CheckpointTarget) (resultErr error) {
	if request.Checkpoint.Kind != workspace.CheckpointManaged {
		return nil
	}
	resource, err := workspace.LoadResource(ctx, store, request.WorkspaceID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	source, err := resource.Source(ctx)
	if err != nil {
		return err
	}
	checkout, err := resource.Checkout(ctx)
	if err != nil {
		return err
	}
	if target.Source != source || target.CheckoutRoot != checkout.CheckoutPath || target.AdminDir != checkout.AdminDir {
		return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	return nil
}

// qualifyCheckpointRemoval returns one registered attempt's qualified
// disposition with the same stored-first rule. Success is removed while the
// original candidate exists and nothing_to_remove without one; the helper's
// transient ref observation is never the disposition.
func qualifyCheckpointRemoval(ctx context.Context, store *taskdir.Store, original, removal *workspace.Operation, attempt workspace.CheckpointRemovalReference) (workspace.CheckpointRemovalReceipt, error) {
	stored, err := original.CheckpointRemovalReceipt(ctx, attempt)
	if !workspace.OperationAbsent(err) {
		return stored, err
	}
	proof, outcome, err := acceptedCheckpointCompletion(ctx, store, removal)
	if err != nil {
		return workspace.CheckpointRemovalReceipt{}, err
	}
	closure := receiptClosureEvidence(proof)
	receipt := workspace.CheckpointRemovalReceipt{
		LifetimeClosure: closure.Kind, SchemaVersion: task.SchemaVersion, Attempt: attempt,
		Status: workspace.CheckpointUnavailable, ReasonCode: checkpointRemovalReasons.reason(outcome),
		TerminationSHA256: closure.TerminationSHA256, WorkerWaitSHA256: closure.WorkerWaitSHA256, UnlaunchedSHA256: closure.UnlaunchedSHA256,
	}
	if outcome == checkpointSucceeded {
		receipt.Status, err = checkpointRemovalDisposition(ctx, original)
		if err != nil {
			return workspace.CheckpointRemovalReceipt{}, err
		}
	}
	if err = original.RecordCheckpointRemovalReceipt(ctx, removal, receipt); err != nil {
		return workspace.CheckpointRemovalReceipt{}, err
	}
	return receipt, nil
}

func checkpointRemovalDisposition(ctx context.Context, original *workspace.Operation) (string, error) {
	_, err := original.CheckpointCandidate(ctx)
	if err == nil {
		return workspace.CheckpointRemoved, nil
	}
	if workspace.OperationAbsent(err) {
		return workspace.CheckpointNothingToRemove, nil
	}
	return "", err
}

// FinalizeCheckpoint publishes the qualified disposition of a completed
// ordinary creation or removal attempt, which own no workspace use. It never
// registers, submits or redrives work; an open closure stays pending.
func FinalizeCheckpoint(ctx context.Context, store *taskdir.Store, operation *workspace.Operation) (resultErr error) {
	request := operation.Request()
	switch request.Kind {
	case workspace.OperationCheckpoint:
		_, err := QualifyCheckpoint(ctx, store, operation)
		return err
	case workspace.OperationCheckpointRemove:
		original, err := loadCheckpointCreation(ctx, store, request.Subject.ID)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, original.Close()) }()
		attempt, err := registeredCheckpointRemoval(ctx, original, operation)
		if err != nil {
			return err
		}
		_, err = qualifyCheckpointRemoval(ctx, store, original, operation, attempt)
		return err
	default:
		return task.ErrIdentityMismatch
	}
}

// settleManagedCheckpointUse closes a managed checkpoint's reservation. The
// use owner authenticates the operation it reserved, repairs a missing
// resource reference from the canonical request, publishes the qualified
// disposition and closes that same reservation without any capture work.
func settleManagedCheckpointUse(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, reservation workspace.Reservation, operation *workspace.Operation, deps OperationOptions) error {
	request := operation.Request()
	if err := requireManagedCheckpointReservation(resource, reservation, operation); err != nil {
		return err
	}
	if err := validateStoredWorkspaceActionRequest(ctx, store, operation); err != nil {
		return err
	}
	if err := resource.RecordOperation(ctx, request); err != nil {
		return err
	}
	if _, err := QualifyCheckpoint(ctx, store, operation); err != nil {
		if PendingError(err) {
			return errors.Join(err, ScheduleExistingOperation(ctx, store, operation, deps))
		}
		return err
	}
	return resource.CloseReservation(ctx, reservation, "operation-completed", "")
}

// requireManagedCheckpointReservation binds reservation to operation's full
// subject, its managed checkpoint request and resource's workspace.
func requireManagedCheckpointReservation(resource *workspace.Resource, reservation workspace.Reservation, operation *workspace.Operation) error {
	request := operation.Request()
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: request.OperationID, SHA256: operation.Digest()}
	if reservation.Subject != subject || request.WorkspaceID != resource.Request().WorkspaceID ||
		request.Checkpoint == nil || request.Checkpoint.Kind != workspace.CheckpointManaged {
		return task.ErrEvidenceFault
	}
	return nil
}
