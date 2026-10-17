package app

import "github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

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
	response.Status = "timed_out"
	response.Continuation = &ContinuationResponse{
		Resumable: offer.Resumable, Mode: offer.Mode,
		PredecessorID: offer.PredecessorID, Reason: offer.Reason,
	}
	if offer.Resumable {
		response.Continuation.ContinueCommand = continuationCommand(root, offer.PredecessorID, offer.CommandRunner, offer.Supervisor)
	}
}

func applyRejectedRecovery(response *Response, available *bool) {
	if available == nil {
		return
	}
	failure := DispatchFailureResponse{Code: "provider_output_rejected", Stage: "collect", NextAction: "stop_and_report"}
	if *available {
		failure.NextAction = "recheck"
	}
	response.Failure = &failure
}
