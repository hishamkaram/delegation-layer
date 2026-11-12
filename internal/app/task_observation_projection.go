package app

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func applyTaskObservation(response *Response, observed tasklifecycle.Observation, root string) {
	fillInspection(response, observed.Inspection)
	if observed.Interaction != nil {
		response.Interaction = observed.Interaction
	}
	if observed.AdmissionObserved {
		response.Admission = observed.Admission.String()
	}
	if observed.LivenessObserved {
		response.Liveness = observed.Liveness.String()
	}
	if observed.Supervisor != nil {
		response.Supervisor = observed.Supervisor
		response.Pending = nil
	}
	if observed.Pending != "" {
		response.Pending = &PendingResponse{Kind: string(observed.Pending)}
	}
	if observed.StopsRead {
		response.Stops = stopResponses(observed.Stops)
	}
	if observed.Raw != nil {
		response.Raw = observed.Raw
	}
	applyContinuationOffer(response, root, observed.Continuation)
}

func applyContinuationOffer(response *Response, root string, offer *tasklifecycle.ContinuationOffer) {
	if response == nil || offer == nil {
		return
	}
	response.Status = string(offer.Cause)
	response.Continuation = &ContinuationResponse{
		Resumable: offer.Resumable, Mode: offer.Mode,
		PredecessorID: offer.PredecessorID, SessionSource: string(offer.SessionSource), Reason: offer.Reason,
	}
	if offer.Resumable {
		response.Continuation.ContinueCommand = continuationCommand(root, offer.PredecessorID, offer.CommandRunner, offer.Supervisor)
	}
	switch {
	case offer.Cause == tasklifecycle.ContinuationInterrupted:
		failure := DispatchFailureResponse{Code: "task_interrupted", Stage: response.Command, NextAction: "stop_and_report"}
		if offer.Resumable {
			failure.NextAction = "continue_task"
		}
		response.Failure = &failure
	case offer.Cancelled && response.Outcome == nil:
		// A committed or rejected outcome stays the authoritative result.
		response.Failure = &DispatchFailureResponse{Code: "task_cancelled", Stage: response.Command, NextAction: "stop_and_report"}
	case offer.Cause == tasklifecycle.ContinuationIdleTimedOut:
		next := "stop_and_report"
		if offer.Resumable {
			next = "continue_task"
		}
		response.Failure = &DispatchFailureResponse{Code: "task_idle_timed_out", Stage: response.Command, NextAction: next}
	}
}

func applyRejectedRecovery(response *Response, available *bool) {
	if available == nil {
		return
	}
	// The lifecycle owner proved an exact-session handoff after output-silence
	// termination. Keep that guidance alongside the rejected outcome and exit 4.
	if response.Status == string(tasklifecycle.ContinuationIdleTimedOut) && response.Continuation != nil && response.Continuation.Resumable {
		return
	}
	if response.Failure != nil && response.Failure.Code != "provider_output_rejected" {
		return
	}
	failure := DispatchFailureResponse{Code: "provider_output_rejected", Stage: "collect", NextAction: "stop_and_report"}
	if *available {
		failure.NextAction = "recheck"
	}
	if response.Outcome != nil && response.Outcome.Verdict == task.VerdictRejected && response.Outcome.Predicate.Adapter == "claude:print" && task.HasProviderAuthenticationFailure(response.Outcome.Diagnostics) {
		failure.NextAction = "run_native_provider_login"
	}
	response.Failure = &failure
}
