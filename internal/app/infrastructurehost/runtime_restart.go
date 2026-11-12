package infrastructurehost

import (
	"context"
	"errors"
	"os"
	"reflect"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ValidateEstablishment is shared by outside and resident-produced callbacks.
// It observes the control journal; it does not grant Start or erase its fence.
func (Host) ValidateEstablishment(ctx context.Context, target *taskdir.Store, initial infrastructure.InitialOperation) error {
	return validateRuntimeEstablishment(ctx, target, initial)
}

func validateRuntimeEstablishment(ctx context.Context, target *taskdir.Store, initial infrastructure.InitialOperation) error {
	if ctx == nil || target == nil {
		return os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := initial.Validate(target.RootID); err != nil {
		return err
	}
	pending, err := pueue.PrivateRuntimeRestartPending(ctx, target.Root)
	if err != nil {
		return err
	}
	if !pending {
		if initial.Kind == infrastructure.InitialRuntimeRestart {
			return infrastructure.ErrOwnerIncompatible
		}
		return nil
	}
	if initial.Kind != infrastructure.InitialRuntimeRestart {
		return pueue.ErrPrivateRuntimeRestartPending
	}
	return validatePendingRuntimeReference(ctx, target, initial.RestartFrom)
}

func validatePendingRuntimeReference(ctx context.Context, target *taskdir.Store, from infrastructure.StartupReference) (resultErr error) {
	reference := infrastructure.RuntimeRestartReference{RootID: target.RootID, Startup: from}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, target.Root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	if err = infrastructure.ValidateRuntimeRestartReference(ctx, reader, reference); err != nil {
		return err
	}
	token, err := infrastructure.FormatRuntimeRestartReference(reference)
	if err != nil {
		return err
	}
	observed, err := pueue.ObservePrivateRuntimeRestart(ctx, target.Root, pueue.RuntimeRestartReference(token))
	if err != nil {
		return err
	}
	if observed.Phase == pueue.RuntimeRestartReady {
		return infrastructure.ErrOwnerBusy
	}
	return ctx.Err()
}

func (h Host) PrepareRuntimeRestart(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, from infrastructure.StartupReference, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (infrastructure.Disposition, infrastructure.RuntimeRestartReport, error) {
	// The already-admitted server goroutine owns canonical work and cleanup.
	// Observation expiry must not replace its live same-owner restart with an
	// async retained-worker failure that retires the sole daemon waiter.
	return h.prepareRuntimeRestart(ctx, operation, selected.Clone(), from, guards, wait)
}

func (h Host) prepareRuntimeRestart(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, from infrastructure.StartupReference, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (disposition infrastructure.Disposition, report infrastructure.RuntimeRestartReport, resultErr error) {
	preparation, err := h.openPreparation(ctx, operation, selected, guards, wait)
	defer func() {
		if report.Phase == infrastructure.RuntimeRestartReady {
			disposition = preparation.disposition()
		}
		resultErr = preparation.close(resultErr)
	}()
	if err != nil {
		return disposition, report, err
	}
	reference := infrastructure.RuntimeRestartReference{RootID: preparation.store.RootID, Startup: from}
	current, original, err := runtimeRestartOperationRole(operation, reference)
	if err != nil {
		return disposition, report, err
	}
	if !original {
		if err = operation.Intent().Inputs.Context.ValidateContext(selected, preparation.store.Root, preparation.store.RootID); err != nil {
			return disposition, report, err
		}
	}
	if err = preparation.bindRuntimeRestart(operation, reference, current, guards); err != nil {
		return disposition, report, err
	}
	token, err := infrastructure.FormatRuntimeRestartReference(reference)
	if err != nil {
		return disposition, report, err
	}
	successor, err := infrastructure.FormatRuntimeRestartReference(current)
	if err != nil {
		return disposition, report, err
	}
	var observed pueue.PrivateRuntimeRestartObservation
	resultErr = pueue.WithExistingBootstrapLockContext(ctx, preparation.store.Root, func(locked context.Context) error {
		var callErr error
		observed, callErr = preparation.runtimeRestartLocked(locked, pueue.RuntimeRestartReference(token), pueue.RuntimeRestartReference(successor), original)
		return callErr
	})
	report, err = runtimeRestartReport(observed, reference)
	return disposition, report, errors.Join(resultErr, err)
}

func (o *infrastructurePreparationOwner) runtimeRestartLocked(ctx context.Context, from, successor pueue.RuntimeRestartReference, original bool) (observed pueue.PrivateRuntimeRestartObservation, resultErr error) {
	client, err := pueue.PrepareRuntimeRestartWithLock(ctx, o.store.Root, from, o.options)
	defer func() { resultErr = supervisor.ClosePrivateAdmissionTransition(client, resultErr) }()
	if err != nil {
		return observed, err
	}
	observed, err = client.QuiesceForRuntimeRestart(ctx, o.store.Root, from)
	alreadyReady := observed.Phase == pueue.RuntimeRestartReady
	if err == nil && !original {
		observed, err = client.StartRuntimeRestartSuccessor(ctx, o.store.Root, from, successor)
	}
	if err == nil && observed.Phase == pueue.RuntimeRestartReady && !alreadyReady {
		o.observation.RecordReady()
	}
	return observed, err
}

func runtimeRestartOperationRole(operation infrastructure.Operation, from infrastructure.RuntimeRestartReference) (infrastructure.RuntimeRestartReference, bool, error) {
	if err := operation.Validate(); err != nil {
		return infrastructure.RuntimeRestartReference{}, false, err
	}
	if err := from.Validate(); err != nil {
		return infrastructure.RuntimeRestartReference{}, false, err
	}
	intent := operation.Intent()
	current, err := infrastructure.ReferenceStartup(intent)
	if err != nil || from.RootID != intent.Inputs.RootID {
		return infrastructure.RuntimeRestartReference{}, false, errors.Join(infrastructure.ErrIdentity, err)
	}
	reference := infrastructure.RuntimeRestartReference{RootID: from.RootID, Startup: current}
	if current == from.Startup {
		return reference, true, nil
	}
	initial := intent.Inputs.Initial
	if initial.Kind != infrastructure.InitialRuntimeRestart || initial.RestartFrom != from.Startup {
		return infrastructure.RuntimeRestartReference{}, false, infrastructure.ErrOwnerRequest
	}
	// Validated runtime-purpose input requires accepted outside provenance; the
	// fixed child independently checked its actual birth environment.
	return reference, false, nil
}

func (o *infrastructurePreparationOwner) bindRuntimeRestart(operation infrastructure.Operation, from, current infrastructure.RuntimeRestartReference, guards infrastructure.ResidentGuards) error {
	policy := o.options.PrivateRuntimeRestartAuthorizer
	if policy == nil {
		return pueue.ErrPrivateRuntimeRestartAuthorization
	}
	o.options.PrivateRuntimeRestartAuthorizer = func(ctx context.Context, root string, token pueue.RuntimeRestartReference, binding task.SupervisorRef, jobs []pueue.Job) error {
		reference, err := infrastructure.ParseRuntimeRestartReference(string(token))
		if err != nil || reference != from || root != o.store.Root {
			return errors.Join(infrastructure.ErrIdentity, err)
		}
		if _, _, err = runtimeRestartOperationRole(operation, reference); err != nil {
			return err
		}
		return policy(ctx, root, token, binding, jobs)
	}
	purpose := runtimeRestartPurpose{operation: operation, from: from, current: current}
	check := purpose.authorizeSuccessor
	o.options.PrivateRuntimeRestartSuccessorAuthorizer = func(ctx context.Context, root string, previous, successor pueue.RuntimeRestartReference, binding task.SupervisorRef) error {
		return guards.GuardPrivateGenerationCommit(ctx, root, func() error { return check(ctx, root, previous, successor, binding) })
	}
	fromToken, err := infrastructure.FormatRuntimeRestartReference(from)
	if err != nil {
		return err
	}
	currentToken, err := infrastructure.FormatRuntimeRestartReference(current)
	if err != nil {
		return err
	}
	o.options.DaemonStartGuard = runtimeRestartStartGuard{guard: o.options.DaemonStartGuard, check: func(ctx context.Context, root string) error {
		binding, found, err := pueue.ReadPrivateSupervisorBinding(root)
		if err != nil || !found {
			return errors.Join(pueue.ErrBinding, err)
		}
		return check(ctx, root, pueue.RuntimeRestartReference(fromToken), pueue.RuntimeRestartReference(currentToken), binding)
	}}
	return nil
}

type runtimeRestartStartGuard struct {
	guard infrastructure.DaemonStartGuard
	check func(context.Context, string) error
}

func (g runtimeRestartStartGuard) GuardDaemonStart(ctx context.Context, root string, start func() error) error {
	return g.guard.GuardDaemonStart(ctx, root, func() error {
		if err := g.check(ctx, root); err != nil {
			return err
		}
		return start()
	})
}

type runtimeRestartPurpose struct {
	operation infrastructure.Operation
	from      infrastructure.RuntimeRestartReference
	current   infrastructure.RuntimeRestartReference
}

func (p runtimeRestartPurpose) authorizeSuccessor(ctx context.Context, root string, previous, successor pueue.RuntimeRestartReference, binding task.SupervisorRef) (resultErr error) {
	if err := p.validateSuccessorSubject(root, previous, successor); err != nil {
		return err
	}
	if err := pueue.RequireRuntimeRestartBootstrap(ctx, root); err != nil {
		return err
	}
	if err := validateRuntimeRestartSuccessorBinding(ctx, root, previous, successor, binding); err != nil {
		return err
	}
	return p.requireOriginalClosed(ctx, root)
}

func (p runtimeRestartPurpose) validateSuccessorSubject(root string, previous, successor pueue.RuntimeRestartReference) error {
	if root != p.operation.Intent().Inputs.Root {
		return infrastructure.ErrIdentity
	}
	parsedFrom, err := infrastructure.ParseRuntimeRestartReference(string(previous))
	if err != nil || parsedFrom != p.from {
		return errors.Join(infrastructure.ErrIdentity, err)
	}
	parsedSuccessor, err := infrastructure.ParseRuntimeRestartReference(string(successor))
	if err != nil || parsedSuccessor != p.current {
		return errors.Join(infrastructure.ErrIdentity, err)
	}
	_, original, err := runtimeRestartOperationRole(p.operation, p.from)
	if err != nil || original {
		return errors.Join(infrastructure.ErrOwnerRequest, err)
	}
	return nil
}

func validateRuntimeRestartSuccessorBinding(ctx context.Context, root string, previous, successor pueue.RuntimeRestartReference, binding task.SupervisorRef) error {
	observed, err := pueue.ObservePrivateRuntimeRestart(ctx, root, previous)
	if err != nil {
		return err
	}
	if observed.Binding != binding || observed.Successor != "" && observed.Successor != successor {
		return infrastructure.ErrIdentity
	}
	published, found, err := pueue.ReadPrivateSupervisorBinding(root)
	if err != nil || !found || published != binding {
		return errors.Join(pueue.ErrBinding, err)
	}
	return nil
}

func (p runtimeRestartPurpose) requireOriginalClosed(ctx context.Context, root string) (resultErr error) {
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	if err = infrastructure.ValidateRuntimeRestartReference(ctx, reader, p.from); err != nil {
		return err
	}
	if err = infrastructure.ValidateRuntimeRestartReference(ctx, reader, p.current); err != nil {
		return err
	}
	closed, err := infrastructure.ObserveOwnerProcessClosed(ctx, reader, p.from.Startup)
	if err != nil {
		return err
	}
	if closed.State != infrastructure.OwnerServingReleased || closed.ProcessClosure != infrastructure.OwnerProcessClosed || closed.Startup != p.from.Startup {
		return &pueue.PrivateRuntimeRestartError{ReasonCode: "original_owner_pending", Err: pueue.ErrPrivateRuntimeRestartPending}
	}
	return ctx.Err()
}

func runtimeRestartReport(observed pueue.PrivateRuntimeRestartObservation, expected infrastructure.RuntimeRestartReference) (infrastructure.RuntimeRestartReport, error) {
	if observed == (pueue.PrivateRuntimeRestartObservation{}) {
		return infrastructure.RuntimeRestartReport{}, nil
	}
	from, err := infrastructure.ParseRuntimeRestartReference(string(observed.From))
	if err != nil || from != expected {
		return infrastructure.RuntimeRestartReport{}, errors.Join(infrastructure.ErrIdentity, err)
	}
	report := infrastructure.RuntimeRestartReport{From: from.Startup}
	switch observed.Phase {
	case pueue.RuntimeRestartRequested:
		report.Phase = infrastructure.RuntimeRestartRequested
	case pueue.RuntimeRestartShutdownPending:
		report.Phase = infrastructure.RuntimeRestartShutdownPending
	case pueue.RuntimeRestartDaemonClosed:
		report.Phase = infrastructure.RuntimeRestartDaemonClosed
	case pueue.RuntimeRestartSuccessorAccepted:
		report.Phase = infrastructure.RuntimeRestartSuccessorAccepted
	case pueue.RuntimeRestartReady:
		report.Phase = infrastructure.RuntimeRestartReady
	default:
		return infrastructure.RuntimeRestartReport{}, infrastructure.ErrIdentity
	}
	if observed.Successor != "" {
		successor, parseErr := infrastructure.ParseRuntimeRestartReference(string(observed.Successor))
		if parseErr != nil || successor.RootID != expected.RootID {
			return infrastructure.RuntimeRestartReport{}, errors.Join(infrastructure.ErrIdentity, parseErr)
		}
		report.Successor = successor.Startup
	}
	if err = report.Validate(); err != nil {
		return infrastructure.RuntimeRestartReport{}, err
	}
	return report, nil
}

type runtimeRestartCustodyOwner interface {
	RequireDaemonWaitCustody(context.Context, string, lifetime.ProcessRef) error
}

func (h Host) ObserveRuntimeRestartCustody(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, wait infrastructure.DaemonWaitOwner) (infrastructure.RuntimeRestartCustody, error) {
	if ctx == nil || wait == nil {
		return infrastructure.RuntimeRestartCustodyUnknown, os.ErrInvalid
	}
	if err := operation.Validate(); err != nil {
		return infrastructure.RuntimeRestartCustodyUnknown, err
	}
	inputs := operation.Intent().Inputs
	if err := inputs.Context.ValidateContext(selected, inputs.Root, inputs.RootID); err != nil {
		return infrastructure.RuntimeRestartCustodyUnknown, err
	}
	options := infrastructureSupervisorOptions(operation.Artifacts(), selected, h.callbacks)
	before, err := pueue.ObservePrivateExecutionTarget(ctx, inputs.Root, options)
	if err != nil {
		return infrastructure.RuntimeRestartCustodyUnknown, err
	}
	custody, custodyErr := observeRuntimeWaitCustody(ctx, inputs.Root, before, wait)
	after, err := pueue.ObservePrivateExecutionTarget(ctx, inputs.Root, options)
	if err != nil {
		return infrastructure.RuntimeRestartCustodyUnknown, errors.Join(custodyErr, err)
	}
	if err = compareRuntimeExecutionTargets(before, after); err != nil {
		return infrastructure.RuntimeRestartCustodyUnknown, errors.Join(custodyErr, err)
	}
	return custody, custodyErr
}

func observeRuntimeWaitCustody(ctx context.Context, root string, target pueue.ExecutionTarget, wait infrastructure.DaemonWaitOwner) (infrastructure.RuntimeRestartCustody, error) {
	if target.State() == pueue.ExecutionTargetAbsent {
		return infrastructure.RuntimeRestartCustodyUnavailable, nil
	}
	if target.State() != pueue.ExecutionTargetRunning {
		return infrastructure.RuntimeRestartCustodyUnknown, infrastructure.ErrIdentity
	}
	owner, ok := wait.(runtimeRestartCustodyOwner)
	if !ok {
		return infrastructure.RuntimeRestartCustodyUnavailable, nil
	}
	birth, err := lifetime.NewProcessRef(target.PID(), target.StartToken())
	if err != nil {
		return infrastructure.RuntimeRestartCustodyUnknown, err
	}
	return runtimeWaitCustodyResult(owner.RequireDaemonWaitCustody(ctx, root, birth))
}

func runtimeWaitCustodyResult(err error) (infrastructure.RuntimeRestartCustody, error) {
	if err == nil {
		return infrastructure.RuntimeRestartCustodyAvailable, nil
	}
	retained := task.AnyErrorNode(err, func(node error) bool {
		_, resident := node.(interface{ RetainsResidentCleanup() })
		return resident
	}) || taskdir.CleanupHasRetainedStore(err)
	missing := reflect.ValueOf(infrastructure.ErrDaemonWaitCustody)
	if !retained && task.AllErrorCauses(err, func(cause error) bool {
		value := reflect.ValueOf(cause)
		return value.IsValid() && value.Comparable() && value.Type() == missing.Type() && value.Equal(missing)
	}) {
		return infrastructure.RuntimeRestartCustodyUnavailable, nil
	}
	return infrastructure.RuntimeRestartCustodyUnknown, err
}

func compareRuntimeExecutionTargets(before, after pueue.ExecutionTarget) error {
	if before.State() != after.State() {
		return infrastructure.ErrIdentity
	}
	if before.State() == pueue.ExecutionTargetAbsent {
		return nil
	}
	if before.State() != pueue.ExecutionTargetRunning {
		return infrastructure.ErrIdentity
	}
	first, err := lifetime.NewProcessRef(before.PID(), before.StartToken())
	if err != nil {
		return err
	}
	second, err := lifetime.NewProcessRef(after.PID(), after.StartToken())
	if err != nil {
		return err
	}
	same, err := lifetime.SameProcess(first, second)
	if err != nil || !same {
		return errors.Join(infrastructure.ErrIdentity, err)
	}
	return nil
}
