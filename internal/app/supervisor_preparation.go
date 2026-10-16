package app

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
// No supervisor binding, workspace wake, or blocking lifetime-owner wait is
// permitted here; a live owner leaves its row for the next maintenance pass.
func preparePrivateTerminalEvidence(ctx context.Context, root string, supervisor task.SupervisorRef, jobs []pueue.Job) (resultErr error) {
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	counts := privateQueueLabelCounts(jobs)
	group := privateInspectionGroup(store.RootID, supervisor)
	for _, job := range jobs {
		if err = ctx.Err(); err != nil {
			return err
		}
		if job.State != pueue.StateEnded || job.Label == nil || counts[*job.Label] != 1 {
			continue
		}
		id, managed, auxiliary, labelErr := legacyJobTaskID(*job.Label, store.RootID)
		if labelErr != nil || !managed || !auxiliary {
			continue
		}
		if err = prepareEndedInspectionEvidence(ctx, store, group, id, job); err != nil {
			log.Printf("delegate: terminal inspection %s evidence preparation deferred: %v", id, err)
		}
	}
	return ctx.Err()
}

func prepareEndedInspectionEvidence(ctx context.Context, store *taskdir.Store, group inspection.GroupRequestRecord, id string, job pueue.Job) (resultErr error) {
	if found, err := inspectPrivateWorkspaceOperation(ctx, store, group, id, job); found || err != nil {
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
	if err = validateLegacyEndedInspectionLifetime(ctx, operation, id, job); err != nil {
		if recoveryErr := recoverInspectionLifetime(ctx, operation); recoveryErr != nil {
			return recoveryErr
		}
		if err = validateLegacyEndedInspectionLifetime(ctx, operation, id, job); err != nil {
			return err
		}
	}
	if operation.Request().Binding.WorkerExitProof == inspection.WorkerExitProofGuardianWaitV1 {
		return nil
	}
	return recordLegacyEndedInspectionExit(ctx, operation, id, job)
}
