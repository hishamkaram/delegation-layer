package tasklifecycle

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TimeoutContinuation(req *task.TaskRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog, session *task.ProviderRefRecord, runner, commandRunner string, supervisor *task.SupervisorRef, runnerReleased, launchStateRecorded bool) *ContinuationOffer {
	if req == nil {
		return nil
	}
	for _, record := range records {
		if record.Request == nil || record.Request.Cause != "budget" {
			continue
		}
		if record.Observation == nil || !record.Observation.Terminated {
			continue
		}
		mode := commonprovider.ContinuationUnsupported
		if registration, err := catalog.Lookup(req.Provider); err == nil {
			mode = registration.Description.Continuation
		}
		resumable := mode == commonprovider.ContinuationNative && session != nil && runnerReleased && runner != "" && launchStateRecorded
		offer := &ContinuationOffer{Resumable: resumable, Mode: string(mode), PredecessorID: req.TaskID}
		if resumable {
			offer.CommandRunner, offer.Supervisor = commandRunner, supervisor
		}
		offer.Reason = timeoutContinuationReason(mode, session, runnerReleased, runner, launchStateRecorded)
		return offer
	}
	return nil
}
