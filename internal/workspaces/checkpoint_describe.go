package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// Checkpoint states derived from committed records. Pending means no
// qualified receipt is committed yet.
const (
	CheckpointPending = "pending"
)

// CheckpointDescription is derived from committed checkpoint records only.
// Usable means a qualified checkpointed receipt and no registered removal
// attempt; it is metadata, not proof that the owned ref is still present.
// SnapshotRef and CommitOID name a stored candidate, which may remain for
// cleanup after an unavailable creation.
type CheckpointDescription struct {
	ID          string                        `json:"id"`
	Selection   string                        `json:"selection"`
	WorkspaceID string                        `json:"workspace_id,omitempty"`
	Cwd         string                        `json:"cwd,omitempty"`
	State       string                        `json:"state"`
	ReasonCode  string                        `json:"reason_code,omitempty"`
	SnapshotRef string                        `json:"snapshot_ref,omitempty"`
	CommitOID   string                        `json:"commit_oid,omitempty"`
	Usable      bool                          `json:"usable"`
	Removal     *CheckpointRemovalDescription `json:"removal,omitempty"`
}

// CheckpointRemovalDescription is one registered removal attempt. The
// containing field selects it: CheckpointDescription.Removal is the latest
// attempt, and ObserveCheckpointRemoval's selected result is the requested
// one. Attempts counts every registered attempt of the validated inventory;
// it is never the described attempt's sequence.
type CheckpointRemovalDescription struct {
	OperationID string `json:"operation_id"`
	Attempts    uint64 `json:"attempts"`
	State       string `json:"state"`
	ReasonCode  string `json:"reason_code,omitempty"`
}

// ObserveCheckpoint reconciles one checkpoint and describes it. It qualifies
// the creation, redriving an unfinished ordinary helper, and reconciles the
// latest removal attempt without registering another. A managed creation's
// still-active reservation is settled through ReconcileUse under a free use
// owner; a held owner leaves it to that owner. A recovery fault is joined
// with the description of the creation's own records.
func ObserveCheckpoint(ctx context.Context, store *taskdir.Store, deps OperationOptions, checkpointID string) (description CheckpointDescription, resultErr error) {
	control, cancel := workspaceSchedulingControl(ctx, deps)
	defer cancel()
	original, err := loadCheckpointCreation(control, store, checkpointID)
	if err != nil {
		return description, err
	}
	defer func() { resultErr = errors.Join(resultErr, original.Close()) }()
	_, observeErr := QualifyCheckpoint(control, store, original)
	if observeErr != nil && !PendingError(observeErr) {
		return description, observeErr
	}
	var recoverErr error
	if original.Request().WorkspaceID == "" {
		if observeErr != nil {
			observeErr = errors.Join(observeErr, ScheduleExistingOperation(control, store, original, deps))
		}
	} else {
		var reconciled bool
		reconciled, recoverErr = reconcileManagedCheckpointUse(control, store, original, deps)
		if reconciled && recoverErr == nil {
			_, observeErr = QualifyCheckpoint(control, store, original)
		}
	}
	if observeErr == nil && recoverErr == nil {
		_, observeErr = reconcileCheckpointRemoval(control, store, original, deps)
	}
	if observeErr != nil && !PendingError(observeErr) {
		return description, errors.Join(observeErr, recoverErr)
	}
	// Description validates its own fresh inventory: another attempt may have
	// registered since reconciliation.
	description, err = describeCheckpoint(control, original)
	return description, errors.Join(observeErr, recoverErr, err)
}

// reconcileManagedCheckpointUse hands original's still-active reservation to
// ReconcileUse, the settlement owner, under a nonblocking use-owner lease.
// The reservation naming original's operation ID is authenticated before the
// lease and again under it: a forged binding is a fault even while another
// owner holds the lease. A clean absence, another use or clean owner
// contention is no work; any other read, lease or close fault is returned
// whole.
func reconcileManagedCheckpointUse(ctx context.Context, store *taskdir.Store, original *workspace.Operation, deps OperationOptions) (reconciled bool, resultErr error) {
	resource, err := workspace.LoadResource(ctx, store, original.Request().WorkspaceID)
	if err != nil {
		return false, err
	}
	held := &Admission{resource: resource}
	defer func() { resultErr = errors.Join(resultErr, held.closeAfter(store, resultErr)) }()
	if selected, selectErr := checkpointReservationActive(ctx, resource, original); selectErr != nil || !selected {
		return false, selectErr
	}
	owner, err := resource.TryAcquireUseOwner(ctx)
	held.owner = owner
	if useOwnerContended(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if selected, selectErr := checkpointReservationActive(ctx, resource, original); selectErr != nil || !selected {
		return false, selectErr
	}
	return true, ReconcileUse(ctx, store, resource, deps)
}

// checkpointReservationActive reports whether resource's active reservation
// names original's operation. Only a clean absence means no active use, and
// a reservation naming original's ID must carry its full settlement binding.
func checkpointReservationActive(ctx context.Context, resource *workspace.Resource, original *workspace.Operation) (bool, error) {
	reservation, err := resource.Reservation(ctx)
	if workspace.OperationAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if reservation.Subject.Kind != workspace.SubjectOperation || reservation.Subject.ID != original.Request().OperationID {
		return false, nil
	}
	if err = requireManagedCheckpointReservation(resource, reservation, original); err != nil {
		return false, err
	}
	return true, nil
}

// useOwnerContended reports a lease refusal whose every cause is another
// live owner; a joined concrete fault is not contention.
func useOwnerContended(err error) bool {
	var cleanup *taskdir.LifetimeOwnerCleanupError
	if errors.As(err, &cleanup) {
		return false
	}
	return task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, task.ErrLockBusy) })
}

