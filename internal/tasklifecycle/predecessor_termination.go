package tasklifecycle

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func PredecessorTerminated(root string, td *taskdir.TaskDir, req *task.TaskRecord, options SupervisorOptions) error {
	return PredecessorTerminatedContext(context.Background(), root, td, req, options)
}

func PredecessorTerminatedContext(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, options SupervisorOptions) error {
	inspection, err := td.InspectContext(ctx)
	if err != nil {
		return withContinuationFailure(ContinuationPredecessorObservationFailed, err)
	}
	if inspection.Terminal() {
		if err = requireRunnerLeaseRelease(td); err != nil {
			return err
		}
		return releaseTerminalPredecessor(ctx, td, req, inspection)
	}
	if handedOff, handoffErr := releaseHandedOffPredecessor(ctx, td, req); handoffErr != nil {
		return handoffErr
	} else if handedOff {
		return nil
	}
	inspection, err = reconcilePredecessor(ctx, root, td, req, options)
	if err != nil {
		return withContinuationFailure(ContinuationPredecessorReconciliationFailed, err)
	}
	if inspection.Terminal() && inspection.Outcome != nil {
		return releaseTerminalPredecessor(ctx, td, req, inspection)
	}
	if handedOff, handoffErr := releaseHandedOffPredecessor(ctx, td, req); handoffErr != nil {
		return handoffErr
	} else if handedOff {
		return nil
	}
	return withContinuationFailure(ContinuationPredecessorOutcomeUnavailable, errPredecessorOutcomeUnavailable)
}

func releaseTerminalPredecessor(ctx context.Context, td *taskdir.TaskDir, req *task.TaskRecord, inspection *taskdir.TaskInspection) error {
	if inspection == nil || !inspection.Terminal() || inspection.Outcome == nil {
		return withContinuationFailure(ContinuationPredecessorOutcomeUnavailable, errPredecessorOutcomeUnavailable)
	}
	return withContinuationFailure(ContinuationPredecessorSessionHandoffFailed, ReleaseContinuationSessionContext(ctx, td, req, inspection.Outcome))
}

func reconcilePredecessor(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, options SupervisorOptions) (*taskdir.TaskInspection, error) {
	_, meta, spec, metaHash, err := td.PreparedIdentity()
	if err != nil {
		return nil, err
	}
	var response Observation
	observation, err := ReconcileStatus(ctx, root, td, req, meta, spec, metaHash, options, &response)
	if err != nil {
		return nil, err
	}
	if err = UpdateStatusStops(ctx, td, &response, observation); err != nil {
		return nil, err
	}
	if observation.State != pueue.StateEnded {
		return nil, errors.Join(pueue.ErrUnknown, response.MaintenanceError)
	}
	return td.InspectContext(ctx)
}

// releaseHandedOffPredecessor releases a predecessor whose provider turn ended
// without an authoritative outcome. Only a terminated budget stop or a
// closure-proven interruption is handoff proof, and the runner lease must be
// released. A first-turn predecessor owns no session claim to release.
func releaseHandedOffPredecessor(ctx context.Context, td *taskdir.TaskDir, req *task.TaskRecord) (bool, error) {
	records, err := td.ReadStopRecords()
	if err != nil {
		return false, withContinuationFailure(ContinuationPredecessorObservationFailed, err)
	}
	budget := HasTerminatedBudgetStop(records)
	if !budget {
		_, interrupted, interruptionErr := closureInterruption(ctx, td)
		if interruptionErr != nil || !interrupted {
			return false, withContinuationFailure(ContinuationPredecessorObservationFailed, interruptionErr)
		}
	}
	if err = requireRunnerLeaseRelease(td); err != nil {
		return false, err
	}
	if req.PriorSession == nil {
		return true, nil
	}
	if budget {
		return true, withContinuationFailure(ContinuationPredecessorSessionHandoffFailed, td.ReleaseSessionAfterTimeoutContext(ctx, req.Provider, req.PriorSession.ConversationID))
	}
	return true, withContinuationFailure(ContinuationPredecessorSessionHandoffFailed, td.ReleaseSessionAfterLifetimeClosureContext(ctx, req.Provider, req.PriorSession.ConversationID))
}

// closureInterruption is the single eligibility rule for a closure-proven
// interruption: publication is unknown with no seal or outcome, and the
// canonical native lifetime closed. Publication evidence is read first so
// sealed work is collected rather than handed off. Queue state, elapsed
// deadlines, and stop-readiness shortcuts are never closure proof. An
// eligible closure returns its validated proof so callers can read the
// recorded termination cause. Callers keep their own runner-lease checks.
func closureInterruption(ctx context.Context, td *taskdir.TaskDir) (guardian.ClosureProof, bool, error) {
	inspection, err := td.InspectContext(ctx)
	if err != nil {
		return guardian.ClosureProof{}, false, err
	}
	if inspection.Publication != task.PublicationUnknown || inspection.SealExists || inspection.OutcomeExists {
		return guardian.ClosureProof{}, false, nil
	}
	proof, err := guardian.ReadClosureProof(ctx, td)
	if errors.Is(err, guardian.ErrProofPending) {
		return guardian.ClosureProof{}, false, nil
	}
	if err != nil {
		return guardian.ClosureProof{}, false, err
	}
	return proof, true, nil
}

func requireRunnerLeaseRelease(td *taskdir.TaskDir) error {
	released, err := td.RunnerLeaseReleased()
	if err != nil {
		return withContinuationFailure(ContinuationPredecessorRunnerLeaseObservationFailed, err)
	}
	if !released {
		return withContinuationFailure(ContinuationPredecessorRunnerLeaseBusy, task.ErrSessionBusy)
	}
	return nil
}
