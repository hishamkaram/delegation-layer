package app

import (
	"errors"
	"strings"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func applyTimeoutContinuation(response *Response, root string, req *task.TaskRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog, session *task.ProviderRefRecord, runner string, supervisor *task.SupervisorRef) {
	applyTimeoutContinuationWithRunnerState(response, root, req, records, catalog, session, runner, supervisor, true, false)
}

func applyTimeoutContinuationWithRunnerState(response *Response, root string, req *task.TaskRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog, session *task.ProviderRefRecord, runner string, supervisor *task.SupervisorRef, runnerReleased, launchStateRecorded bool) {
	if response == nil || req == nil {
		return
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
		response.Status = "timed_out"
		response.Continuation = &ContinuationResponse{Resumable: resumable, Mode: string(mode), PredecessorID: req.TaskID}
		if resumable {
			response.Continuation.ContinueCommand = continuationCommand(root, req.TaskID, runner, supervisor)
		}
		response.Continuation.Reason = timeoutContinuationReason(mode, session, runnerReleased, runner, launchStateRecorded)
		return
	}
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

func hasTerminatedBudgetStop(records []taskdir.StopRecord) bool {
	for _, record := range records {
		if record.Request != nil && record.Request.Cause == "budget" && record.Observation != nil && record.Observation.Terminated {
			return true
		}
	}
	return false
}

func continuationCommand(root, taskID, runner string, supervisor *task.SupervisorRef) string {
	command := "delegate --root " + shellQuote(root)
	if supervisor != nil && supervisor.ConfigPath != "" && !pueue.IsPrivateConfig(root, supervisor.ConfigPath) {
		command += " --pueue-config " + shellQuote(supervisor.ConfigPath)
	}
	if runner != "" {
		command += " --runner " + shellQuote(runner)
	}
	return command + " continue --task " + taskID + " --json"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func readTimeoutRecords(td *taskdir.TaskDir) ([]taskdir.StopRecord, error) {
	if td == nil {
		return nil, errors.New("nil task directory")
	}
	return td.ReadStopRecords()
}
