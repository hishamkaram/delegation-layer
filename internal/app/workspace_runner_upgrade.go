package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// queuedWorkspaceOperation preserves a helper's immutable content-addressed
// runner. Package upgrades do not remove that state-owned executable. This queue
// transition cannot replace or relabel it as a provider inspection.
func queuedWorkspaceOperation(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, job pueue.Job, id string) (owned bool, resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, id)
	if workspace.OperationAbsent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	request := operation.Request()
	identity := pueue.InspectionIdentity{RootID: store.RootID, TaskID: id, QueueGroup: "default"}
	matches, err := privateMigrationSupervisorMatches(store.Root, supervisor.Binding(), request.Supervisor)
	if err != nil {
		return true, err
	}
	if !matches || job.Label == nil || *job.Label != identity.Label() || job.Group != identity.Group() {
		return true, task.ErrIdentityMismatch
	}
	return true, nil
}
