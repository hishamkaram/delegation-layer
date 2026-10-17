package supervisor

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func (c *Cleanup) authorizeCandidates(ctx context.Context, store *taskdir.Store, group inspection.GroupRequestRecord, candidates []privateCleanupCandidate) (plan pueue.PrivateQueueCleanupPlan, resultErr error) {
	decisions := make([]privateCleanupDecision, len(candidates))
	work := make(chan int, len(candidates))
	for index := range candidates {
		work <- index
	}
	close(work)
	workers := min(len(candidates), privateQueueAuthorizationWorkers)
	var groupWorkers sync.WaitGroup
	groupWorkers.Add(workers)
	for range workers {
		go func() {
			defer groupWorkers.Done()
			for index := range work {
				if ctx.Err() != nil {
					return
				}
				candidate := candidates[index]
				if candidate.settling {
					decisions[index].settling = c.inspectSettlingInspection(ctx, store, group, candidate.taskID, candidate.job) == nil
					continue
				}
				decisions[index].remove = c.InspectEndedJob(ctx, store, group, candidate.job) == nil
			}
		}()
	}
	groupWorkers.Wait()
	for index, candidate := range candidates {
		if decisions[index].remove {
			plan.RemoveIDs = append(plan.RemoveIDs, candidate.job.ID)
		}
		plan.Settling = plan.Settling || decisions[index].settling
	}
	return plan, ctx.Err()
}

func (c *Cleanup) inspectSettlingInspection(ctx context.Context, store *taskdir.Store, group inspection.GroupRequestRecord, taskID string, job pueue.Job) (resultErr error) {
	if found, err := c.workspace(ctx, store, group, taskID, job); found || err != nil {
		return err
	}
	operation, err := inspection.LoadOperationContext(ctx, store, taskID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if err = validateLegacyEndedInspectionBinding(ctx, store, group, operation, taskID, job); err != nil {
		return err
	}
	terminal, decisionErr := c.inspection.RecordedTerminal(ctx, operation)
	if !terminal {
		if decisionErr != nil {
			return decisionErr
		}
		return os.ErrNotExist
	}
	return decisionErr
}

// authorizePrivateSuccessfulCleanup selects exact terminal rows using the same
// evidence validator as migration. Active or incomplete records are preserved
// individually; they cannot veto cleanup of an unrelated validated result.
func (c *Cleanup) AuthorizeSuccessfulCleanup(ctx context.Context, stateRoot string, supervisor task.SupervisorRef, jobs []pueue.Job) (plan pueue.PrivateQueueCleanupPlan, resultErr error) {
	if ctx == nil {
		return pueue.PrivateQueueCleanupPlan{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return pueue.PrivateQueueCleanupPlan{}, err
	}
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, stateRoot, predicate.Default())
	if err != nil {
		return pueue.PrivateQueueCleanupPlan{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	group := InspectionGroup(store.RootID, supervisor)
	candidates := privateSuccessfulCleanupCandidates(store.RootID, jobs)
	plan, err = c.authorizeCandidates(ctx, store, group, candidates)
	return plan, err
}
