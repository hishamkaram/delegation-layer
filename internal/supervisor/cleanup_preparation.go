package supervisor

import (
	"context"
	"errors"
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
	group := InspectionGroup(store.RootID, supervisor)
	for _, candidate := range endedInspectionRows(store.RootID, jobs) {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = c.PrepareEndedInspectionEvidence(ctx, store, group, candidate.taskID, candidate.job); err != nil {
			log.Printf("delegate: terminal inspection %s evidence preparation deferred: %v", candidate.taskID, err)
		}
	}
	return ctx.Err()
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
