package supervisor

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// MigrateOwn routes the explicit manual operation after the command's passive
// not-initialized check. It never stages content or acquires transition locks.
func (r ControlRuntime) MigrateOwn(ctx context.Context, store *taskdir.Store) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error) {
	if ctx == nil || store == nil {
		return infrastructure.Disposition{}, infrastructure.ManualMigrationReport{}, os.ErrInvalid
	}
	operation, cancel := ControlContextWithParent(ctx, r.Options)
	defer cancel()
	pin, err := store.PinRootContext(operation)
	if err != nil {
		return infrastructure.Disposition{}, infrastructure.ManualMigrationReport{}, err
	}
	if r.Routing == nil {
		return infrastructure.Disposition{}, infrastructure.ManualMigrationReport{}, ErrInfrastructureAuthorityUnavailable
	}
	selected, err := captureInfrastructureLaunchContext(pin, r.Options)
	if err != nil {
		return infrastructure.Disposition{}, infrastructure.ManualMigrationReport{}, err
	}
	disposition, report, err := r.Routing.MigrateOwn(operation, store, selected)
	err = errors.Join(err, checkInfrastructureTarget(operation, store, pin))
	if err != nil {
		return disposition, report, err
	}
	if err = report.Validate(); err != nil {
		return disposition, report, err
	}
	if err = report.ValidateCondition(infrastructure.PreparationOK); err != nil {
		return disposition, report, err
	}
	if report.State != infrastructure.ManualMigrationNotInitialized || disposition != (infrastructure.Disposition{}) {
		err = disposition.Validate()
	}
	return disposition, report, err
}
