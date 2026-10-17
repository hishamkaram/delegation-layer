package tasklifecycle

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func stopOutcomeFromResult(id, cause string, result pueue.StopResult) StopOutcome {
	return StopOutcome{requestID: id, cause: cause, requested: result.Requested, matched: result.Matched, attempted: result.Attempted, action: result.Action, numericTaskID: copyTaskID(result.NumericTaskID), acknowledged: copyAcknowledgement(result.Acknowledged), inFlight: result.InFlight, observedState: string(result.ObservedState), message: result.Message}
}

func copyTaskID(id *int64) *int64 {
	if id == nil {
		return nil
	}
	copied := *id
	return &copied
}
