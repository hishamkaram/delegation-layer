package tasklifecycle

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func PredecessorTerminated(root string, td *taskdir.TaskDir, req *task.TaskRecord, options SupervisorOptions) error {
	inspection, err := td.Inspect()
	if err != nil {
		return err
	}
	if supervisorcore.TaskTerminal(inspection) {
		if err = requireRunnerLeaseRelease(td); err != nil {
			return err
		}
		return releaseTerminalPredecessor(td, req, inspection)
	}
	if handedOff, handoffErr := releaseHandedOffPredecessor(td, req); handoffErr != nil {
		return handoffErr
	} else if handedOff {
		return nil
	}
	inspection, err = reconcilePredecessor(root, td, req, options)
	if err != nil {
		return err
	}
	if supervisorcore.TaskTerminal(inspection) && inspection.Outcome != nil {
		return releaseTerminalPredecessor(td, req, inspection)
	}
	if handedOff, handoffErr := releaseHandedOffPredecessor(td, req); handoffErr != nil {
		return handoffErr
	} else if handedOff {
		return nil
	}
	return errPredecessorOutcomeUnavailable
}

func releaseTerminalPredecessor(td *taskdir.TaskDir, req *task.TaskRecord, inspection *taskdir.TaskInspection) error {
	if inspection == nil || !supervisorcore.TaskTerminal(inspection) || inspection.Outcome == nil {
		return errPredecessorOutcomeUnavailable
	}
	return ReleaseContinuationSession(td, req, inspection.Outcome)
}

func reconcilePredecessor(root string, td *taskdir.TaskDir, req *task.TaskRecord, options SupervisorOptions) (*taskdir.TaskInspection, error) {
	_, meta, spec, metaHash, err := td.PreparedIdentity()
	if err != nil {
		return nil, err
	}
	var response Observation
	observation, err := ReconcileStatus(context.Background(), root, td, req, meta, spec, metaHash, options, &response)
	if err != nil {
		return nil, err
	}
	if err = UpdateStatusStops(context.Background(), td, &response, observation); err != nil {
		return nil, err
	}
	if observation.State != pueue.StateEnded {
		return nil, errors.Join(pueue.ErrUnknown, response.MaintenanceError)
	}
	return td.Inspect()
}

// releaseHandedOffPredecessor releases a predecessor whose provider turn ended
// without an authoritative outcome. Only a terminated budget stop or a
// closure-proven interruption is handoff proof, and the runner lease must be
// released. A first-turn predecessor owns no session claim to release.
func releaseHandedOffPredecessor(td *taskdir.TaskDir, req *task.TaskRecord) (bool, error) {
	records, err := td.ReadStopRecords()
	if err != nil {
		return false, err
	}
	budget := HasTerminatedBudgetStop(records)
	if !budget {
		interrupted, interruptionErr := closureInterruption(td)
		if interruptionErr != nil || !interrupted {
			return false, interruptionErr
		}
	}
	if err = requireRunnerLeaseRelease(td); err != nil {
		return false, err
	}
	if req.PriorSession == nil {
		return true, nil
	}
	if budget {
		return true, td.ReleaseSessionAfterTimeout(req.Provider, req.PriorSession.ConversationID)
	}
	return true, td.ReleaseSessionAfterLifetimeClosure(req.Provider, req.PriorSession.ConversationID)
}

// closureInterruption is the single eligibility rule for a closure-proven
// interruption: publication is unknown with no seal or outcome, and the
// canonical native lifetime closed. Publication evidence is read first so
// sealed work is collected rather than handed off. Queue state, elapsed
// deadlines, and stop-readiness shortcuts are never closure proof. Callers
// keep their own runner-lease checks.
func closureInterruption(td *taskdir.TaskDir) (bool, error) {
	inspection, err := td.Inspect()
	if err != nil {
		return false, err
	}
	if inspection.Publication != task.PublicationUnknown || inspection.SealExists || inspection.OutcomeExists {
		return false, nil
	}
	_, err = guardian.ReadClosureProof(context.Background(), td)
	if errors.Is(err, guardian.ErrProofPending) {
		return false, nil
	}
	return err == nil, err
}

func requireRunnerLeaseRelease(td *taskdir.TaskDir) error {
	released, err := td.RunnerLeaseReleased()
	if err != nil {
		return err
	}
	if !released {
		return task.ErrSessionBusy
	}
	return nil
}
