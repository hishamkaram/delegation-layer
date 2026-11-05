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

var ErrInfrastructureAuthorityUnavailable = errors.New("independent infrastructure authority unavailable")

// OwnerRouting is the application-composed preparation boundary. The caller
// borrows its actual Store throughout contact; only the resident may run the
// canonical start-capable preparation. A reply is not a ready Pueue client.
// Unknown contact outcomes must remain errors, never replay or local fallback.
type OwnerRouting interface {
	EnsurePrivate(context.Context, *taskdir.Store, infrastructure.LaunchContext) (infrastructure.Disposition, error)
	RecoverSaved(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error)
	PrepareContinuation(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error)
	MigrateReferenced(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.MigrationReference) (infrastructure.Disposition, infrastructure.MigrationResult, error)
	MigrateOwn(context.Context, *taskdir.Store, infrastructure.LaunchContext) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error)
}

// ControlRuntime owns invocation configuration, not supervisor state. The root
// journals and canonical preparation own that state; Routing owns independent
// contact. Healthy local observation needs no resident or artifact selection.
type ControlRuntime struct {
	Options   pueue.Options
	Lookup    ExecutableLookup
	Routing   OwnerRouting
	OpenStore StoreOpener
}

// OpenSaved owns only an otherwise unavailable Store handle for this contact.
// Callers already borrowing a Store use RecoverSaved directly.
func (r ControlRuntime) OpenSaved(ctx context.Context, root string, saved task.SupervisorRef, source infrastructure.SavedBindingSource) (client *pueue.Client, resultErr error) {
	if ctx == nil {
		return nil, os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !pueue.IsPrivateConfig(root, saved.ConfigPath) {
		return pueue.NewClient(saved, r.Options)
	}
	if r.OpenStore == nil {
		return nil, ErrInfrastructureAuthorityUnavailable
	}
	operation, cancel := ControlContextWithParent(ctx, r.Options)
	defer cancel()
	store, err := r.OpenStore(operation, root)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !CleanupRetainsStore(resultErr, store) {
			resultErr = errors.Join(resultErr, store.Close())
		}
	}()
	return r.RecoverSaved(operation, store, saved, source)
}

// PreparePrivateAdmission borrows store. It observes locally, routes a missing
// preparation before taking target transition locks, then observes readiness
// again through the same canonical gate. It never enters a local transition.
// Any nonnil client, including alongside an error, remains caller-owned.
func (r ControlRuntime) PreparePrivateAdmission(ctx context.Context, store *taskdir.Store) (*pueue.Client, error) {
	if ctx == nil || store == nil {
		return nil, os.ErrInvalid
	}
	operation, cancel := ControlContextWithParent(ctx, r.Options)
	defer cancel()
	pin, err := store.PinRootContext(operation)
	if err != nil {
		return nil, err
	}
	client, err := r.probeAdmission(operation, store, pin)
	if err != nil || client != nil {
		return client, err
	}
	if r.Routing == nil {
		return nil, ErrInfrastructureAuthorityUnavailable
	}
	selected, err := captureInfrastructureLaunchContext(pin, r.Options)
	if err != nil {
		return nil, err
	}
	disposition, err := r.Routing.EnsurePrivate(operation, store, selected)
	if err != nil {
		return nil, err
	}
	if err = disposition.Validate(); err != nil {
		return nil, err
	}
	if err = checkInfrastructureTarget(operation, store, pin); err != nil {
		return nil, err
	}
	client, err = r.probeAdmission(operation, store, pin)
	if err != nil || client != nil {
		return client, err
	}
	return nil, fmt.Errorf("%w: resident reply has no verified ready supervisor", ErrInfrastructureAuthorityUnavailable)
}

// RecoverSaved observes the exact saved generation before contacting its owner.
// The locator names a durable record which that owner must independently read.
func (r ControlRuntime) RecoverSaved(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef, source infrastructure.SavedBindingSource) (*pueue.Client, error) {
	if ctx == nil || store == nil {
		return nil, os.ErrInvalid
	}
	if !pueue.IsPrivateConfig(store.Root, saved.ConfigPath) {
		return pueue.NewClient(saved, r.Options)
	}
	operation, cancel := ControlContextWithParent(ctx, r.Options)
	defer cancel()
	pin, err := store.PinRootContext(operation)
	if err != nil {
		return nil, err
	}
	client, err := r.probeSaved(operation, store, pin, saved)
	if err != nil || client != nil {
		return client, err
	}
	if err = source.ValidateBinding(saved); err != nil {
		return nil, err
	}
	if source.RootID != pin.RootID() {
		return nil, infrastructure.ErrIdentity
	}
	if r.Routing == nil {
		return nil, ErrInfrastructureAuthorityUnavailable
	}
	selected, err := captureInfrastructureLaunchContext(pin, r.Options)
	if err != nil {
		return nil, err
	}
	disposition, err := r.Routing.RecoverSaved(operation, store, selected, source)
	if err != nil {
		return nil, err
	}
	if err = disposition.Validate(); err != nil {
		return nil, err
	}
	client, err = r.probeSaved(operation, store, pin, saved)
	if err != nil || client != nil {
		return client, err
	}
	return nil, fmt.Errorf("%w: saved preparation reply has no verified ready supervisor", ErrInfrastructureAuthorityUnavailable)
}

func (r ControlRuntime) probeSaved(ctx context.Context, store *taskdir.Store, pin taskdir.RootPin, saved task.SupervisorRef) (*pueue.Client, error) {
	if err := checkInfrastructureTarget(ctx, store, pin); err != nil {
		return nil, err
	}
	client, err := pueue.ProbeSavedPrivateCompatible(ctx, pin.Root(), saved, r.Options, func(selection context.Context) (string, string, error) {
		if err := selection.Err(); err != nil {
			return "", "", err
		}
		client, daemon, available, err := availableSavedPair(pin.Root(), saved)
		if err != nil || available {
			return client, daemon, errors.Join(err, selection.Err())
		}
		client, daemon, err = ResolvePrivateSupervisorExecutables(pin.Root(), r.Lookup)
		if unavailableBundledPair(err) && selection.Err() == nil {
			return "", "", pueue.ErrCompatiblePairUnavailable
		}
		return client, daemon, errors.Join(err, selection.Err())
	})
	if err != nil {
		return client, err
	}
	return client, checkInfrastructureTarget(ctx, store, pin)
}

func (r ControlRuntime) probeAdmission(ctx context.Context, store *taskdir.Store, pin taskdir.RootPin) (*pueue.Client, error) {
	client, err := ProbePrivateAdmissionSupervisor(ctx, store, "", "", r.Options)
	if err != nil {
		return client, err
	}
	return client, checkInfrastructureTarget(ctx, store, pin)
}
