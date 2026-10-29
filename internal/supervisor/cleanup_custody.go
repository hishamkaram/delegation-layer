package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ReleaseInspectionCustody releases the custody of each captured ended
// inspection whose canonical terminal evidence still validates. Its caller
// holds neither the generation lease nor queue identity, so it publishes no
// queue-derived observation: worker exits and workspace reconciliation stay in
// identity-protected preparation. A fault retains only its own row through the
// removal gate and is joined into the result; recorded outcomes never change.
func (c *Cleanup) ReleaseInspectionCustody(ctx context.Context, root string, supervisor task.SupervisorRef, jobs []pueue.Job) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if c.custody == nil {
		return nil
	}
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	group := InspectionGroup(store.RootID, supervisor)
	var faults []error
	for _, candidate := range endedInspectionRows(store.RootID, jobs) {
		if err = ctx.Err(); err != nil {
			return errors.Join(append(faults, err)...)
		}
		if err = c.releaseEndedInspectionCustody(ctx, store, group, candidate.taskID, candidate.job); err != nil {
			faults = append(faults, fmt.Errorf("terminal inspection %s custody release: %w", candidate.taskID, err))
		}
	}
	return errors.Join(faults...)
}

func (c *Cleanup) releaseEndedInspectionCustody(ctx context.Context, store *taskdir.Store, group inspection.GroupRequestRecord, id string, job pueue.Job) (resultErr error) {
	operation, err := inspection.LoadOperationContext(ctx, store, id)
	if task.RecordAbsent(err) {
		// A workspace operation owns this auxiliary label and holds no
		// inspection custody.
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if validateLegacyEndedInspectionBinding(ctx, store, group, operation, id, job) != nil ||
		c.inspection.ValidateLifetime(ctx, operation, id, job) != nil {
		// Unproven evidence is not a custody fault. The removal authorizer
		// applies the same validators and retains this row by itself.
		return nil
	}
	return c.custody.PrepareVerifiedLifetime(ctx, operation)
}
