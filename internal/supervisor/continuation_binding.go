package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func RebindContinuation(ctx context.Context, root string, saved task.SupervisorRef, options ContinuationOptions) (task.SupervisorRef, error) {
	if ctx == nil {
		return task.SupervisorRef{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return task.SupervisorRef{}, err
	}
	// The task's supervisor reference is durable evidence. Keep it unchanged;
	// newSupervisorClient reopens the private endpoint through the current
	// state-root pair and adopts a compatible daemon without rewriting task or
	// inspection history. Explicit supervisor repair owns real migrations.
	if !pueue.IsPrivateConfig(root, saved.ConfigPath) {
		return saved, nil
	}
	if err := task.ValidateFreshSupervisorRef(saved); err != nil {
		return task.SupervisorRef{}, err
	}
	return rebindPrivateContinuation(ctx, root, saved, options)
}

func rebindPrivateContinuation(ctx context.Context, root string, saved task.SupervisorRef, options ContinuationOptions) (task.SupervisorRef, error) {
	store, err := options.OpenStore(ctx, root)
	if errors.Is(err, os.ErrNotExist) {
		return saved, nil
	}
	if err != nil {
		return task.SupervisorRef{}, err
	}
	resolution, resolutionErr := ResolveGeneration(ctx, store, saved)
	closeErr := store.Close()
	if resolutionErr != nil {
		return task.SupervisorRef{}, errors.Join(resolutionErr, closeErr)
	}
	if closeErr != nil {
		return task.SupervisorRef{}, closeErr
	}
	if resolution.PrivateAuthority || resolution.GroupMigrated {
		return resolution.Active, nil
	}
	if resolution.GroupPresent {
		return saved, nil
	}
	current, currentErr := resolveCurrentContinuationSupervisor(ctx, root, options)
	if currentErr != nil {
		return task.SupervisorRef{}, currentErr
	}
	return reconcilePrivateContinuationBinding(root, saved, current)
}

// reconcilePrivateContinuationBinding is the single no-journal continuation
// decision. The resolved endpoint and config path must remain the same as the
// predecessor; digest drift is accepted only when the state root has no
// conflicting persisted logical binding, allowing an installation upgrade to
// refresh stale task provenance without opening a second queue.
func reconcilePrivateContinuationBinding(root string, saved, current task.SupervisorRef) (task.SupervisorRef, error) {
	if saved.ConfigPath != current.ConfigPath || saved.Endpoint != current.Endpoint {
		return task.SupervisorRef{}, fmt.Errorf("%w: continuation predecessor uses a changed private supervisor endpoint", pueue.ErrPrivateSupervisorMigrationRequired)
	}
	drift, err := pueue.PrivateSupervisorLogicalDrift(root, current)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	if drift {
		return task.SupervisorRef{}, fmt.Errorf("%w: continuation predecessor uses a changed private supervisor binding", pueue.ErrPrivateSupervisorMigrationRequired)
	}
	return current, nil
}

func resolveCurrentContinuationSupervisor(ctx context.Context, root string, options ContinuationOptions) (task.SupervisorRef, error) {
	clientExecutable, daemonExecutable, err := ResolvePrivateSupervisorExecutables(root, options.InitialExecutable)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	client, err := pueue.PreparePrivate(ctx, clientExecutable, daemonExecutable, root, options.Options)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	binding := client.Binding()
	if err = client.CloseAfterPending(nil); err != nil {
		return task.SupervisorRef{}, err
	}
	return binding, nil
}
