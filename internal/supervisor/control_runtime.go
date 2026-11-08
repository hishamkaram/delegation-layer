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
		return NewClient(ctx, root, saved, r.Options, r.Lookup)
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
	selected, err := r.captureLaunchContext(pin)
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
	return r.recoverSaved(ctx, store, saved, source, pueue.ProbeSavedPrivateCompatible)
}

// RecoverSavedTaskObservation borrows the caller's Store and may observe an
// equivalent recovery alias. Callers must determine maintenance eligibility
// separately; the alias grants no submission or cleanup authority.
func (r ControlRuntime) RecoverSavedTaskObservation(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef, source infrastructure.SavedBindingSource) (*pueue.Client, error) {
	return r.recoverSaved(ctx, store, saved, source, pueue.ProbeSavedPrivateTaskObservation)
}

type savedPrivateProbe func(context.Context, string, task.SupervisorRef, pueue.Options, pueue.CompatiblePairSelector) (*pueue.Client, error)

func (r ControlRuntime) recoverSaved(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef, source infrastructure.SavedBindingSource, probe savedPrivateProbe) (*pueue.Client, error) {
	if ctx == nil || store == nil {
		return nil, os.ErrInvalid
	}
	if !pueue.IsPrivateConfig(store.Root, saved.ConfigPath) {
		return NewClient(ctx, store.Root, saved, r.Options, r.Lookup)
	}
	operation, cancel := ControlContextWithParent(ctx, r.Options)
	defer cancel()
	pin, err := store.PinRootContext(operation)
	if err != nil {
		return nil, err
	}
	client, err := r.probeSavedWith(operation, store, pin, saved, probe)
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
	selected, err := r.captureLaunchContext(pin)
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
	client, err = r.probeSavedWith(operation, store, pin, saved, probe)
	if err != nil || client != nil {
		return client, err
	}
	return nil, fmt.Errorf("%w: saved preparation reply has no verified ready supervisor", ErrInfrastructureAuthorityUnavailable)
}

func (r ControlRuntime) probeSavedWith(ctx context.Context, store *taskdir.Store, pin taskdir.RootPin, saved task.SupervisorRef, probe savedPrivateProbe) (*pueue.Client, error) {
	if err := checkInfrastructureTarget(ctx, store, pin); err != nil {
		return nil, err
	}
	client, err := probe(ctx, pin.Root(), saved, r.Options, func(selection context.Context) (string, string, error) {
		if err := selection.Err(); err != nil {
			return "", "", err
		}
		client, daemon, available, err := availableSavedPair(pin.Root(), saved)
		if err != nil || available {
			return client, daemon, errors.Join(err, selection.Err())
		}
		return resolveCompatiblePrivatePair(selection, pin.Root(), r.Lookup)
	})
	if err != nil {
		return client, err
	}
	return client, checkInfrastructureTarget(ctx, store, pin)
}

func (r ControlRuntime) probeAdmission(ctx context.Context, store *taskdir.Store, pin taskdir.RootPin) (*pueue.Client, error) {
	// This read selects transport only. The canonical probe revalidates the
	// published generation and admission gate under its own shared lease.
	published, found, readErr := pueue.ReadPrivateSupervisorBinding(store.Root)
	needsPair := false
	if readErr == nil && found {
		missing, pairErr := pueue.PrivateSupervisorPairMissing(published)
		needsPair = pairErr == nil && (missing || pueue.IsLegacyStateSupervisorClient(store.Root, published))
	}
	var client *pueue.Client
	var err error
	if needsPair {
		clientExecutable, daemonExecutable, selectionErr := resolveCompatiblePrivatePair(ctx, store.Root, r.Lookup)
		if selectionErr != nil && !task.AllErrorCauses(selectionErr, func(cause error) bool { return errors.Is(cause, pueue.ErrCompatiblePairUnavailable) }) {
			return nil, selectionErr
		}
		if selectionErr == nil {
			client, err = ProbePrivateAdmissionSupervisor(ctx, store, clientExecutable, daemonExecutable, r.Options)
		}
	}
	if err == nil && client == nil {
		// Preserve existing cold/refused/incomplete routing and wrapped faults;
		// an optional transport never authorizes a transition by itself.
		client, err = ProbePrivateAdmissionSupervisor(ctx, store, "", "", r.Options)
	}
	if err != nil {
		return client, err
	}
	return client, checkInfrastructureTarget(ctx, store, pin)
}
