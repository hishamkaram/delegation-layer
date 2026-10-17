package supervisor

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func NewClient(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, recoverPrivate bool, initialSupervisorExecutable string, openStore StoreOpener) (*pueue.Client, error) {
	if recoverPrivate && pueue.IsPrivateConfig(root, saved.ConfigPath) {
		if ctx == nil {
			return nil, context.Canceled
		}
		client, err := recoverSavedPrivateSupervisor(ctx, root, saved, options, initialSupervisorExecutable)
		if err == nil || errors.Is(err, pueue.ErrInFlight) || ctx.Err() != nil {
			return client, err
		}
		if !errors.Is(err, pueue.ErrBinding) && !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) && !errors.Is(err, os.ErrNotExist) {
			return client, err
		}
		if client != nil {
			err = errors.Join(err, client.CloseAfterPending(err))
		}
		if reconcileErr := ReconcileSaved(ctx, root, options, openStore); reconcileErr != nil {
			return nil, errors.Join(err, reconcileErr)
		}
		return recoverSavedPrivateSupervisor(ctx, root, saved, options, initialSupervisorExecutable)
	}
	return pueue.NewClient(saved, options)
}

func recoverSavedPrivateSupervisor(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, initialSupervisorExecutable string) (*pueue.Client, error) {
	clientExecutable, daemonExecutable, err := ResolveRecoveryPrivateSupervisorExecutables(root, saved, initialSupervisorExecutable)
	if err != nil {
		return nil, err
	}
	return pueue.RecoverPrivate(ctx, root, saved, clientExecutable, daemonExecutable, options)
}

// NewControlClient proves the task's exact saved endpoint ready before
// a mutating queue command. If that endpoint is unavailable, completed private
// migration evidence may recover the current successor before the one control
// command is sent. An uncertain probe and a failed control are never retried.
func NewControlClient(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, initialSupervisorExecutable string, openStore StoreOpener) (*pueue.Client, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	direct, directErr := pueue.BindSaved(ctx, saved, options)
	if directErr == nil {
		directErr = direct.Ready(ctx)
		if directErr == nil {
			return direct, nil
		}
	}
	if direct != nil {
		directErr = errors.Join(directErr, direct.CloseAfterPending(directErr))
	}
	if errors.Is(directErr, pueue.ErrInFlight) || ctx.Err() != nil || !pueue.IsPrivateConfig(root, saved.ConfigPath) {
		return nil, directErr
	}
	recovered, recoveryErr := NewClient(ctx, root, saved, options, true, initialSupervisorExecutable, openStore)
	if recoveryErr != nil {
		return nil, errors.Join(directErr, recoveryErr)
	}
	return recovered, nil
}

func ReconcileSaved(ctx context.Context, root string, options pueue.Options, openStore StoreOpener) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if _, bounded := ctx.Deadline(); !bounded {
		boundedContext, cancel := context.WithTimeout(ctx, pueue.PrivateControlTimeout(options))
		defer cancel()
		ctx = boundedContext
	}
	store, err := openStore(ctx, root)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	return pueue.WithBootstrapLockContext(ctx, root, func(lockedContext context.Context) error {
		return ReconcilePublishedSupervisorWithLock(lockedContext, store, options, false)
	})
}
