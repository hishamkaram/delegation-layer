package tasklifecycle

import (
	"errors"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func IsPureCollectionPendingError(err error) bool {
	return task.AllErrorCauses(err, func(candidate error) bool {
		return errors.Is(candidate, task.ErrNoSeal) || errors.Is(candidate, task.ErrLockBusy)
	})
}

func IsPureSupervisorUncertaintyError(err error) bool {
	return task.AllErrorCauses(err, func(candidate error) bool {
		return errors.Is(candidate, pueue.ErrUnknown) || errors.Is(candidate, pueue.ErrInFlight)
	})
}

func HasTerminatedBudgetStop(records []taskdir.StopRecord) bool {
	for _, record := range records {
		if record.Request != nil && record.Request.Cause == "budget" && record.Observation != nil && record.Observation.Terminated {
			return true
		}
	}
	return false
}

func timeoutContinuationReason(mode commonprovider.ContinuationMode, session *task.ProviderRefRecord, runnerReleased bool, runner string, launchStateRecorded bool) string {
	switch mode {
	case commonprovider.ContinuationUnsupported:
		return "provider does not advertise continuation"
	case commonprovider.ContinuationCheckpoint:
		return "checkpoint continuation is not implemented"
	case commonprovider.ContinuationNative:
		if session == nil {
			return "provider session identity is unavailable"
		}
		if !launchStateRecorded {
			return "launch environment is unavailable for this task"
		}
		if runner == "" {
			return "runner executable is unavailable for this task"
		}
		if !runnerReleased {
			return "runner lease is still active"
		}
	}
	return ""
}

func TimeoutContinuationCommandRunner(meta *task.MetaRecord, ownership string) string {
	if meta == nil || ownership == task.RunnerOwnershipManaged {
		return ""
	}
	return meta.RunnerExecutable
}

func InteractionWaiting(state *task.InteractionState) bool {
	return state != nil && !state.Closed && len(state.PendingRequests) > 0
}

func DescribeSupervisor(observation pueue.Observation) *SupervisorObservation {
	var id *int64
	if observation.Matched {
		id = &observation.NumericTaskID
	} else {
		id = copyTaskID(observation.Identity.NumericTaskID)
	}
	return &SupervisorObservation{Matched: observation.Matched, State: string(observation.State), NumericTaskID: id}
}

func LivenessFromQueue(state pueue.State) task.Liveness {
	switch state {
	case pueue.StateUnknown:
		return task.LivenessUndetermined
	case pueue.StateQueued, pueue.StateStashed:
		return task.LivenessRunning
	case pueue.StateRunning:
		return task.LivenessRunning
	case pueue.StateEnded:
		return task.LivenessEnded
	default:
		return task.LivenessUndetermined
	}
}

func savedSupervisor(meta *task.MetaRecord) *task.SupervisorRef {
	if meta == nil {
		return nil
	}
	supervisor := meta.SupervisorConfig
	return &supervisor
}

func budgetStopNeedsObservation(records []taskdir.StopRecord) bool {
	for _, record := range records {
		if record.Request != nil && record.Request.Cause == "budget" && (record.Observation == nil || !record.Observation.Terminated) {
			return true
		}
	}
	return false
}

func TaskIdentity(req *task.TaskRecord, meta *task.MetaRecord, specHash, metaHash string, receipt *task.SupervisorReceipt) pueue.Identity {
	identity := pueue.Identity{RootID: req.RootID, TaskID: req.TaskID, SpecSHA256: specHash, MetaSHA256: metaHash}
	if receipt != nil {
		id := receipt.NumericTaskID
		identity.NumericTaskID = &id
	}
	return identity
}
