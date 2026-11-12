package infrastructurehost

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// RestartOwn preserves the caller-known original reference across every phase.
// The control journal owns daemon transitions; infrastructure owns election,
// exact original-owner closure and each actual Start/Wait. This route owns only
// borrowed readers and parent startup handles, never a task/execution state.
func (r Routing) RestartOwn(ctx context.Context, target *taskdir.Store, selected infrastructure.LaunchContext, from infrastructure.RuntimeRestartReference) (disposition infrastructure.Disposition, report infrastructure.RuntimeRestartReport, resultErr error) {
	pin, selection, err := r.selectRuntimeRestart(ctx, target, selected, from)
	if err != nil {
		return disposition, report, err
	}
	resources := &infrastructureRoutingResources{target: target}
	defer func() { resultErr = resources.finish(resultErr) }()
	resources.reader, err = taskdir.OpenReadOnlyStoreContext(ctx, target.Root, predicate.Default())
	if err != nil {
		return disposition, report, err
	}
	if err = requireInfrastructureRoutingPin(ctx, resources.reader, pin); err != nil {
		return disposition, report, err
	}
	if err = infrastructure.ValidateRuntimeRestartReference(ctx, resources.reader, from); err != nil {
		return disposition, report, err
	}
	route := runtimeRestartRoute{routing: r, resources: resources, pin: pin, selection: selection, from: from}
	outcome, err := route.run(ctx)
	return outcome.Disposition, outcome.RuntimeRestart, errors.Join(err, requireInfrastructureRoutingPin(ctx, resources.reader, pin))
}

func (r Routing) selectRuntimeRestart(ctx context.Context, target *taskdir.Store, selected infrastructure.LaunchContext, from infrastructure.RuntimeRestartReference) (taskdir.RootPin, infrastructureRoutingSelection, error) {
	if ctx == nil || target == nil {
		return taskdir.RootPin{}, infrastructureRoutingSelection{}, os.ErrInvalid
	}
	if _, finite := ctx.Deadline(); !finite {
		return taskdir.RootPin{}, infrastructureRoutingSelection{}, os.ErrInvalid
	}
	if r.invocation == nil {
		return taskdir.RootPin{}, infrastructureRoutingSelection{}, infrastructure.ErrIdentity
	}
	if r.invocation.err != nil {
		return taskdir.RootPin{}, infrastructureRoutingSelection{}, r.invocation.err
	}
	if _, nested := r.invocation.source.Origin(); nested {
		return taskdir.RootPin{}, infrastructureRoutingSelection{}, infrastructure.ErrOwnerRequest
	}
	if err := from.Validate(); err != nil {
		return taskdir.RootPin{}, infrastructureRoutingSelection{}, err
	}
	pin, err := target.PinRootContext(ctx)
	if err != nil {
		return pin, infrastructureRoutingSelection{}, err
	}
	if pin.RootID() != from.RootID {
		return pin, infrastructureRoutingSelection{}, infrastructure.ErrIdentity
	}
	selection, _, err := r.selectPreparation(ctx, pin, selected, infrastructure.InitialOperation{Kind: infrastructure.InitialRuntimeRestart, RestartFrom: from.Startup})
	return pin, selection, err
}

type runtimeRestartRoute struct {
	routing   Routing
	resources *infrastructureRoutingResources
	pin       taskdir.RootPin
	selection infrastructureRoutingSelection
	from      infrastructure.RuntimeRestartReference
}

func (r runtimeRestartRoute) run(ctx context.Context) (infrastructure.PreparationOutcome, error) {
	token, err := infrastructure.FormatRuntimeRestartReference(r.from)
	if err != nil {
		return infrastructure.PreparationOutcome{}, err
	}
	observed, err := pueue.ObservePrivateRuntimeRestart(ctx, r.pin.Root(), pueue.RuntimeRestartReference(token))
	if err != nil {
		if !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, pueue.ErrPrivateRuntimeRestartReferenceNotRetained) }) {
			return infrastructure.PreparationOutcome{}, err
		}
		return r.begin(ctx, err)
	}
	report, err := runtimeRestartReport(observed, r.from)
	if err != nil {
		return infrastructure.PreparationOutcome{}, err
	}
	outcome := infrastructure.PreparationOutcome{Condition: infrastructure.PreparationPending, RuntimeRestart: report}
	if report.Phase == infrastructure.RuntimeRestartReady {
		if _, err = infrastructure.ReadRuntimeRestartSuccessorContext(ctx, r.resources.reader, r.from, infrastructure.RuntimeRestartReference{RootID: r.from.RootID, Startup: report.Successor}); err != nil {
			return infrastructure.PreparationOutcome{}, err
		}
		outcome.Condition, outcome.Startup = infrastructure.PreparationOK, report.Successor
		return outcome, nil
	}
	if report.Successor != (infrastructure.StartupReference{}) {
		prepared, contactErr := r.contact(ctx, report.Successor)
		if prepared.RuntimeRestart == (infrastructure.RuntimeRestartReport{}) {
			prepared = outcome
		}
		return prepared, contactErr
	}
	return r.resumeOriginal(ctx, outcome)
}

