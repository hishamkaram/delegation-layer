package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type endedTaskRequirement uint8

const (
	endedTaskPublished endedTaskRequirement = iota
	endedTaskClosedForMigration
)

// InspectEndedJob validates one ended row's native terminal evidence. It never
// consults custody, so migration keeps its pool-free verdict.
func (c *Cleanup) InspectEndedJob(ctx context.Context, store *taskdir.Store, legacy inspection.GroupRequestRecord, job pueue.Job) error {
	return c.inspectEndedJob(ctx, store, legacy, job, endedTaskClosedForMigration, nil)
}

// inspectEndedJob also applies validated, when non-nil, to an ended inspection
// row whose terminal evidence was validated. Other rows never reach it.
func (c *Cleanup) inspectEndedJob(ctx context.Context, store *taskdir.Store, legacy inspection.GroupRequestRecord, job pueue.Job, requirement endedTaskRequirement, validated func(context.Context, *inspection.Operation) error) (resultErr error) {
	if store == nil {
		return fmt.Errorf("%w: inspection state store is unavailable", inspection.ErrGroupMigrationRequired)
	}
	if legacy.RootID != store.RootID || legacy.Name != (pueue.InspectionIdentity{RootID: store.RootID}).Group() || legacy.ParallelTasks != 1 || task.ValidateFreshSupervisorRef(legacy.Supervisor) != nil {
		return fmt.Errorf("%w: invalid legacy inspection group binding", inspection.ErrGroupMigrationRequired)
	}
	if job.Label == nil {
		return nil
	}
	taskID, managed, inspectionWorker, err := LegacyJobTaskID(*job.Label, store.RootID)
	if err != nil {
		return fmt.Errorf("%w: ended Delegate job label is invalid: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if !managed {
		return nil
	}
	if inspectionWorker {
		return c.inspectEndedInspection(ctx, store, legacy, taskID, job, validated)
	}
	return inspectLegacyEndedTask(ctx, store, legacy, taskID, job, requirement)
}

func inspectLegacyEndedTask(ctx context.Context, store *taskdir.Store, legacy inspection.GroupRequestRecord, taskID string, job pueue.Job, requirement endedTaskRequirement) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	td, err := store.OpenTask(taskID)
	if err != nil {
		return fmt.Errorf("%w: ended Delegate task %s could not be reopened: %w", inspection.ErrGroupMigrationRequired, taskID, err)
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	receipt, err := td.ReadSupervisorReceipt()
	if err != nil {
		return fmt.Errorf("%w: ended Delegate task %s has no valid supervisor receipt: %w", inspection.ErrGroupMigrationRequired, taskID, err)
	}
	belongsToGeneration, generationErr := PrivateMigrationSupervisorMatches(store.Root, legacy.Supervisor, receipt.SupervisorRef())
	if receipt.NumericTaskID != job.ID || receipt.RootID != store.RootID || generationErr != nil || !belongsToGeneration {
		return fmt.Errorf("%w: ended Delegate task %s supervisor receipt does not match the legacy job", inspection.ErrGroupMigrationRequired, taskID)
	}
	result, err := td.InspectContext(ctx)
	if err != nil {
		return fmt.Errorf("%w: ended Delegate task %s could not be inspected: %w", inspection.ErrGroupMigrationRequired, taskID, err)
	}
	if !result.Terminal() {
		if requirement == endedTaskClosedForMigration && result.Publication == task.PublicationUnknown && !result.SealExists && !result.OutcomeExists {
			return inspectClosedUnpublishedTask(ctx, td)
		}
		return fmt.Errorf("%w: ended Delegate task %s is awaiting publication", inspection.ErrGroupMigrationBusy, taskID)
	}
	return nil
}

// Migration preserves the row and task records. Unlike removal, it may proceed
// after canonical native closure without manufacturing a publication outcome.
func inspectClosedUnpublishedTask(ctx context.Context, td *taskdir.TaskDir) error {
	if _, err := guardian.ReadClosureProof(ctx, td); err != nil {
		if errors.Is(err, guardian.ErrProofPending) {
			return fmt.Errorf("%w: ended Delegate task %s awaits lifetime closure", inspection.ErrGroupMigrationBusy, td.TaskID)
		}
		return fmt.Errorf("%w: ended Delegate task %s closure could not be verified: %w", inspection.ErrGroupMigrationRequired, td.TaskID, err)
	}
	released, err := td.RunnerLeaseReleased()
	if err != nil {
		return fmt.Errorf("%w: ended Delegate task %s runner lease could not be verified: %w", inspection.ErrGroupMigrationRequired, td.TaskID, err)
	}
	if !released {
		return fmt.Errorf("%w: ended Delegate task %s runner remains owned", inspection.ErrGroupMigrationBusy, td.TaskID)
	}
	return nil
}

func validateLegacyEndedInspectionEvidence(ctx context.Context, operation *inspection.Operation, taskID string, job pueue.Job) error {
	result, succeeded, err := operation.ReadWorkerResultContext(ctx)
	managed := operation.Request().Binding.WorkerExitProof == inspection.WorkerExitProofGuardianWaitV1
	if managed && errors.Is(err, inspection.ErrAdmissionExpired) {
		return nil
	}
	if err != nil || (!managed && succeeded != job.Succeeded) || (succeeded && (result == nil || result.Reason != inspection.ResultEligible)) {
		return fmt.Errorf("%w: ended Delegate inspection %s has no valid terminal evidence: %w", inspection.ErrGroupMigrationRequired, taskID, errors.Join(task.ErrEvidenceFault, err))
	}
	return nil
}

func validateLegacyEndedInspectionBinding(ctx context.Context, store *taskdir.Store, legacy inspection.GroupRequestRecord, operation *inspection.Operation, taskID string, job pueue.Job) error {
	request := operation.Request()
	receipt, err := operation.ReceiptContext(ctx)
	if err != nil {
		return fmt.Errorf("%w: ended Delegate inspection %s has no valid supervisor receipt: %w", inspection.ErrGroupMigrationRequired, taskID, err)
	}
	belongsToGeneration, generationErr := PrivateMigrationSupervisorMatches(store.Root, legacy.Supervisor, request.Binding.Supervisor)
	if receipt.NumericTaskID == nil || *receipt.NumericTaskID != job.ID || job.Group != request.Group || (request.Group != "default" && request.Group != legacy.Name) || generationErr != nil || !belongsToGeneration {
		return fmt.Errorf("%w: ended Delegate inspection %s supervisor receipt does not match the legacy job", inspection.ErrGroupMigrationRequired, taskID)
	}
	return nil
}

func recordLegacyEndedInspectionExit(ctx context.Context, operation *inspection.Operation, taskID string, job pueue.Job) error {
	if err := operation.RecordWorkerExitContext(ctx, job.ID, job.Succeeded); err != nil {
		return fmt.Errorf("%w: ended Delegate inspection %s worker observation could not be retained: %w", inspection.ErrGroupMigrationBusy, taskID, err)
	}
	return nil
}

func (c *Cleanup) inspectEndedInspection(ctx context.Context, store *taskdir.Store, legacy inspection.GroupRequestRecord, taskID string, job pueue.Job, validated func(context.Context, *inspection.Operation) error) (resultErr error) {
	if err := validateLegacyEndedInspectionJob(taskID, job); err != nil {
		return err
	}
	if found, err := c.workspace(ctx, store, legacy, taskID, job); found || err != nil {
		return err
	}
	operation, err := inspection.LoadOperationContext(ctx, store, taskID)
	if err != nil {
		return fmt.Errorf("%w: ended Delegate inspection %s could not be reopened: %w", inspection.ErrGroupMigrationRequired, taskID, err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if err := validateLegacyEndedInspectionBinding(ctx, store, legacy, operation, taskID, job); err != nil {
		return err
	}
	if err := c.inspection.ValidateLifetime(ctx, operation, taskID, job); err != nil {
		return err
	}
	if err := validateLegacyEndedInspectionEvidence(ctx, operation, taskID, job); err != nil || validated == nil {
		return err
	}
	return validated(ctx, operation)
}
