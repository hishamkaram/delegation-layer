package supervisor

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func NewClient(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, lookup installation.ExecutableLookup) (*pueue.Client, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.RuntimeClientExecutable == "" {
		selected, err := ResolveSavedSupervisorClient(root, saved, lookup)
		if err != nil {
			return nil, err
		}
		options.RuntimeClientExecutable = selected
	}
	return pueue.NewClient(saved, options)
}

// RecoverPrivateClient borrows the resident's pinned Store. Only a clean
// unavailable-pair fact allows reconciliation and one recovery retry.
func RecoverPrivateClient(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef, options pueue.Options, lookup installation.ExecutableLookup) (*pueue.Client, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store == nil || !pueue.IsPrivateConfig(store.Root, saved.ConfigPath) {
		return nil, pueue.ErrConfiguration
	}
	ctx, cancel := ControlContextWithParent(ctx, options)
	defer cancel()
	if _, err := store.PinRootContext(ctx); err != nil {
		return nil, err
	}
	pending, err := pueue.PrivatePairRestartPending(store.Root)
	if err != nil {
		return nil, &pueue.PrivateRecoveryError{ReasonCode: "state_unreadable", Err: errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, err)}
	}
	if pending {
		if err = reconcileSavedPrivateClient(ctx, store, options, lookup); err != nil {
			return nil, err
		}
	}
	recovery, err := attemptSavedPrivateRecovery(ctx, store.Root, saved, options, lookup)
	if err != nil || !recovery.PairUnavailable() {
		return recovery.Client(), err
	}
	cause := recovery.Cause()
	if err = reconcileSavedPrivateClient(ctx, store, options, lookup); err != nil {
		return nil, errors.Join(cause, err)
	}
	retry, err := attemptSavedPrivateRecovery(ctx, store.Root, saved, options, lookup)
	if err != nil || retry.PairUnavailable() {
		return nil, errors.Join(cause, err, retry.Cause())
	}
	return retry.Client(), nil
}

func reconcileSavedPrivateClient(ctx context.Context, store *taskdir.Store, options pueue.Options, lookup installation.ExecutableLookup) error {
	return pueue.WithBootstrapLockContext(ctx, store.Root, func(lockedContext context.Context) error {
		return ReconcilePublishedSupervisorWithLock(lockedContext, store, options, lookup, false)
	})
}

func attemptSavedPrivateRecovery(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, lookup installation.ExecutableLookup) (pueue.PrivateRecovery, error) {
	clientExecutable, daemonExecutable, err := ResolveRecoveryPrivateSupervisorExecutables(root, saved, lookup)
	if err != nil {
		return pueue.PrivateRecovery{}, err
	}
	return pueue.AttemptPrivateRecovery(ctx, root, saved, clientExecutable, daemonExecutable, options)
}

// NewControlClient proves an external supervisor's saved endpoint ready before
// mutation. Private clients are prepared by the independent resident owner.
func NewControlClient(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, lookup installation.ExecutableLookup) (*pueue.Client, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.RuntimeClientExecutable == "" {
		selected, err := ResolveSavedSupervisorClient(root, saved, lookup)
		if err != nil {
			return nil, err
		}
		options.RuntimeClientExecutable = selected
	}
	direct, err := pueue.BindSaved(ctx, saved, options)
	if err != nil {
		return nil, errors.Join(err, direct.CloseAfterPending(err))
	}
	if err = direct.Ready(ctx); err != nil {
		return nil, errors.Join(err, direct.CloseAfterPending(err))
	}
	return direct, nil
}
