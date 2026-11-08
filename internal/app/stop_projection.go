package app

import "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/stop"

func stopOutcomeResponse(outcome stop.Outcome) StopResponse {
	return StopResponse{
		RequestID:     outcome.RequestID(),
		Cause:         outcome.Cause(),
		Requested:     outcome.Requested(),
		Matched:       outcome.Matched(),
		Attempted:     outcome.Attempted(),
		Action:        outcome.Action(),
		NumericTaskID: outcome.NumericTaskID(),
		Acknowledged:  outcome.Acknowledged(),
		InFlight:      outcome.InFlight(),
		ObservedState: outcome.ObservedState(),
		Terminated:    outcome.Terminated(),
		Message:       outcome.Message(),
	}
}
