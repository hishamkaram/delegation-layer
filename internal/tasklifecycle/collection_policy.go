package tasklifecycle

import (
	"errors"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
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

func continuationIsFinal(continuation *ContinuationOffer) bool {
	if continuation == nil {
		return false
	}
	return continuation.Resumable || continuation.Mode == string(commonprovider.ContinuationUnsupported) || continuation.Mode == string(commonprovider.ContinuationCheckpoint)
}
