package tasklifecycle

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func ObserveTimeoutContinuation(root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog) (*ContinuationOffer, error) {
	return ObserveTimeoutContinuationContext(context.Background(), root, td, req, meta, catalog)
}

func ObserveTimeoutContinuationContext(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog) (*ContinuationOffer, error) {
	records, err := ReadTimeoutRecords(td)
	if err != nil {
		return nil, err
	}
	return ObserveTimeoutContinuationWithRecordsContext(ctx, root, td, req, meta, records, catalog)
}

func ObserveTimeoutContinuationWithRecords(root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog) (*ContinuationOffer, error) {
	return ObserveTimeoutContinuationWithRecordsContext(context.Background(), root, td, req, meta, records, catalog)
}

func ObserveTimeoutContinuationWithRecordsContext(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog) (*ContinuationOffer, error) {
	runnerReleased, err := td.RunnerLeaseReleased()
	if err != nil {
		return nil, err
	}
	return ObserveTimeoutContinuationWithRunnerContext(ctx, root, td, req, meta, records, catalog, runnerReleased)
}

// ObserveTimeoutContinuationWithRunner observes the automatic continuation
// offer for a terminated budget stop or, without one, for a closure-proven
// unpublished turn whose recorded termination cause decides between a timeout
// and an interruption. Callers check the published outcome first. The offer is
// derived first so its eligibility or session error stays authoritative; only
// a derived offer reads the validated user stop that suppresses automatic
// resume guidance.
func ObserveTimeoutContinuationWithRunner(root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog, runnerReleased bool) (*ContinuationOffer, error) {
	return ObserveTimeoutContinuationWithRunnerContext(context.Background(), root, td, req, meta, records, catalog, runnerReleased)
}

func ObserveTimeoutContinuationWithRunnerContext(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog, runnerReleased bool) (*ContinuationOffer, error) {
	offer, err := observeSilenceContinuation(ctx, root, td, req, meta, catalog, runnerReleased)
	if err != nil {
		return nil, err
	}
	if offer != nil {
		// A validated native closure carries the cause even when the runner sealed
		// before a later queue observation could annotate its stop request.
	} else if HasTerminatedBudgetStop(records) {
		offer, err = observeBudgetContinuation(ctx, root, td, req, meta, records, catalog, runnerReleased)
	} else {
		offer, err = observeInterruptedContinuation(ctx, root, td, req, meta, catalog, runnerReleased)
	}
	if err != nil || offer == nil {
		return nil, err
	}
	cancelled, err := td.UserStopRequestedContext(ctx)
	if err != nil {
		return nil, err
	}
	if !cancelled {
		return offer, nil
	}
	return withUserCancellation(*offer), nil
}

func observeBudgetContinuation(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog, runnerReleased bool) (*ContinuationOffer, error) {
	if req == nil || !HasTerminatedBudgetStop(records) {
		return nil, nil
	}
	source, err := observedSessionSource(td, req)
	if err != nil {
		return nil, err
	}
	launch, err := observeContinuationLaunch(ctx, root, td, meta)
	if err != nil {
		return nil, err
	}
	return TimeoutContinuation(req, records, catalog, source, launch.runner, launch.commandRunner, launch.supervisor, runnerReleased, launch.stateRecorded), nil
}

// observedSessionSource resolves an offer's session through the canonical
// continuation resolver. Only a genuinely absent session means "no source";
// every other fault stays an error so an offer is never built on bad evidence.
func observedSessionSource(td *taskdir.TaskDir, req *task.TaskRecord) (ContinuationSessionSource, error) {
	_, source, err := resolveContinuationSession(td, req)
	if errors.Is(err, errPredecessorSessionAbsent) {
		return "", nil
	}
	return source, err
}

// observeInterruptedContinuation recognizes a turn whose canonical native
// lifetime closed after the runner released its lease, with no seal or
// outcome. The closure's validated termination cause classifies it, so the
// result never depends on a derived stop annotation or an earlier status
// poll. Queue state is never evidence. A held runner lease may still seal.
func observeInterruptedContinuation(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog, runnerReleased bool) (*ContinuationOffer, error) {
	if req == nil || !runnerReleased {
		return nil, nil
	}
	proof, closed, err := closureInterruption(ctx, td)
	if err != nil || !closed {
		return nil, err
	}
	source, err := observedSessionSource(td, req)
	if err != nil {
		return nil, err
	}
	launch, err := observeContinuationLaunch(ctx, root, td, meta)
	if err != nil {
		return nil, err
	}
	return continuationOffer(req, catalog, continuationOfferFacts{cause: closureContinuationCause(proof), source: source, launch: launch, runnerReleased: runnerReleased}), nil
}

// closureContinuationCause reports a timeout only for an explicit budget
// termination; every other recorded cause is an interruption.
func closureContinuationCause(proof guardian.ClosureProof) ContinuationCause {
	if proof.TerminationCause() == task.StopCauseOutputSilence {
		return ContinuationIdleTimedOut
	}
	if proof.TerminationCause() == task.LifetimeTerminationBudget {
		return ContinuationTimedOut
	}
	return ContinuationInterrupted
}

func observeContinuationLaunch(ctx context.Context, root string, td *taskdir.TaskDir, meta *task.MetaRecord) (continuationLaunch, error) {
	if meta == nil {
		return continuationLaunch{}, nil
	}
	ownership, err := supervisorcore.RunnerOwnershipForTaskContext(ctx, root, td, meta)
	if err != nil {
		return continuationLaunch{}, err
	}
	return continuationLaunch{
		runner: meta.RunnerExecutable, commandRunner: TimeoutContinuationCommandRunner(meta, ownership),
		supervisor: savedSupervisor(meta), stateRecorded: meta.EnvironmentRecorded || len(meta.Environment) > 0,
	}, nil
}

func observeSilenceContinuation(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog, runnerReleased bool) (*ContinuationOffer, error) {
	if req == nil || !runnerReleased {
		return nil, nil
	}
	proof, err := guardian.ReadClosureProof(ctx, td)
	if errors.Is(err, guardian.ErrProofPending) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if proof.TerminationCause() != task.StopCauseOutputSilence {
		return nil, nil
	}
	source, err := observedSessionSource(td, req)
	if err != nil {
		return nil, err
	}
	launch, err := observeContinuationLaunch(ctx, root, td, meta)
	if err != nil {
		return nil, err
	}
	offer := continuationOffer(req, catalog, continuationOfferFacts{cause: ContinuationIdleTimedOut, source: source, launch: launch, runnerReleased: runnerReleased})
	inspection, err := td.Inspect()
	if err != nil {
		return nil, err
	}
	if inspection.SealExists && inspection.Outcome == nil {
		offer.Resumable = false
		offer.CommandRunner = ""
		offer.Supervisor = nil
		offer.Reason = "sealed turn must be collected first"
	}
	return offer, nil
}
