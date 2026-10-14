package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type lifetimeTerminationWriter func(context.Context, string) (*task.LifetimeTerminationRecord, error)

type lifetimeFinalizationScope interface {
	Terminate(context.Context, []lifetime.LaunchRef) error
	EmptyContext(context.Context, []lifetime.LaunchRef) (bool, error)
}

// finalizeLifetimeScope is the sole terminate-and-prove-empty policy used by
// live guardians and recovery. A concurrent cleanup is accepted only after a
// fresh independent observation proves the scope empty.
func finalizeLifetimeScope(ctx context.Context, scope lifetimeFinalizationScope, refs []lifetime.LaunchRef) error {
	if ctx == nil {
		return lifetime.ErrNilContext
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	empty, err := observeLifetimeEmpty(ctx, scope, refs)
	if err != nil {
		return fmt.Errorf("observe lifetime scope: %w", err)
	}
	if empty {
		return nil
	}
	if terminateErr := scope.Terminate(ctx, refs); terminateErr != nil {
		emptyAfterError, observeErr := observeLifetimeEmpty(ctx, scope, refs)
		if observeErr == nil && emptyAfterError {
			return nil
		}
		return errors.Join(fmt.Errorf("terminate nonempty lifetime scope: %w", terminateErr), observeErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	empty, err = observeLifetimeEmpty(ctx, scope, refs)
	if err != nil {
		return fmt.Errorf("reobserve lifetime scope: %w", err)
	}
	if !empty {
		return errLifetimeGuardianScopeNotEmpty
	}
	return nil
}

// reconcileOwnedLifetime is the single native-lifetime finalization policy for
// ordinary and inspection work. Its caller holds the durable lifetime-owner
// lease for the complete transition.
func reconcileOwnedLifetime(
	ctx context.Context,
	intent *task.LifetimeIntentRecord,
	registration *task.LifetimeRegistrationRecord,
	worker *task.LifetimeWorkerRecord,
	workerWait *task.LifetimeWorkerWaitRecord,
	launches []task.LifetimeLaunchRecord,
	terminated bool,
	recordTermination lifetimeTerminationWriter,
) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if intent == nil && registration == nil {
		return nil
	}
	bootID, err := lifetime.BootID()
	if err != nil {
		return err
	}
	if registration == nil {
		return reconcileUnregisteredLifetimeIntent(ctx, intent, bootID)
	}
	if intent != nil && !lifetimeIntentMatchesRegistrationRecord(intent, registration) {
		return task.ErrIdentityMismatch
	}
	if registration.BootID != bootID {
		return reconcilePreviousBootLifetime(ctx, terminated, recordTermination)
	}
	return reconcileCurrentBootLifetime(ctx, intent, registration, worker, workerWait, launches, terminated, recordTermination)
}

func reconcileCurrentBootLifetime(
	ctx context.Context,
	intent *task.LifetimeIntentRecord,
	registration *task.LifetimeRegistrationRecord,
	worker *task.LifetimeWorkerRecord,
	workerWait *task.LifetimeWorkerWaitRecord,
	launches []task.LifetimeLaunchRecord,
	terminated bool,
	recordTermination lifetimeTerminationWriter,
) (resultErr error) {
	scope, refs, err := openRegisteredLifetime(ctx, intent, registration, launches)
	if terminated && errors.Is(err, lifetime.ErrScopeAbsent) {
		return requireDurableLifetimeWorkerWait(workerWait)
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, releaseLifetimeScopeUncanceled(ctx, scope)) }()

	if !terminated {
		if err = recoverRegisteredLifetime(ctx, scope, refs, registration.Assurance, intent != nil, worker); err != nil {
			return err
		}
		if err = persistRecoveredLifetimeTermination(ctx, recordTermination, task.LifetimeTerminationRecovery); err != nil {
			return err
		}
	}
	if err = requireDurableLifetimeWorkerWait(workerWait); err != nil {
		return err
	}
	return scope.DestroyEmpty()
}

func requireDurableLifetimeWorkerWait(workerWait *task.LifetimeWorkerWaitRecord) error {
	if workerWait == nil {
		return fmt.Errorf("%w: managed worker completion is not durable", task.ErrEvidenceFault)
	}
	return nil
}

func reconcilePreviousBootLifetime(ctx context.Context, terminated bool, recordTermination lifetimeTerminationWriter) error {
	if terminated {
		return nil
	}
	return persistRecoveredLifetimeTermination(ctx, recordTermination, task.LifetimeTerminationPreviousBoot)
}

func reconcileUnregisteredLifetimeIntent(ctx context.Context, intent *task.LifetimeIntentRecord, bootID string) (resultErr error) {
	if intent == nil {
		return nil
	}
	if intent.BootID != bootID {
		return nil
	}
	plan, err := lifetime.NewPlan(lifetime.Kind(intent.ScopeKind), lifetime.Assurance(intent.Assurance), intent.ScopePlan)
	if err != nil {
		return err
	}
	scope, err := lifetime.OpenPrepared(plan)
	if errors.Is(err, lifetime.ErrScopeAbsent) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, releaseLifetimeScopeUncanceled(ctx, scope)) }()
	return disposeUnregisteredLifetimeScope(ctx, scope, plan.Assurance())
}

type lifetimeUnregisteredScope interface {
	EmptyContext(context.Context, []lifetime.LaunchRef) (bool, error)
	DestroyEmpty() error
}

