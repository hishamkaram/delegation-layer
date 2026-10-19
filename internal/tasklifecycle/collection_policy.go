package tasklifecycle

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func CollectionInspectionOutcome(inspection *taskdir.TaskInspection, collectErr, inspectErr, descriptorErr error) (*task.OutcomeRecord, error, bool) {
	if inspection == nil || !supervisorcore.TaskTerminal(inspection) || inspection.Outcome == nil {
		return nil, nil, false
	}
	if IsPureCollectionPendingError(collectErr) {
		collectErr = nil
	}
	return inspection.Outcome, errors.Join(collectErr, inspectErr, descriptorErr), true
}

func observedPending(observation pueue.Observation) PendingKind {
	if observation.Pending == nil {
		return ""
	}
	return PendingSupervisor
}

// uncertaintyPending describes a pure supervisor uncertainty that observed no
// queue row. Only a retained in-flight command is pending; ErrUnknown alone
// proves no unfinished command.
func uncertaintyPending(err error) PendingKind {
	if errors.Is(err, pueue.ErrInFlight) {
		return PendingSupervisor
	}
	return ""
}
