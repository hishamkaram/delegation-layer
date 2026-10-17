package guardian

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type (
	lifetimeTerminationWriter func(context.Context, string) (*task.LifetimeTerminationRecord, error)
	lifetimeAbsenceWriter     func(context.Context, task.LifetimeAbsenceProof, string) (*task.LifetimeWorkerWaitRecord, error)
)

// RecoveryRecords is the record port shared by task and auxiliary recovery.
type RecoveryRecords interface {
	taskdir.LifetimeRecordReader
	RecordLifetimeTerminationContext(context.Context, string) (*task.LifetimeTerminationRecord, error)
	RecordLifetimeWorkerAbsenceContext(context.Context, task.LifetimeAbsenceProof, string) (*task.LifetimeWorkerWaitRecord, error)
}

// ReconcileRecordsOwned resumes the canonical finalizer from durable evidence.
// The caller must hold the record family's lifetime-owner lease throughout.
func ReconcileRecordsOwned(ctx context.Context, records RecoveryRecords) error {
	evidence, err := taskdir.LoadLifetimeEvidenceContext(ctx, records)
	if err != nil {
		return err
	}
	if evidence.Intent == nil && evidence.Registration == nil {
		return task.ErrEvidenceFault
	}
	return reconcileOwnedLifetime(ctx, evidence, records.RecordLifetimeTerminationContext, records.RecordLifetimeWorkerAbsenceContext)
}

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
func reconcileOwnedLifetime(ctx context.Context, evidence task.LifetimeEvidence, recordTermination lifetimeTerminationWriter, recordAbsence lifetimeAbsenceWriter) error {
	if ctx == nil {
		return context.Canceled
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil {
		return err
	}
	if closure.Closed() {
		return nil
	}
	if evidence.Intent == nil && evidence.Registration == nil {
		return nil
	}
	bootID, err := lifetime.BootID()
	if err != nil {
		return err
	}
	if evidence.Registration == nil {
		if err = reconcileUnregisteredLifetimeIntent(ctx, evidence.Intent, bootID); err != nil {
			return err
		}
		proof := task.LifetimeAbsenceUnregisteredDisposed
		if lifetime.CompareBootIDs(evidence.Intent.BootID, bootID) == lifetime.BootDifferent {
			proof = task.LifetimeAbsencePreviousBoot
		}
		return persistRecoveredLifetimeAbsence(ctx, &evidence, proof, bootID, recordAbsence)
	}
	switch lifetime.CompareBootIDs(evidence.Registration.BootID, bootID) {
	case lifetime.BootDifferent:
		if err = reconcilePreviousBootLifetime(ctx, evidence.Termination != nil, func(ctx context.Context, cause string) (*task.LifetimeTerminationRecord, error) {
			record, writeErr := recordTermination(ctx, cause)
			if writeErr == nil {
				evidence.Termination = record
			}
			return record, writeErr
		}); err != nil {
			return err
		}
		return persistRecoveredLifetimeAbsence(ctx, &evidence, task.LifetimeAbsencePreviousBoot, bootID, recordAbsence)
	case lifetime.BootUnknown:
		return fmt.Errorf("%w: stored boot identity cannot prove native lifetime recovery", task.ErrEvidenceFault)
	case lifetime.BootSame:
		return reconcileCurrentBootLifetime(ctx, &evidence, bootID, recordTermination, recordAbsence)
	}
	return task.ErrEvidenceFault
}

func reconcileCurrentBootLifetime(ctx context.Context, evidence *task.LifetimeEvidence, bootID string, recordTermination lifetimeTerminationWriter, recordAbsence lifetimeAbsenceWriter) (resultErr error) {
	if evidence.Termination != nil {
		// A genuine durable empty-scope proof already exists. Do not demand
		// ownerless cooperative scope inspection to observe the exact worker.
		return recoverLifetimeWorkerCompletion(ctx, evidence, bootID, recordAbsence)
	}
	scope, refs, err := openRegisteredLifetime(ctx, evidence.Intent, evidence.Registration, evidence.Launches)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, ReleaseScopeUncanceled(ctx, scope)) }()
	if err = recoverRegisteredLifetime(ctx, scope, refs, evidence.Registration.Assurance, evidence.Intent != nil, evidence.Worker); err != nil {
		return err
	}
	if err = persistRecoveredLifetimeTermination(ctx, func(ctx context.Context, cause string) (*task.LifetimeTerminationRecord, error) {
		record, writeErr := recordTermination(ctx, cause)
		if writeErr == nil {
			evidence.Termination = record
		}
		return record, writeErr
	}, task.LifetimeTerminationRecovery); err != nil {
		return err
	}
	if err = recoverLifetimeWorkerCompletion(ctx, evidence, bootID, recordAbsence); err != nil {
		return err
	}
	return scope.DestroyEmpty()
}

func recoverLifetimeWorkerCompletion(ctx context.Context, evidence *task.LifetimeEvidence, bootID string, recordAbsence lifetimeAbsenceWriter) error {
	if evidence.WorkerWait != nil {
		return requireDurableLifetimeWorkerWait(evidence.WorkerWait)
	}
	if evidence.Worker == nil {
		return fmt.Errorf("%w: scope closure cannot prove an unrecorded worker has exited", task.ErrEvidenceFault)
	}
	worker, err := lifetime.NewProcessRef(evidence.Worker.PID, evidence.Worker.ProcessStart)
	if err != nil {
		return err
	}
	if err = lifetime.WaitProcessAbsent(ctx, worker); err != nil {
		return err
	}
	return persistRecoveredLifetimeAbsence(ctx, evidence, task.LifetimeAbsenceExactWorker, bootID, recordAbsence)
}

func persistRecoveredLifetimeAbsence(ctx context.Context, evidence *task.LifetimeEvidence, proof task.LifetimeAbsenceProof, bootID string, writer lifetimeAbsenceWriter) error {
	if writer == nil {
		return task.ErrEvidenceFault
	}
	completion, err := writer(ctx, proof, bootID)
	if err != nil {
		return err
	}
	if err = task.ValidateLifetimeCompletionBinding(completion, *evidence); err != nil {
		return err
	}
	evidence.WorkerWait = completion
	closure, err := task.ResolveLifetimeClosure(*evidence)
	if err != nil {
		return err
	}
	if !closure.Closed() {
		return task.ErrEvidenceFault
	}
	return nil
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
	switch lifetime.CompareBootIDs(intent.BootID, bootID) {
	case lifetime.BootDifferent:
		return nil
	case lifetime.BootUnknown:
		return fmt.Errorf("%w: stored boot identity cannot prove planned-scope recovery", task.ErrEvidenceFault)
	case lifetime.BootSame:
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
	defer func() { resultErr = errors.Join(resultErr, ReleaseScopeUncanceled(ctx, scope)) }()
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
			return nil, nil, errors.Join(launchErr, ReleaseScopeUncanceled(ctx, scope))
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

// OptionalIntent accepts authoritative absence and preserves unreadable evidence.
func OptionalIntent(record *task.LifetimeIntentRecord, err error) (*task.LifetimeIntentRecord, error) {
	if task.RecordAbsent(err) {
		return nil, nil
	}
	return record, err
}

// OptionalRegistration accepts authoritative absence and preserves unreadable evidence.
func OptionalRegistration(record *task.LifetimeRegistrationRecord, err error) (*task.LifetimeRegistrationRecord, error) {
	if task.RecordAbsent(err) {
		return nil, nil
	}
	return record, err
}
