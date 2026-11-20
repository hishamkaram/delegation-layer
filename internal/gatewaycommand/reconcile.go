package gatewaycommand

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var (
	ErrRootContextHistory    = errors.New("root context has no validated matching startup history")
	ErrRootContextTransition = errors.New("root context reconciliation requires a completed supervisor transition")
	ErrRootContextRuntime    = errors.New("root context reconciliation requires the connector and producers offline")
	ErrRootContextEvidence   = errors.New("root context reconciliation evidence is unreadable")
)

func (c invocation) repairContext(ctx context.Context, response Response) (Response, error) {
	if c.arguments.ConfirmContext == "" {
		local, err := c.dependencies.Client.LocalContext(ctx, c.profile)
		if err != nil {
			return response, err
		}
		prepared, err := c.prepareContext(ctx, local)
		if err != nil {
			return response, err
		}
		response.RootContext = &prepared
		response.NextAction = "confirm_root_context_reconciliation"
		return response, nil
	}
	if !c.arguments.OfflineConfirmed {
		return response, ErrOfflineCutoverRequired
	}
	err := gatewayservice.WithProfileLock(ctx, c.profile, func() error {
		var err error
		response, err = c.confirmContext(ctx, response)
		return err
	})
	if err == nil {
		response.NextAction = "run_gateway_doctor"
	}
	return response, err
}

// Caller holds the existing profile-directory coordination lock throughout.
func (c invocation) confirmContext(ctx context.Context, response Response) (Response, error) {
	inactive, err := c.dependencies.Client.RuntimeInactive(ctx, c.profile)
	if err != nil {
		return response, err
	}
	if !inactive {
		return response, ErrRootContextRuntime
	}
	local, err := c.dependencies.Client.LocalContext(ctx, c.profile)
	if err != nil {
		return response, err
	}
	prepared, err := c.prepareContext(ctx, local)
	if err != nil {
		return response, err
	}
	store, err := taskdir.OpenExistingStoreMetadataContext(ctx, local.DelegateRoot, c.dependencies.Predicates)
	if err != nil {
		return response, rootContextNativeError(err)
	}
	gate := c.dependencies.RootContextGate
	if gate == nil {
		gate = rootContextRuntimeGate{store: store, options: pueue.Options{Environment: c.dependencies.Environment, ObservationTimeout: 30 * time.Second, WaitForPendingClose: true}}
	}
	confirmed, confirmErr := store.ConfirmRootContextReconciliation(ctx, local.DelegateRootContext, prepared.Origin, c.arguments.ConfirmContext, true, gate)
	// A later store-close fault does not erase successful confirmation.
	if confirmErr == nil {
		response.RootContext = &confirmed
	}
	return response, rootContextNativeError(errors.Join(confirmErr, store.Close()))
}

func (c invocation) prepareContext(ctx context.Context, local deviceclient.LocalContextInfo) (result taskdir.RootContextReconciliation, resultErr error) {
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, local.DelegateRoot, c.dependencies.Predicates)
	if err != nil {
		return result, rootContextNativeError(err)
	}
	defer func() { resultErr = rootContextNativeError(errors.Join(resultErr, reader.Close())) }()
	origin, found, err := reader.RootContextOrigin(ctx, local.DelegateRootContext)
	if err != nil {
		return result, err
	}
	if !found {
		origin, found, err = infrastructure.FindHistoricalRootContext(ctx, reader, local.DelegateRootContext)
	}
	if err != nil {
		return result, err
	}
	if !found {
		return result, ErrRootContextHistory
	}
	return reader.PrepareRootContextReconciliation(ctx, local.DelegateRootContext, origin)
}

// Native's bootstrap owner excludes new queue admission while taskdir's
// nonblocking maintenance EX freezes record writers during the final callback.
// External producers and old binaries are covered by the explicit attestation.
type rootContextRuntimeGate struct {
	store   *taskdir.Store
	options pueue.Options
}

func (g rootContextRuntimeGate) WithQuiescentRoot(ctx context.Context, expected taskdir.RootPin, apply func(context.Context) error) error {
	if g.store == nil || apply == nil || g.store.Root != expected.Root() {
		return task.ErrInvalidPermit
	}
	return pueue.WithExistingBootstrapLockContext(ctx, expected.Root(), func(locked context.Context) error {
		if err := g.requirePin(locked, expected); err != nil {
			return err
		}
		if err := g.observeQuiescence(locked); err != nil {
			return err
		}
		if err := apply(locked); err != nil {
			return err
		}
		return g.requirePin(locked, expected)
	})
}

