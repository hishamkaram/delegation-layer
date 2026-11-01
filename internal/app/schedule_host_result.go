package app

import "fmt"

// savedScheduleHostResult projects host readiness only after a successful
// definition mutation. It never creates a host or changes the saved definition.
func savedScheduleHostResult(result commandResult, observed SchedulerResponse, observationErr error) commandResult {
	if result.err != nil || result.code != 0 || result.response.Schedule == nil {
		return result
	}
	result.response.Schedule.Saved = true
	result.response.Scheduler = &observed
	if observationErr != nil {
		if code := schedulerHostResultCode(observationErr); code != 3 {
			return failed(result.response, observationErr, code)
		}
		result.code = 3
		result.response.Scheduler.NextAction = "inspect_scheduler"
		result.response.Pending = &PendingResponse{Kind: "scheduler"}
		return result
	}
	if observed.Fault {
		return failed(result.response, fmt.Errorf("scheduler host fault: %s", observed.LastError), 1)
	}
	if observed.State == "ready" && observed.Ready {
		return result
	}
	result.code = 3
	if observed.State == "no_host_observed" || observed.State == "stopped" {
		result.response.Schedule.NextAction = "start_required"
		result.response.Scheduler.NextAction = "start_required"
		result.response.Pending = &PendingResponse{Kind: "start_required"}
		return result
	}
	result.response.Scheduler.NextAction = "inspect_scheduler"
	result.response.Pending = &PendingResponse{Kind: "scheduler"}
	return result
}
