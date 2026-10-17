package guardian

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// finalizeOwnedRun is the only release boundary for a live guardian. A
// registered scope and its owner lease remain held until both terminal scope
// evidence and the worker completion disposition are durable. Production
// retries use an uncancelled context because no other process can reconstruct
// these handles.
func (g *Guardian) finalizeOwnedRun(parent context.Context) error {
	if g.owner == nil || g.ownerReleased {
		return nil
	}
	if parent == nil {
		return errLifetimeGuardianInvalid
	}
	finalizationCtx := context.WithoutCancel(parent)
	if g.scope == nil {
		return g.releaseOwnerUntilDone(finalizationCtx)
	}
	finalized, err := g.finalizeScopeRegistration(finalizationCtx)
	if err != nil || finalized {
		return err
	}
	waitErr, err := g.persistFinalLifetimeEvidence(finalizationCtx)
	if err != nil {
		return errors.Join(waitErr, err)
	}
	return g.releaseFinalizedScope(finalizationCtx, waitErr)
}

// finalizeScopeRegistration resolves the only state that may precede a
// durable registration. It reports finalized after an unregistered scope was
// safely disposed and both owners were released.
func (g *Guardian) finalizeScopeRegistration(ctx context.Context) (bool, error) {
	backoff := defaultLifetimeFinalizationBackoff
	for {
		if g.unregisteredDisposed {
			if err := g.releaseScopeUntilDone(ctx); err != nil {
				return false, err
			}
			return true, g.releaseOwnerUntilDone(ctx)
		}
		if g.registrationRecorded {
			return false, nil
		}
		attemptCtx, cancel := g.cleanupContext(ctx)
		err := g.resolveCreatedScopeFailure(attemptCtx, g.bootID)
		cancel()
		if err == nil {
			backoff = defaultLifetimeFinalizationBackoff
			continue
		}
		if g.registrationRecorded || g.unregisteredDisposed {
			continue
		}
		g.waitFinalizationRetry(ctx, backoff, g.options.TerminationRecordRetry)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
}

func (g *Guardian) persistFinalLifetimeEvidence(ctx context.Context) (waitErr, recordErr error) {
	if g.terminationCause == "" {
		g.terminationCause = task.LifetimeTerminationRecovery
	}
	if err := g.persistTermination(ctx); err != nil {
		return nil, err
	}
	if g.workerStarted {
		waitErr = g.awaitWorkerWaitUntilDone(ctx)
	}
	if !g.workerWaitRecorded {
		if err := g.persistWorkerWait(ctx); err != nil {
			return waitErr, err
		}
	}
	return waitErr, nil
}

func (g *Guardian) awaitWorkerWaitUntilDone(ctx context.Context) error {
	backoff := defaultLifetimeFinalizationBackoff
	for {
		observed, err := g.awaitWorkerWait(ctx)
		if observed {
			return err
		}
		g.waitFinalizationBackoff(ctx, backoff)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
}

func (g *Guardian) releaseFinalizedScope(ctx context.Context, waitErr error) error {
	if err := g.destroyScopeUntilDone(ctx); err != nil {
		return errors.Join(waitErr, err)
	}
	if err := g.releaseScopeUntilDone(ctx); err != nil {
		return errors.Join(waitErr, err)
	}
	return errors.Join(waitErr, g.releaseOwnerUntilDone(ctx))
}

// destroyScopeUntilDone, releaseScopeUntilDone, and releaseOwnerUntilDone are
// deliberately resident. Once registration is durable, returning from the
// guardian would leave a native scope whose owner can no longer reconstruct
// the exact local handles. Each helper retries only its current phase so a
// completed transition is never repeated.
func (g *Guardian) destroyScopeUntilDone(ctx context.Context) error {
	if g.scope == nil || g.scopeDestroyed {
		return nil
	}
	backoff := defaultLifetimeFinalizationBackoff
	for !g.scopeDestroyed {
		attemptCtx, cancel := g.cleanupContext(ctx)
		err := destroyLifetimeScope(attemptCtx, g.scope)
		cancel()
		if err == nil {
			g.scopeDestroyed = true
			return nil
		}
		g.waitFinalizationBackoff(ctx, backoff)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
	return nil
}

func (g *Guardian) releaseScopeUntilDone(ctx context.Context) error {
	if g.scope == nil || g.scopeReleased {
		return nil
	}
	backoff := defaultLifetimeFinalizationBackoff
	for !g.scopeReleased {
		attemptCtx, cancel := g.cleanupContext(ctx)
		err := releaseLifetimeScope(attemptCtx, g.scope)
		cancel()
		if err == nil {
			g.scopeReleased = true
			return nil
		}
		g.waitFinalizationBackoff(ctx, backoff)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
	return nil
}

func (g *Guardian) releaseOwnerUntilDone(ctx context.Context) error {
	if g.owner == nil || g.ownerReleased {
		return nil
	}
	backoff := defaultLifetimeFinalizationBackoff
	for !g.ownerReleased {
		if err := g.owner.Release(); err == nil {
			g.ownerReleased = true
			return nil
		}
		g.waitFinalizationBackoff(ctx, backoff)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
	return nil
}

func (g *Guardian) terminateAndRecord(ctx context.Context, cause string) error {
	g.fenced = true
	if g.terminationRecorded {
		return nil
	}
	if err := g.freezeTerminationCause(cause); err != nil {
		return err
	}
	if !g.registrationRecorded {
		return fmt.Errorf("%w: registration is not durable", errLifetimeGuardianInvalid)
	}
	if !g.terminationEmptyProven {
		if err := g.fencePendingStart(ctx); err != nil {
			return fmt.Errorf("fence pending lifetime start: %w", err)
		}
		g.authorizedSequence = 0
		if err := g.ensureScopeEmpty(ctx, g.launchRefs); err != nil {
			return fmt.Errorf("terminate lifetime scope: %w", err)
		}
		g.terminationEmptyProven = true
	}
	return g.recordTermination(ctx)
}

func (g *Guardian) recordTermination(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	record, err := g.options.Records.RecordLifetimeTerminationContext(ctx, g.terminationCause)
	if err != nil {
		existing, readErr := g.options.Records.ReadLifetimeTerminationContext(ctx)
		if readErr == nil && g.terminationRecordMatches(existing) {
			g.activeSequence = 0
			g.terminationRecorded = true
			return nil
		}
		if task.RecordAbsent(readErr) {
			readErr = nil
		}
		return errors.Join(fmt.Errorf("record lifetime termination: %w", err), readErr)
	}
	if !g.terminationRecordMatches(record) {
		return fmt.Errorf("%w: termination writer returned invalid record", errLifetimeGuardianInvalid)
	}
	g.activeSequence = 0
	g.terminationRecorded = true
	return nil
}

func (g *Guardian) freezeTerminationCause(cause string) error {
	switch cause {
	case task.LifetimeTerminationCompleted,
		task.LifetimeTerminationBudget,
		task.LifetimeTerminationRequested,
		task.LifetimeTerminationRecovery:
	default:
		return fmt.Errorf("%w: invalid termination cause", errLifetimeGuardianInvalid)
	}
	if g.terminationCause == "" {
		g.terminationCause = cause
	}
	return nil
}

func (g *Guardian) terminationRecordMatches(record *task.LifetimeTerminationRecord) bool {
	return record != nil && record.ScopeEmpty && record.Cause == g.terminationCause
}

func (g *Guardian) persistTermination(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	backoff := defaultLifetimeFinalizationBackoff
	for {
		if g.terminationRecorded {
			return nil
		}
		if g.terminationEmptyProven {
			// Empty-scope proof is durable progress. Once it succeeds, only
			// termination publication may be retried; repeating native proof
			// could mutate ownership after the scope has already been fenced.
			attemptCtx, cancel := g.cleanupContext(ctx)
			err := g.terminateAndRecord(attemptCtx, g.terminationCause)
			cancel()
			if err == nil {
				return nil
			}
			g.waitFinalizationBackoff(ctx, backoff)
			backoff = nextLifetimeFinalizationBackoff(backoff)
			continue
		}

		// A proof batch is intentionally bounded. A fresh cleanup context is
		// created for every attempt, and a new batch is separated by backoff.
		for attempt := 0; attempt < lifetimeTerminationProofAttempts; attempt++ {
			attemptCtx, cancel := g.cleanupContext(ctx)
			err := g.terminateAndRecord(attemptCtx, g.terminationCause)
			cancel()
			if err == nil {
				return nil
			}
			if g.terminationEmptyProven {
				break
			}
		}
		g.waitFinalizationBackoff(ctx, backoff)
		backoff = nextLifetimeFinalizationBackoff(backoff)
	}
}

func (g *Guardian) fencePendingStart(ctx context.Context) error {
	if g.pendingStartFenced {
		return nil
	}
	if g.authorizedSequence == 0 {
		g.pendingStartFenced = true
		return nil
	}
	if g.workerWaitObserved {
		return g.verifyPendingStartFence(ctx)
	}
	if g.workerWait == nil {
		return errors.New("task lifetime guardian: worker wait is unavailable for pending start")
	}
	// Closing the private control channel prevents a healthy worker from
	// completing the pending handoff. The worker owns the exact command Wait
	// and cleans a started-but-unacknowledged command through its scope before
	// exiting. Killing only the worker would leave a provider process group
	// alive and would violate the lifetime owner's stopping boundary.
	if g.codec == nil {
		return errLifetimeGuardianInvalid
	}
	if err := g.codec.Close(); err != nil {
		return fmt.Errorf("close lifetime control channel: %w", err)
	}
	observed, err := g.awaitWorkerWait(ctx)
	if !observed {
		return err
	}
	if err := g.verifyPendingStartFence(ctx); err != nil {
		return err
	}
	g.pendingStartFenced = true
	return nil
}

func (g *Guardian) verifyPendingStartFence(ctx context.Context) error {
	if g.scope == nil {
		return fmt.Errorf("%w: pending-start scope is unavailable", lifetime.ErrUnverifiableLaunch)
	}
	empty, err := g.scope.PendingStartEmptyContext(ctx)
	if err != nil {
		return fmt.Errorf("prove pending lifetime start is empty: %w", err)
	}
	if empty {
		return nil
	}
	if err = finalizeLifetimeScope(ctx, g.scope, g.launchRefs); err != nil {
		return fmt.Errorf("terminate pending lifetime start: %w", err)
	}
	return nil
}

func (g *Guardian) ensureScopeEmpty(ctx context.Context, refs []lifetime.LaunchRef) error {
	return finalizeLifetimeScope(ctx, g.scope, refs)
}