func (g rootContextRuntimeGate) requirePin(ctx context.Context, expected taskdir.RootPin) error {
	pin, err := g.store.PinRootContext(ctx)
	if err != nil || !pin.Equal(expected) {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	return nil
}

func (g rootContextRuntimeGate) observeQuiescence(ctx context.Context) error {
	binding, found, err := pueue.ReadPrivateSupervisorBinding(g.store.Root)
	if err != nil {
		return errors.Join(ErrRootContextEvidence, err)
	}
	if !found {
		return ErrRootContextTransition
	}
	complete, err := pueue.PrivateSupervisorMigrationComplete(g.store.Root, binding)
	if err != nil {
		if rootContextTransitionOnly(err) {
			return ErrRootContextTransition
		}
		return errors.Join(ErrRootContextEvidence, err)
	}
	if !complete {
		return ErrRootContextTransition
	}
	return g.observeRuntime(ctx, binding)
}

func (g rootContextRuntimeGate) observeRuntime(ctx context.Context, binding task.SupervisorRef) error {
	client, err := pueue.BindSaved(ctx, binding, g.options)
	if err != nil {
		if client != nil {
			err = errors.Join(err, client.CloseAfterPending(err))
		}
		return errors.Join(ErrRootContextEvidence, err)
	}
	snapshot, observeErr := client.Snapshot(ctx)
	if observeErr == nil {
		observeErr = supervisor.CheckRuntimeQuiescence(ctx, g.store, binding, snapshot.Jobs)
	}
	return rootContextNativeError(errors.Join(observeErr, client.CloseAfterPending(observeErr)))
}

// The original owner classifies the complete error after transport cleanup.
// A new sentinel sibling would invalidate that owner's whole-error decision.
type rootContextRuntimeBusyError struct{ cause error }

func (e *rootContextRuntimeBusyError) Error() string {
	return "native work prevents root context reconciliation"
}
func (e *rootContextRuntimeBusyError) Unwrap() error { return e.cause }

type rootContextChallengeStaleError struct{ cause error }

func (e *rootContextChallengeStaleError) Error() string {
	return "root context reconciliation challenge is stale"
}
func (e *rootContextChallengeStaleError) Unwrap() error { return e.cause }

// Native preparation and confirmation share the same whole-error policy after
// their read/transport/store cleanup. SDK/profile failures retain their owner.
func rootContextNativeError(err error) error {
	if err == nil {
		return nil
	}
	if taskdir.CleanupFault(err) {
		return errors.Join(ErrRootContextEvidence, err)
	}
	if publicInterruption(err) {
		return err
	}
	if supervisor.MaintenanceBusy(err) {
		return &rootContextRuntimeBusyError{cause: err}
	}
	if onlyRootContextCause(err, task.ErrRequestConflict) {
		return &rootContextChallengeStaleError{cause: err}
	}
	for _, known := range []error{task.ErrIdentityMismatch, taskdir.ErrRootContextNotRequired, ErrRootContextHistory, ErrRootContextTransition, ErrRootContextRuntime} {
		if onlyRootContextCause(err, known) {
			return err
		}
	}
	return errors.Join(ErrRootContextEvidence, err)
}

func onlyRootContextCause(err, expected error) bool {
	return task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, expected) })
}

func rootContextTransitionOnly(err error) bool {
	return err != nil && task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, pueue.ErrPrivateUpgradePending) || errors.Is(cause, pueue.ErrPrivateRuntimeRestartPending)
	})
}

func rootContextPublicError(err error) (failure, next string, exitCode int, matched bool) {
	var busy *rootContextRuntimeBusyError
	var stale *rootContextChallengeStaleError
	switch {
	case errors.As(err, &stale):
		if onlyRootContextCause(err, task.ErrRequestConflict) {
			return "root_context_challenge_stale", "prepare_root_context_reconciliation", 1, true
		}
		return "root_context_evidence_unreadable", "stop_and_report", 1, true
	case errors.Is(err, taskdir.ErrRootContextNotRequired):
		if !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, taskdir.ErrRootContextNotRequired) }) {
			return "root_context_evidence_unreadable", "stop_and_report", 1, true
		}
		return "root_context_not_required", "run_gateway_doctor", 1, true
	case errors.As(err, &busy):
		if supervisor.MaintenanceBusy(err) {
			return "root_context_native_work_pending", "wait_for_native_work", 3, true
		}
		return "root_context_evidence_unreadable", "stop_and_report", 1, true
	case errors.Is(err, ErrRootContextEvidence):
		return "root_context_evidence_unreadable", "stop_and_report", 1, true
	case errors.Is(err, ErrRootContextHistory):
		return "root_context_history_unavailable", "stop_and_report", 1, true
	case errors.Is(err, ErrRootContextTransition):
		if !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, ErrRootContextTransition) }) {
			return "root_context_evidence_unreadable", "stop_and_report", 1, true
		}
		return "root_context_transition_pending", "complete_existing_supervisor_transition", 3, true
	case errors.Is(err, ErrRootContextRuntime):
		if !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, ErrRootContextRuntime) }) {
			return "root_context_evidence_unreadable", "stop_and_report", 1, true
		}
		return "root_context_runtime_active", "stop_gateway_service_and_producers", 3, true
	default:
		return "", "", 0, false
	}
}
