package supervisor

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func PrepareContinuationSuccessor(ctx context.Context, root string, successor task.SupervisorRef, options ContinuationOptions) (task.SupervisorRef, error) {
	if !pueue.IsPrivateConfig(root, successor.ConfigPath) {
		return successor, nil
	}
	ready := successor
	err := pueue.WithBootstrapLockContext(ctx, root, func(lockedContext context.Context) error {
		current, err := RevalidateContinuationSuccessor(lockedContext, root, successor, options.OpenStore)
		if err != nil {
			return err
		}
		successor = current
		ready = current
		legacyBinding, present, err := pueue.ReadPrivateRecoveryBinding(root)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		// The alias is the immediate predecessor for the current successor. The
		// continuation's saved task may point to an older generation after a
		// second migration, so it must not be used to validate this cleanup.
		if task.SameSupervisorIdentity(legacyBinding, successor) {
			return nil
		}
		clientExecutable, daemonExecutable, err := ResolvePrivateSupervisorExecutables(root, options.InitialExecutable)
		if err != nil {
			return err
		}
		currentClient, err := pueue.PreparePrivateWithLock(lockedContext, clientExecutable, daemonExecutable, root, options.Options)
		if err != nil {
			return err
		}
		if !task.SameSupervisorIdentity(currentClient.Binding(), successor) {
			return errors.Join(pueue.ErrBinding, currentClient.Close())
		}
		startErr := currentClient.StartPrivate(lockedContext, root)
		closeErr := currentClient.CloseAfterPending(startErr)
		if startErr != nil {
			return errors.Join(startErr, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		ready = currentClient.Binding()
		return pueue.RetirePrivateRecoveryBindingContext(lockedContext, root, ready)
	})
	return ready, err
}

func RevalidateContinuationSuccessor(ctx context.Context, root string, candidate task.SupervisorRef, openStore StoreOpener) (task.SupervisorRef, error) {
	store, err := openStore(ctx, root)
	if errors.Is(err, os.ErrNotExist) {
		return candidate, nil
	}
	if err != nil {
		return task.SupervisorRef{}, err
	}
	resolution, resolutionErr := ResolveGenerationWithLock(ctx, store, candidate)
	if resolutionErr == nil && resolution.PrivateAuthority {
		resolutionErr = CompletePendingGroupMigrationWithLock(ctx, store, resolution)
	}
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
	if resolution.GroupPresent && !task.SameSupervisorIdentity(resolution.Active, candidate) {
		return task.SupervisorRef{}, task.ErrIdentityMismatch
	}
	return candidate, nil
}
