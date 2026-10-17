package tasklifecycle

import (
	"context"

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
	if timedOut, timeoutErr := releaseTimedOutPredecessor(td, req); timeoutErr != nil {
		return timeoutErr
	} else if timedOut {
		return nil
	}
	inspection, err = reconcilePredecessor(root, td, req, options)
	if err != nil {
		return err
	}
	if supervisorcore.TaskTerminal(inspection) && inspection.Outcome != nil {
		return releaseTerminalPredecessor(td, req, inspection)
	}
	if timedOut, timeoutErr := releaseTimedOutPredecessor(td, req); timeoutErr != nil {
		return timeoutErr
	} else if timedOut {
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
		return nil, pueue.ErrUnknown
	}
	return td.Inspect()
}

func releaseTimedOutPredecessor(td *taskdir.TaskDir, req *task.TaskRecord) (bool, error) {
	records, err := td.ReadStopRecords()
	if err != nil {
		return false, err
	}
	if !HasTerminatedBudgetStop(records) {
		return false, nil
	}
	if err = requireRunnerLeaseRelease(td); err != nil {
		return false, err
	}
	if req.PriorSession == nil {
		return true, nil
	}
	return true, td.ReleaseSessionAfterTimeout(req.Provider, req.PriorSession.ConversationID)
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