// ListCheckpoints describes checkpoint creations after the cursor in ID order
// from committed metadata. It never closes, fences, recovers or submits an
// operation. Only an operation directory whose request record is cleanly
// absent is skipped; every later load error is a fault.
func ListCheckpoints(ctx context.Context, store *taskdir.Store, after string, limit int) (page []CheckpointDescription, cursor string, resultErr error) {
	if limit < 1 {
		return nil, "", task.ErrInvalidPermit
	}
	ids, err := store.WorkspaceOperationIDs(ctx)
	if err != nil {
		return nil, "", err
	}
	page = []CheckpointDescription{}
	for _, id := range ids {
		if id <= after {
			continue
		}
		description, listed, describeErr := listedCheckpoint(ctx, store, id)
		if describeErr != nil {
			return nil, "", describeErr
		}
		if !listed {
			continue
		}
		if len(page) == limit {
			return page, page[len(page)-1].ID, nil
		}
		page = append(page, description)
	}
	return page, "", nil
}

func listedCheckpoint(ctx context.Context, store *taskdir.Store, id string) (description CheckpointDescription, listed bool, resultErr error) {
	present, err := workspace.OperationRequestPresent(ctx, store, id)
	if err != nil || !present {
		return description, false, err
	}
	operation, err := workspace.LoadOperation(ctx, store, id)
	if err != nil {
		return description, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if operation.Request().Kind != workspace.OperationCheckpoint {
		return description, false, nil
	}
	if err = validateCheckpointCreationIntent(operation.Request()); err != nil {
		return description, false, err
	}
	description, err = describeCheckpoint(ctx, operation)
	return description, err == nil, err
}

// describeCheckpoint projects committed creation and removal records. It
// reads only the original's records; a missing receipt is pending, never
// inferred from closure. A refused removal history or any read fault returns
// no description.
func describeCheckpoint(ctx context.Context, operation *workspace.Operation) (CheckpointDescription, error) {
	request := operation.Request()
	description := CheckpointDescription{
		ID: request.OperationID, Selection: request.Checkpoint.Kind, WorkspaceID: request.WorkspaceID,
		Cwd: request.Checkpoint.Cwd, State: CheckpointPending,
	}
	receipt, err := operation.CheckpointReceipt(ctx)
	if err == nil {
		description.State, description.ReasonCode = receipt.Status, receipt.ReasonCode
	} else if !workspace.OperationAbsent(err) {
		return CheckpointDescription{}, err
	}
	candidate, err := operation.CheckpointCandidate(ctx)
	if err == nil {
		description.SnapshotRef, description.CommitOID = candidate.SnapshotRef, candidate.CommitOID
	} else if !workspace.OperationAbsent(err) {
		return CheckpointDescription{}, err
	}
	attempts, err := operation.CheckpointRemovalAttempts(ctx)
	if err != nil {
		return CheckpointDescription{}, err
	}
	if err = requireCheckpointRemovalHistory(ctx, operation, attempts); err != nil {
		return CheckpointDescription{}, err
	}
	description.Usable = description.State == workspace.CheckpointCheckpointed && len(attempts) == 0
	if len(attempts) == 0 {
		return description, nil
	}
	head := attempts[len(attempts)-1]
	removal := &CheckpointRemovalDescription{OperationID: head.OperationID, Attempts: uint64(len(attempts))}
	if err = describeCheckpointRemovalState(ctx, operation, head, removal); err != nil {
		return CheckpointDescription{}, err
	}
	description.Removal = removal
	return description, nil
}

// describeCheckpointRemovalState projects attempt's committed receipt: its
// validated disposition, or pending only after a clean receipt absence. Any
// other read fault leaves the state unset and is returned whole.
func describeCheckpointRemovalState(ctx context.Context, original *workspace.Operation, attempt workspace.CheckpointRemovalReference, removal *CheckpointRemovalDescription) error {
	receipt, err := original.CheckpointRemovalReceipt(ctx, attempt)
	if workspace.OperationAbsent(err) {
		removal.State = CheckpointPending
		return nil
	}
	if err != nil {
		return err
	}
	removal.State, removal.ReasonCode = receipt.Status, receipt.ReasonCode
	return nil
}

// OperationOutcome is the canonical completion verdict of one workspace
// operation: generic acceptance for existing kinds and strict qualification
// for a checkpoint, whose unavailable disposition is an operation failure.
func OperationOutcome(ctx context.Context, store *taskdir.Store, operation *workspace.Operation) error {
	if operation.Request().Kind != workspace.OperationCheckpoint {
		_, _, err := AcceptedOperation(ctx, store, operation)
		return err
	}
	receipt, err := QualifyCheckpoint(ctx, store, operation)
	if err != nil {
		return err
	}
	if receipt.Status != workspace.CheckpointCheckpointed {
		return &OperationError{reason: receipt.ReasonCode}
	}
	return nil
}
