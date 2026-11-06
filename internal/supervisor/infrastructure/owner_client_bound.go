package infrastructure

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// RequestBoundPreparation contacts exactly the checked startup once. A nonzero
// selector requests its immutable initial outcome; zero requests fresh work.
// Returned errors describe only local selection, transport, binding or cleanup.
func RequestBoundPreparation(ctx context.Context, store *taskdir.ReadOnlyStore, expected StartupReference, selection PreparationRequest) (PreparationOutcome, error) {
	if err := expected.Validate(); err != nil {
		return PreparationOutcome{}, err
	}
	request, pin, err := currentOwnerRequestAtStartup(ctx, store, preparationAction(selection.Operation), expected)
	if err != nil {
		return PreparationOutcome{}, err
	}
	selection, err = selection.Normalized(pin.RootID(), expected)
	if err != nil {
		return PreparationOutcome{}, err
	}
	request.Context, request.InitialResultFor = selection.Context, selection.InitialResultFor
	request.Source = selection.Operation.Source
	reply, err := contactOwner(ctx, store, pin, request)
	if reply == (ownerReply{}) {
		return PreparationOutcome{}, err
	}
	return reply.preparationOutcome(), err
}

// RequestOtherEstablishment asks A only to establish or match B. The caller
// contacts returned Owner directly; A never executes B's selected operation.
func RequestOtherEstablishment(ctx context.Context, store *taskdir.ReadOnlyStore, expected StartupReference, target Origin, selected LaunchContextReference, initial InitialOperation) (EstablishedOwner, error) {
	if err := expected.Validate(); err != nil {
		return EstablishedOwner{}, err
	}
	if err := initial.Validate(target.RootID()); err != nil {
		return EstablishedOwner{}, err
	}
	request, pin, err := currentOwnerRequestAtStartup(ctx, store, ownerEnsureOther, expected)
	if err != nil {
		return EstablishedOwner{}, err
	}
	request.Target = originRecord{Root: target.Root(), RootID: target.RootID()}
	request.Context, request.Initial = selected, initial.normalized()
	reply, err := classifiedOwnerContact(ctx, store, pin, request)
	return EstablishedOwner{Owner: reply.Startup, InitialResultFor: reply.InitialResultFor}, err
}

func preparationAction(operation InitialOperation) ownerAction {
	switch operation.Kind {
	case "", InitialEnsureOwn:
		return ownerEnsureOwn
	case InitialPrepareContinuation:
		return ownerPrepareContinuation
	case InitialRecoverSaved:
		return ownerRecoverSaved
	case InitialMigrateOwn:
		return ownerMigrateOwn
	default:
		return ""
	}
}

func (r ownerRequest) preparationOperation() InitialOperation {
	switch r.Action {
	case ownerEnsureOwn:
		return InitialOperation{}
	case ownerPrepareContinuation:
		return InitialOperation{Kind: InitialPrepareContinuation, Source: r.Source}
	case ownerRecoverSaved:
		return InitialOperation{Kind: InitialRecoverSaved, Source: r.Source}
	case ownerMigrateOwn:
		return InitialOperation{Kind: InitialMigrateOwn}
	case ownerEnsureOther:
		return r.Initial
	case ownerStatus:
		return InitialOperation{}
	default:
		return InitialOperation{Kind: InitialOperationKind(r.Action)}
	}
}

func (r ownerReply) preparationOutcome() PreparationOutcome {
	return PreparationOutcome{Condition: PreparationCondition(r.Code), Startup: r.Startup, Disposition: r.Disposition, Report: r.Report, Failure: r.Failure}
}
