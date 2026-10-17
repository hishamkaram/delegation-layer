package guardian

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

func observeLifetimeEmpty(ctx context.Context, scope lifetimeFinalizationScope, refs []lifetime.LaunchRef) (bool, error) {
	if ctx == nil {
		return false, lifetime.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return scope.EmptyContext(ctx, refs)
}

func destroyLifetimeScope(ctx context.Context, scope Scope) error {
	if ctx == nil {
		return lifetime.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return scope.DestroyEmpty()
}

type lifetimeReleaseScope interface {
	Release() error
}

func releaseLifetimeScope(ctx context.Context, scope lifetimeReleaseScope) error {
	if ctx == nil {
		return lifetime.ErrNilContext
	}
	if scope == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var releaseErr error
	for attempt := 0; attempt < lifetimeScopeReleaseAttempts; attempt++ {
		if err := scope.Release(); err == nil {
			return nil
		} else {
			releaseErr = errors.Join(releaseErr, err)
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(releaseErr, err)
		}
	}
	return releaseErr
}

// ReleaseScopeUncanceled ignores caller cancellation but remains
// synchronous. A caller holding the lifetime-owner lease therefore retains it
// until the local scope handle has been released.
func ReleaseScopeUncanceled(parent context.Context, scope lifetimeReleaseScope) error {
	if parent == nil {
		return lifetime.ErrNilContext
	}
	return releaseLifetimeScope(context.WithoutCancel(parent), scope)
}

func observeLifetimeLaunch(ctx context.Context, scope Scope, pid int) (lifetime.LaunchRef, error) {
	if ctx == nil {
		return lifetime.LaunchRef{}, lifetime.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return lifetime.LaunchRef{}, err
	}
	ref, err := scope.ObserveStarted(pid)
	if err != nil {
		return lifetime.LaunchRef{}, err
	}
	if err = ctx.Err(); err != nil {
		return lifetime.LaunchRef{}, err
	}
	return ref, nil
}

func appendLifetimeLaunchRef(refs []lifetime.LaunchRef, candidate lifetime.LaunchRef) []lifetime.LaunchRef {
	for _, existing := range refs {
		if existing.PID() == candidate.PID() && existing.StartToken() == candidate.StartToken() && existing.PGID() == candidate.PGID() {
			return refs
		}
	}
	return append(refs, candidate)
}
