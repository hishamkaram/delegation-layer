package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Preparation owns durable observations, never removal authority. Its caller
// protects this exact snapshot with queue identity or exclusive bootstrap.
// No supervisor binding, workspace wake, custody filesystem work, or blocking
// lifetime-owner wait is permitted here; a live owner leaves its row for the
// next maintenance pass, and ReleaseInspectionCustody runs after the lease.
func (c *Cleanup) PrepareTerminalEvidence(ctx context.Context, root string, supervisor task.SupervisorRef, jobs []pueue.Job) (resultErr error) {
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	fenced, err := maintenanceRecoveryFenced(ctx, store, supervisor)
	if err != nil {
		return err
	}
	group := InspectionGroup(store.RootID, supervisor)
	var recoveryErr error
	for _, candidate := range endedInspectionRows(store.RootID, jobs) {
		if err = ctx.Err(); err != nil {
			return errors.Join(recoveryErr, err)
		}

		if err = c.prepareMaintenanceCandidate(ctx, store, group, candidate, fenced); err != nil {
			if fenced {
				recoveryErr = joinRuntimeObservation(recoveryErr, err)
			} else {
				c.reportDeferredInspection(candidate.taskID, err)
			}
		}
	}
	if fenced {
		recoveryErr = joinRuntimeObservation(recoveryErr, c.prepareRowlessEvidence(ctx, store, supervisor, jobs))
	}
	return errors.Join(recoveryErr, ctx.Err())
}

// endedInspectionRows selects the ended inspection rows whose label is unique
// in one captured snapshot. Evidence preparation and custody release share it.
func endedInspectionRows(rootID string, jobs []pueue.Job) []privateCleanupCandidate {
	counts := PrivateQueueLabelCounts(jobs)
	rows := make([]privateCleanupCandidate, 0, len(jobs))
	for _, job := range jobs {
		if job.State != pueue.StateEnded || job.Label == nil || counts[*job.Label] != 1 {
			continue
		}
		id, managed, auxiliary, labelErr := LegacyJobTaskID(*job.Label, rootID)
		if labelErr != nil || !managed || !auxiliary {
			continue
		}
		rows = append(rows, privateCleanupCandidate{job: job, taskID: id})
	}
	return rows
}

func (c *Cleanup) PrepareEndedInspectionEvidence(ctx context.Context, store *taskdir.Store, group inspection.GroupRequestRecord, id string, job pueue.Job) (resultErr error) {
	if found, err := c.workspace(ctx, store, group, id, job); found || err != nil {
		return err
	}
	operation, err := inspection.LoadOperationContext(ctx, store, id)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if err = validateLegacyEndedInspectionBinding(ctx, store, group, operation, id, job); err != nil {
		return err
	}
	if err = c.inspection.ValidateLifetime(ctx, operation, id, job); err != nil {
		if recoveryErr := c.inspection.RecoverLifetime(ctx, operation); recoveryErr != nil {
			return recoveryErr
		}
		if err = c.inspection.ValidateLifetime(ctx, operation, id, job); err != nil {
			return err
		}
	}
	if operation.Request().Binding.WorkerExitProof == inspection.WorkerExitProofGuardianWaitV1 {
		return nil
	}
	return recordLegacyEndedInspectionExit(ctx, operation, id, job)
}

func maintenanceRecoveryFenced(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef) (bool, error) {
	if pueue.RequireRuntimeRestartBootstrap(ctx, store.Root) != nil {
		return false, ctx.Err()
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return false, err
	}
	upgrade, found, err := pueue.ObservePrivateUpgrade(ctx, pin, "")
	if err != nil {
		return false, err
	}
	if found {
		if upgrade.Completed || upgrade.Predecessor != binding {
			return false, task.ErrIdentityMismatch
		}
		return true, nil
	}
	return pueue.PrivateRuntimeRestartPending(ctx, store.Root)
}

func (c *Cleanup) prepareRowlessEvidence(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, jobs []pueue.Job) error {
	candidates, err := readRuntimeCandidates(ctx, store)
	if err != nil {
		return err
	}
	labels := PrivateQueueLabelCounts(jobs)
	var resultErr error
	for _, id := range candidates.inspections {
		label := (pueue.InspectionIdentity{RootID: store.RootID, TaskID: id}).Label()
		if labels[label] != 0 {
			continue
		}
		openErr := c.recoverRowlessInspection(ctx, store, id, binding, jobs)
		if openErr != nil {
			resultErr = joinRuntimeObservation(resultErr, &RuntimeMaintenanceError{Kind: "inspection", ID: id, Cause: runtimeQuiescenceError(openErr)})
		}
	}
	for _, id := range candidates.workspaces {
		label := (pueue.InspectionIdentity{RootID: store.RootID, TaskID: id}).Label()
		if labels[label] != 0 {
			continue
		}
		var recoverErr error
		if c.workspaceRecovery == nil {
			recoverErr = task.ErrInvalidPermit
		} else {
			recoverErr = c.workspaceRecovery(ctx, store, id)
		}
		if recoverErr != nil {
			resultErr = joinRuntimeObservation(resultErr, &RuntimeMaintenanceError{Kind: "workspace_operation", ID: id, Cause: runtimeQuiescenceError(recoverErr)})
		}
	}
	return resultErr
}

func (c *Cleanup) reportDeferredInspection(id string, cause error) {
	diagnostic := fmt.Errorf("delegate: terminal inspection %s evidence preparation deferred: %w", id, cause)
	if c.report != nil {
		c.report(diagnostic)
		return
	}
	log.Print(diagnostic)
}

func (c *Cleanup) prepareMaintenanceCandidate(ctx context.Context, store *taskdir.Store, group inspection.GroupRequestRecord, candidate privateCleanupCandidate, fenced bool) error {
	if fenced {
		err := c.InspectEndedJob(ctx, store, group, candidate.job)
		if err == nil || taskdir.CleanupFault(err) || inspectionSettlementPending(err) {
			return err
		}
	}
	return c.PrepareEndedInspectionEvidence(ctx, store, group, candidate.taskID, candidate.job)
}

func (c *Cleanup) recoverRowlessInspection(ctx context.Context, store *taskdir.Store, id string, binding task.SupervisorRef, jobs []pueue.Job) (resultErr error) {
	err := inspectRuntimeInspection(ctx, store, id, true, binding, jobs)
	if err == nil || taskdir.CleanupFault(err) || inspectionSettlementPending(err) {
		return err
	}
	if c.inspection == nil {
		return task.ErrInvalidPermit
	}
	operation, err := inspection.LoadOperationContext(ctx, store, id)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	return c.inspection.RecoverLifetime(ctx, operation)
}

func inspectionSettlementPending(cause error) bool {
	var pending *MaintenanceError
	if !MaintenanceBusy(cause) || !errors.As(cause, &pending) || pending.Assessment.FirstBlocker == nil {
		return false
	}
	blocker := pending.Assessment.FirstBlocker
	return blocker.Kind == "inspection" && blocker.ReasonCode == "inspection_closure_unproven" && blocker.NextAction == "settle_inspection"
}