func disposeUnregisteredLifetimeScope(ctx context.Context, scope lifetimeUnregisteredScope, assurance lifetime.Assurance) error {
	if scope == nil {
		return task.ErrEvidenceFault
	}
	if assurance == lifetime.AssuranceCooperative {
		// Commit order guarantees that an intent without registration never
		// authorized a worker launch. Dispose the prepared scope directly; an
		// ownerless cooperative Empty call cannot prove a terminal state.
		return scope.DestroyEmpty()
	}
	empty, err := scope.EmptyContext(ctx, nil)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("%w: unregistered planned scope is populated", lifetime.ErrNotEmpty)
	}
	return scope.DestroyEmpty()
}

func recoverRegisteredLifetime(ctx context.Context, scope lifetimeFinalizationScope, refs []lifetime.LaunchRef, assurance string, hasIntent bool, worker *task.LifetimeWorkerRecord) error {
	if !hasIntent {
		return recoverLifetimeScope(ctx, scope, refs, assurance)
	}
	if assurance != task.LifetimeAssuranceExact {
		// A cooperative scope has no identity-bound way to rule out a launch
		// between registration and worker acknowledgement. Clean durable launch
		// identities and wait for the exact worker when one exists, then fail
		// closed without terminal evidence.
		cleanupErr := recoverLifetimeScope(ctx, scope, refs, assurance)
		var waitErr error
		if worker != nil {
			workerRef, err := lifetime.NewProcessRef(worker.PID, worker.ProcessStart)
			if err != nil {
				waitErr = err
			} else {
				waitErr = lifetime.WaitProcessAbsent(ctx, workerRef)
			}
		}
		return errors.Join(cleanupErr, waitErr)
	}
	if worker == nil {
		empty, err := scope.EmptyContext(ctx, refs)
		if err != nil {
			return err
		}
		if !empty {
			return fmt.Errorf("%w: scope populated before worker acknowledgement", lifetime.ErrNotEmpty)
		}
		return nil
	}
	workerRef, err := lifetime.NewProcessRef(worker.PID, worker.ProcessStart)
	if err != nil {
		return err
	}
	if err = recoverLifetimeScope(ctx, scope, refs, assurance); err != nil {
		return err
	}
	if err = lifetime.WaitProcessAbsent(ctx, workerRef); err != nil {
		return err
	}
	// The worker could have crossed Start while recovery made its first empty
	// observation. Reconcile once more after its exact identity disappears.
	return recoverLifetimeScope(ctx, scope, refs, assurance)
}

func openRegisteredLifetime(ctx context.Context, intent *task.LifetimeIntentRecord, registration *task.LifetimeRegistrationRecord, launches []task.LifetimeLaunchRecord) (*lifetime.Scope, []lifetime.LaunchRef, error) {
	if registration == nil {
		return nil, nil, task.ErrEvidenceFault
	}
	ref, err := lifetime.NewRef(lifetime.Kind(registration.ScopeKind), lifetime.Assurance(registration.Assurance), registration.ScopeID)
	if err != nil {
		return nil, nil, err
	}
	var scope *lifetime.Scope
	if intent == nil {
		scope, err = lifetime.OpenContext(ctx, ref)
	} else {
		plan, planErr := lifetime.NewPlan(lifetime.Kind(intent.ScopeKind), lifetime.Assurance(intent.Assurance), intent.ScopePlan)
		if planErr != nil {
			return nil, nil, planErr
		}
		scope, err = lifetime.OpenRegisteredContext(ctx, plan, ref)
	}
	if err != nil {
		return nil, nil, err
	}
	refs := make([]lifetime.LaunchRef, 0, len(launches))
	for _, launch := range launches {
		launchRef, launchErr := lifetime.NewLaunchRef(launch.PID, launch.ProcessStart, launch.PGID)
		if launchErr != nil {
			return nil, nil, errors.Join(launchErr, releaseLifetimeScopeUncanceled(ctx, scope))
		}
		refs = append(refs, launchRef)
	}
	return scope, refs, nil
}

func persistRecoveredLifetimeTermination(ctx context.Context, writer lifetimeTerminationWriter, cause string) error {
	if writer == nil {
		return task.ErrEvidenceFault
	}
	record, err := writer(ctx, cause)
	if err != nil {
		return err
	}
	if record == nil || !record.ScopeEmpty || record.Cause != cause {
		return task.ErrEvidenceFault
	}
	return nil
}

func lifetimeIntentMatchesRegistrationRecord(intent *task.LifetimeIntentRecord, registration *task.LifetimeRegistrationRecord) bool {
	return intent.RootID == registration.RootID &&
		intent.TaskID == registration.TaskID &&
		intent.OwnerKind == registration.OwnerKind &&
		intent.OwnerSHA256 == registration.OwnerSHA256 &&
		intent.ScopeKind == registration.ScopeKind &&
		intent.Assurance == registration.Assurance &&
		intent.BootID == registration.BootID &&
		intent.Deadline == registration.Deadline
}

func optionalLifetimeIntent(record *task.LifetimeIntentRecord, err error) (*task.LifetimeIntentRecord, error) {
	if task.RecordAbsent(err) {
		return nil, nil
	}
	return record, err
}

func optionalLifetimeRegistration(record *task.LifetimeRegistrationRecord, err error) (*task.LifetimeRegistrationRecord, error) {
	if task.RecordAbsent(err) {
		return nil, nil
	}
	return record, err
}

func optionalLifetimeWorker(record *task.LifetimeWorkerRecord, err error) (*task.LifetimeWorkerRecord, error) {
	if task.RecordAbsent(err) {
		return nil, nil
	}
	return record, err
}

func optionalLifetimeWorkerWait(record *task.LifetimeWorkerWaitRecord, err error) (*task.LifetimeWorkerWaitRecord, error) {
	if task.RecordAbsent(err) {
		return nil, nil
	}
	return record, err
}
