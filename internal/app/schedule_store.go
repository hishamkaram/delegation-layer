package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// openOrInitializeSchedules creates storage only after confirmed absence, with
// a second observation under the scheduler owner's lease. Explicit create and
// serve reuse this boundary; existing databases are never migrated here.
func openOrInitializeSchedules(ctx context.Context, root *taskdir.Store) (contract.Store, error) {
	opened, err := scheduler.Open(ctx, root)
	if err == nil || !onlyNotExist(err) {
		return opened, err
	}
	lease, err := scheduler.AcquireOwnerLease(ctx, root)
	if err != nil {
		return nil, err
	}
	opened, err = scheduler.Open(ctx, root)
	switch {
	case err == nil:
		err = opened.Close()
	case onlyNotExist(err):
		err = lease.Migrate(ctx)
	}
	if err = errors.Join(err, lease.Close()); err != nil {
		return nil, err
	}
	return scheduler.Open(ctx, root)
}

// readBackScheduleDefinition compares the exact captured immutable identity,
// including CreatedAt and the brief digest, after one uncertain create.
func readBackScheduleDefinition(ctx context.Context, store contract.Store, expected contract.Definition) (bool, error) {
	actual, err := store.GetSchedule(ctx, expected.ScheduleID)
	if err != nil {
		return false, err
	}
	want, err := contract.DefinitionDigests(expected)
	if err != nil {
		return false, err
	}
	got, err := contract.DefinitionDigests(actual.Definition)
	if err != nil {
		return false, err
	}
	if got != want {
		return false, fmt.Errorf("schedule %s: %w", expected.ScheduleID, contract.ErrConflict)
	}
	return true, nil
}
