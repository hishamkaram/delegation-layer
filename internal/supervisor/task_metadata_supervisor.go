package supervisor

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ReadTaskMetadataSupervisor reads the immutable predecessor metadata through
// its canonical validating reader. The locator selects data, never authority;
// the actual Store and its unchanged root pin own the read boundary.
func ReadTaskMetadataSupervisor(ctx context.Context, store *taskdir.Store, source infrastructure.SavedBindingSource) (binding task.SupervisorRef, resultErr error) {
	if ctx == nil || store == nil {
		return binding, os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return binding, err
	}
	if source.Kind != infrastructure.SavedBindingTaskMetadata {
		return binding, infrastructure.ErrIdentity
	}
	if err := source.Validate(); err != nil {
		return binding, err
	}
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return binding, err
	}
	if source.RootID != pin.RootID() {
		return binding, infrastructure.ErrIdentity
	}
	td, err := store.OpenTask(source.OwnerID)
	if err != nil {
		return binding, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	_, meta, _, _, err := td.PreparedIdentity()
	if err != nil {
		return binding, err
	}
	binding = meta.SupervisorConfig
	if err = source.ValidateBinding(binding); err != nil {
		return task.SupervisorRef{}, err
	}
	if !pueue.IsPrivateConfig(pin.Root(), binding.ConfigPath) {
		return task.SupervisorRef{}, infrastructure.ErrIdentity
	}
	if err = checkInfrastructureTarget(ctx, store, pin); err != nil {
		return task.SupervisorRef{}, err
	}
	return binding, nil
}