func (r runtimeRestartRoute) begin(ctx context.Context, missing error) (infrastructure.PreparationOutcome, error) {
	pending, err := pueue.PrivateRuntimeRestartPending(ctx, r.pin.Root())
	if err != nil || pending {
		return infrastructure.PreparationOutcome{}, errors.Join(missing, err)
	}
	// A new shutdown needs an available replacement installation. This
	// read-only check is no future guarantee and never gates retained retries.
	if _, err = installation.ResolveRunner("", r.routing.defaultRunner); err != nil {
		return infrastructure.PreparationOutcome{}, err
	}
	owner, err := infrastructure.ProbeOwner(ctx, r.resources.reader)
	if err != nil || owner != r.from.Startup {
		return infrastructure.PreparationOutcome{}, errors.Join(infrastructure.ErrOwnerRequest, err)
	}
	outcome, err := r.contact(ctx, owner)
	if err != nil || outcome.RuntimeRestart.Phase != infrastructure.RuntimeRestartDaemonClosed {
		return outcome, err
	}
	return r.replaceOriginal(ctx, outcome)
}

func (r runtimeRestartRoute) resumeOriginal(ctx context.Context, outcome infrastructure.PreparationOutcome) (infrastructure.PreparationOutcome, error) {
	closed, err := infrastructure.ObserveOwnerProcessClosed(ctx, r.resources.reader, r.from.Startup)
	if err != nil && !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, task.ErrLockBusy) }) {
		return outcome, err
	}
	if err == nil && closed.State == infrastructure.OwnerServingReleased {
		if closed.ProcessClosure == infrastructure.OwnerProcessClosed {
			return r.establishSuccessor(ctx, outcome)
		}
		if outcome.RuntimeRestart.Phase != infrastructure.RuntimeRestartDaemonClosed {
			// A retired sole waiter cannot receive the pending shutdown RPC.
			// An outside recovery resident may finish that same recorded daemon;
			// its Start guard still refuses until the original process is closed.
			return r.establishSuccessor(ctx, outcome)
		}
		if _, waitErr := infrastructure.WaitOwnerProcessClosed(ctx, r.resources.reader, r.from.Startup); waitErr != nil {
			return outcome, waitErr
		}
		return r.establishSuccessor(ctx, outcome)
	}
	if outcome.RuntimeRestart.Phase == infrastructure.RuntimeRestartDaemonClosed {
		return r.replaceOriginal(ctx, outcome)
	}
	prepared, err := r.contact(ctx, r.from.Startup)
	if err != nil || prepared.RuntimeRestart.Phase != infrastructure.RuntimeRestartDaemonClosed {
		return prepared, err
	}
	return r.replaceOriginal(ctx, prepared)
}

func (r runtimeRestartRoute) replaceOriginal(ctx context.Context, outcome infrastructure.PreparationOutcome) (infrastructure.PreparationOutcome, error) {
	released, err := infrastructure.StopOwnerServingAt(ctx, r.resources.reader, r.from.Startup)
	if err != nil {
		return outcome, err
	}
	if released.State != infrastructure.OwnerServingReleased || released.Startup != r.from.Startup {
		return outcome, infrastructure.ErrOwnerBusy
	}
	if _, err = infrastructure.WaitOwnerProcessClosed(ctx, r.resources.reader, r.from.Startup); err != nil {
		return outcome, err
	}
	return r.establishSuccessor(ctx, outcome)
}

func (r runtimeRestartRoute) establishSuccessor(ctx context.Context, previous infrastructure.PreparationOutcome) (infrastructure.PreparationOutcome, error) {
	if err := requireInfrastructureRoutingPin(ctx, r.resources.reader, r.pin); err != nil {
		return previous, err
	}
	selection := r.selection
	var err error
	selection.reference, err = infrastructure.StageLaunchContext(ctx, r.resources.target, selection.context)
	if err != nil {
		return previous, err
	}
	established, err := r.routing.establish(ctx, r.resources, r.pin, selection, newInfrastructureEstablishmentHost(r.routing.defaultRunner, selection.initial))
	if err != nil {
		return previous, err
	}
	if err = established.Validate(); err != nil {
		return previous, err
	}
	prepared, err := r.contact(ctx, established.Owner)
	if prepared.RuntimeRestart == (infrastructure.RuntimeRestartReport{}) {
		prepared = previous
	}
	return prepared, err
}

func (r runtimeRestartRoute) contact(ctx context.Context, expected infrastructure.StartupReference) (infrastructure.PreparationOutcome, error) {
	if expected != r.from.Startup {
		accepted, err := infrastructure.ReadRuntimeRestartSuccessorContext(ctx, r.resources.reader, r.from, infrastructure.RuntimeRestartReference{RootID: r.from.RootID, Startup: expected})
		if err != nil || accepted != r.selection.reference {
			return infrastructure.PreparationOutcome{}, errors.Join(infrastructure.ErrIdentity, err)
		}
	}
	selected, err := infrastructure.StageLaunchContext(ctx, r.resources.target, r.selection.context)
	if err != nil {
		return infrastructure.PreparationOutcome{}, err
	}
	// Fresh same-From reconciliation never replays the immutable initial result.
	outcome, err := infrastructure.RequestBoundPreparation(ctx, r.resources.reader, expected, infrastructure.PreparationRequest{Operation: r.selection.initial, Context: selected})
	return outcome, errors.Join(infrastructurePreparationOutcomeError(outcome), err)
}
