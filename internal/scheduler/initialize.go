package scheduler

import (
	"context"
	"errors"
	"io/fs"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func OpenOrInitialize(ctx context.Context, root *taskdir.Store) (contract.Store, error) {
	opened, err := Open(ctx, root)
	if err == nil || !onlyMissing(err) {
		return opened, err
	}
	lease, err := AcquireOwnerLease(ctx, root)
	if err != nil {
		return nil, err
	}
	opened, err = Open(ctx, root)
	switch {
	case err == nil:
		err = opened.Close()
	case onlyMissing(err):
		err = lease.Migrate(ctx)
	}
	if err = errors.Join(err, lease.Close()); err != nil {
		return nil, err
	}
	return Open(ctx, root)
}

// onlyMissing permits initialization only when every cause is absence; joined
// storage or cleanup faults remain operational failures.
func onlyMissing(err error) bool {
	return task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, fs.ErrNotExist) })
}
