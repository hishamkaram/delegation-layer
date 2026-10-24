package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func NewClient(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, recoverPrivate bool, lookup ExecutableLookup, openStore StoreOpener) (*pueue.Client, error) {
	if recoverPrivate && pueue.IsPrivateConfig(root, saved.ConfigPath) {
		if ctx == nil {
			return nil, context.Canceled
		}
		return recoverPrivateClient(ctx, root, saved, options, lookup, openStore)
	}
	return pueue.NewClient(saved, options)
}

// recoverPrivateClient reconciles the published pair once, and only for the
// explicit PairUnavailable fact. An operational recovery error is returned
// unchanged; a failed reconciliation or retry keeps the first cause.
func recoverPrivateClient(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, lookup ExecutableLookup, openStore StoreOpener) (*pueue.Client, error) {
	recovery, err := attemptSavedPrivateRecovery(ctx, root, saved, options, lookup)
	if err != nil || !recovery.PairUnavailable() {
		return recovery.Client(), err
	}
	cause := recovery.Cause()
	if reconcileErr := ReconcileSaved(ctx, root, options, openStore); reconcileErr != nil {
		return nil, errors.Join(cause, reconcileErr)
	}
	retry, err := attemptSavedPrivateRecovery(ctx, root, saved, options, lookup)
	if err != nil {
		return nil, errors.Join(cause, err)
	}
	if retry.PairUnavailable() {
		return nil, errors.Join(cause, retry.Cause())
	}
	return retry.Client(), nil
}

func attemptSavedPrivateRecovery(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, lookup ExecutableLookup) (pueue.PrivateRecovery, error) {
	clientExecutable, daemonExecutable, err := ResolveRecoveryPrivateSupervisorExecutables(root, saved, lookup)
	if err != nil {
		return pueue.PrivateRecovery{}, err
	}
	return pueue.AttemptPrivateRecovery(ctx, root, saved, clientExecutable, daemonExecutable, options)
}

// NewControlClient proves the task's exact saved endpoint ready before
// a mutating queue command. Only a verified missing saved private pair or a
// clean absent or refused private endpoint may enter saved recovery, where
// completed private migration evidence may recover the current successor
// before the one control command is sent. A failed bind, an uncertain
// observation, a close failure, and a failed control are never retried or
// recovered.
func NewControlClient(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, lookup ExecutableLookup, openStore StoreOpener) (*pueue.Client, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	private := pueue.IsPrivateConfig(root, saved.ConfigPath)
	if private {
		// An upgrade may remove the exact saved pair; recovery then reopens the
		// current generation instead of a direct bind that cannot run.
		missing, err := pueue.PrivateSupervisorPairMissing(saved)
		if err != nil {
			return nil, err
		}
		if missing {
			return NewClient(ctx, root, saved, options, true, lookup, openStore)
		}
	}
	direct, err := pueue.BindSaved(ctx, saved, options)
	if err != nil {
		return nil, errors.Join(err, direct.CloseAfterPending(err))
	}
	if !private {
		if err = direct.Ready(ctx); err != nil {
			return nil, errors.Join(err, direct.CloseAfterPending(err))
		}
		return direct, nil
	}
	observation, err := direct.ObservePrivateEndpoint(ctx)
	if err == nil && observation.State() == pueue.PrivateEndpointReady {
		return direct, nil
	}
	if closeErr := direct.CloseAfterPending(err); err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	if state := observation.State(); state != pueue.PrivateEndpointAbsent && state != pueue.PrivateEndpointRefused {
		return nil, fmt.Errorf("%w: private supervisor endpoint was not observed", pueue.ErrBinding)
	}
	recovered, err := NewClient(ctx, root, saved, options, true, lookup, openStore)
	if err != nil {
		return nil, errors.Join(observation.Cause(), err)
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
