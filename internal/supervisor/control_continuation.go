package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// PrepareContinuation derives the active generation from original predecessor
// metadata on every call. A passive, ready generation needs no owner contact;
// preparation and alias retirement belong exclusively to the resident.
func (r ControlRuntime) PrepareContinuation(ctx context.Context, store *taskdir.Store, source infrastructure.SavedBindingSource) (*pueue.Client, error) {
	if ctx == nil || store == nil {
		return nil, os.ErrInvalid
	}
	operation, cancel := ControlContextWithParent(ctx, r.Options)
	defer cancel()
	original, err := ReadTaskMetadataSupervisor(operation, store, source)
	if err != nil {
		return nil, err
	}
	pin, err := store.PinRootContext(operation)
	if err != nil {
		return nil, err
	}
	client, err := r.probeContinuation(operation, store, pin, original)
	if err != nil || client != nil {
		return client, err
	}
	if r.Routing == nil {
		return nil, ErrInfrastructureAuthorityUnavailable
	}
	selected, err := r.captureLaunchContext(pin)
	if err != nil {
		return nil, err
	}
	disposition, err := r.Routing.PrepareContinuation(operation, store, selected, source)
	if err != nil {
		return nil, err
	}
	if err = disposition.Validate(); err != nil {
		return nil, err
	}
	if err = checkInfrastructureTarget(operation, store, pin); err != nil {
		return nil, err
	}
	original, err = ReadTaskMetadataSupervisor(operation, store, source)
	if err != nil {
		return nil, err
	}
	client, err = r.probeContinuation(operation, store, pin, original)
	if err != nil || client != nil {
		return client, err
	}
	return nil, fmt.Errorf("%w: continuation reply has no verified ready successor", ErrInfrastructureAuthorityUnavailable)
}

func (r ControlRuntime) probeContinuation(ctx context.Context, store *taskdir.Store, pin taskdir.RootPin, original task.SupervisorRef) (client *pueue.Client, resultErr error) {
	entered := false
	resultErr = pueue.WithSupervisorLeaseContext(ctx, pin.Root(), func(locked context.Context) error {
		entered = true
		if err := checkInfrastructureTarget(locked, store, pin); err != nil {
			return err
		}
		generation, err := ResolveGenerationWithLock(locked, store, original)
		if err != nil {
			return err
		}
		if (!generation.PrivateAuthority && !generation.GroupMigrated) || generation.GroupCleanupDue {
			return nil
		}
		alias, present, err := pueue.ReadPrivateRecoveryBinding(pin.Root())
		if err != nil {
			return err
		}
		if present && !task.SameSupervisorIdentity(alias, generation.Active) {
			return nil
		}
		client, err = r.probeSavedWith(locked, store, pin, generation.Active, pueue.ProbeSavedPrivateCompatible)
		return err
	})
	if !entered && errors.Is(resultErr, pueue.ErrPrivateSupervisorNotInitialized) {
		return nil, nil
	}
	return client, resultErr
}
