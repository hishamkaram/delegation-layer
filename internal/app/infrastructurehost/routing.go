package infrastructurehost

import (
	"context"
	"errors"
	"os"
	"reflect"
	"runtime"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The command owns its immutable invocation capture. Each request owns its
// selected context; canonical root records own the artifacts,
// accepted operation and current owner. No task-selected environment can mint
// an outside starter or replace the original invocation's routing locator.
type Routing struct {
	invocation    *Invocation
	defaultRunner string
}

var _ supervisor.OwnerRouting = Routing{}

func (r Routing) EnsurePrivate(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext) (infrastructure.Disposition, error) {
	outcome, err := r.prepare(ctx, store, selected, infrastructure.InitialOperation{})
	return outcome.Disposition, err
}

func (r Routing) RecoverSaved(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	outcome, err := r.prepare(ctx, store, selected, infrastructure.InitialOperation{Kind: infrastructure.InitialRecoverSaved, Source: source})
	return outcome.Disposition, err
}

func (r Routing) PrepareContinuation(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	outcome, err := r.prepare(ctx, store, selected, infrastructure.InitialOperation{Kind: infrastructure.InitialPrepareContinuation, Source: source})
	return outcome.Disposition, err
}

func (r Routing) MigrateOwn(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error) {
	outcome, err := r.prepare(ctx, store, selected, infrastructure.InitialOperation{Kind: infrastructure.InitialMigrateOwn})
	return outcome.Disposition, outcome.Report, err
}

type infrastructureRoutingSelection struct {
	initial   infrastructure.InitialOperation
	context   infrastructure.LaunchContext
	reference infrastructure.LaunchContextReference
	owner     infrastructure.StartupReference
	probeErr  error
}

func (r Routing) prepare(ctx context.Context, target *taskdir.Store, selected infrastructure.LaunchContext, initial infrastructure.InitialOperation) (outcome infrastructure.PreparationOutcome, resultErr error) {
	if ctx == nil || target == nil {
		return outcome, os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return outcome, err
	}
	if _, finite := ctx.Deadline(); !finite {
		return outcome, os.ErrInvalid
	}
	if r.invocation == nil {
		return outcome, infrastructure.ErrIdentity
	}
	if r.invocation.err != nil {
		return outcome, r.invocation.err
	}
	var selection infrastructureRoutingSelection
	resources := &infrastructureRoutingResources{target: target}
	defer func() { resultErr = resources.finishPreparation(resultErr, selection.probeErr) }()
	pin, err := target.PinRootContext(ctx)
	if err != nil {
		return outcome, err
	}
	selection, host, err := r.selectPreparation(ctx, pin, selected, initial)
	if err != nil {
		return outcome, err
	}
	resources.reader, err = taskdir.OpenReadOnlyStoreContext(ctx, target.Root, predicate.Default())
	if err != nil {
		return outcome, err
	}
	if err = requireInfrastructureRoutingPin(ctx, resources.reader, pin); err != nil {
		return outcome, err
	}
	observed, err := host.probePreparationOwner(ctx, resources.reader)
	selection.owner, selection.probeErr = observed.owner, observed.probeErr
	if err != nil {
		return outcome, err
	}
	selection.reference, err = infrastructure.StageLaunchContext(ctx, target, selected)
	if err != nil {
		return outcome, err
	}
	established, err := r.establish(ctx, resources, pin, selection, host)
	if err != nil {
		return outcome, err
	}
	if err = established.Validate(); err != nil {
		return outcome, err
	}
	return requestInfrastructurePreparation(ctx, selection.initial, established, infrastructurePreparationContact{
		routing: r, resources: resources, selection: selection,
	})
}

func (r Routing) selectPreparation(ctx context.Context, pin taskdir.RootPin, selected infrastructure.LaunchContext, initial infrastructure.InitialOperation) (infrastructureRoutingSelection, infrastructureEstablishmentHost, error) {
	if err := initial.Validate(pin.RootID()); err != nil {
		return infrastructureRoutingSelection{}, infrastructureEstablishmentHost{}, err
	}
	if err := validateOwnerEndpointPath(pin.Root()); err != nil {
		return infrastructureRoutingSelection{}, infrastructureEstablishmentHost{}, err
	}
	digest, err := selected.Digest()
	if err != nil {
		return infrastructureRoutingSelection{}, infrastructureEstablishmentHost{}, err
	}
	reference := infrastructure.LaunchContextReference{SHA256: digest}
	if err = reference.ValidateContext(selected, pin.Root(), pin.RootID()); err != nil || selected.OS != runtime.GOOS {
		return infrastructureRoutingSelection{}, infrastructureEstablishmentHost{}, errors.Join(infrastructure.ErrIdentity, err)
	}
	if origin, found := r.invocation.source.Origin(); found {
		if origin.Root() == pin.Root() && origin.RootID() != pin.RootID() {
			return infrastructureRoutingSelection{}, infrastructureEstablishmentHost{}, infrastructure.ErrIdentity
		}
	}
	host := newInfrastructureEstablishmentHost(r.defaultRunner, initial)
	return infrastructureRoutingSelection{initial: initial, context: selected.Clone(), reference: reference}, host, nil
}

func (r Routing) establish(ctx context.Context, resources *infrastructureRoutingResources, pin taskdir.RootPin, selection infrastructureRoutingSelection, host infrastructureEstablishmentHost) (infrastructure.EstablishedOwner, error) {
	if selection.owner != (infrastructure.StartupReference{}) {
		return infrastructure.EstablishedOwner{Owner: selection.owner}, nil
	}
	origin, found := r.invocation.source.Origin()
	if found {
		if err := origin.ValidateRoot(ctx); err != nil {
			return infrastructure.EstablishedOwner{}, err
		}
		if origin.Root() == pin.Root() {
			owner, err := infrastructure.ProbeOwner(ctx, resources.reader)
			return infrastructure.EstablishedOwner{Owner: owner}, err
		}
		var err error
		resources.origin, err = taskdir.OpenReadOnlyStoreContext(ctx, origin.Root(), predicate.Default())
		if err != nil {
			return infrastructure.EstablishedOwner{}, err
		}
		if resources.origin.Root() != origin.Root() || resources.origin.RootID() != origin.RootID() {
			return infrastructure.EstablishedOwner{}, infrastructure.ErrIdentity
		}
		owner, err := infrastructure.ProbeOwner(ctx, resources.origin)
		if err != nil {
			return infrastructure.EstablishedOwner{}, err
		}
		target, err := infrastructure.NewOrigin(pin.Root(), pin.RootID())
		if err != nil {
			return infrastructure.EstablishedOwner{}, err
		}
		return infrastructure.RequestOtherEstablishment(ctx, resources.origin, owner, target, selection.reference, selection.initial)
	}
	deadline, _ := ctx.Deadline() // prepare requires the caller's finite deadline.
	var err error
	resources.attempt, err = r.invocation.source.EstablishOutside(ctx, resources.target, infrastructure.EstablishmentRequest{
		Context: selection.context, Initial: selection.initial, Deadline: deadline,
	}, host)
	if resources.attempt == nil {
		if contact, ok := infrastructureRoutingContact(err); ok {
			return infrastructure.EstablishedOwner{Owner: contact}, nil
		}
		return infrastructure.EstablishedOwner{}, err
	}
	if err != nil {
		return infrastructure.EstablishedOwner{}, err
	}
	if err = resources.attempt.WaitForContact(ctx); err != nil {
		return infrastructure.EstablishedOwner{}, err
	}
	reference := resources.attempt.Reference()
	return infrastructure.EstablishedOwner{Owner: reference, InitialResultFor: reference}, nil
}

func infrastructureRoutingContact(err error) (infrastructure.StartupReference, bool) {
	if infrastructureRoutingRetainsCleanup(err) {
		return infrastructure.StartupReference{}, false
	}
	var contact *infrastructure.OwnerContactRequiredError
	if !errors.As(err, &contact) || contact == nil || contact.Startup.Validate() != nil {
		return infrastructure.StartupReference{}, false
	}
	clean := task.AllErrorCauses(err, func(cause error) bool {
		var current *infrastructure.OwnerContactRequiredError
		// A virtual As projection cannot certify that this leaf is a clean contact refusal.
		return errors.As(cause, &current) && current != nil &&
			reflect.TypeOf(cause) == reflect.TypeOf(current) && current.Startup == contact.Startup
	})
	return contact.Startup, clean
}

func requireInfrastructureRoutingPin(ctx context.Context, reader *taskdir.ReadOnlyStore, expected taskdir.RootPin) error {
	current, err := reader.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !current.Equal(expected) {
		return infrastructure.ErrIdentity
	}
	return nil
}
