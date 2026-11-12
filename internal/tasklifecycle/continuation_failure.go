package tasklifecycle

import "errors"

// ContinuationFailureReason names the predecessor step that refused this
// invocation. It is diagnostic metadata, never retry or session authority.
type ContinuationFailureReason string

const (
	ContinuationPredecessorRecordsUnavailable           ContinuationFailureReason = "predecessor_records_unavailable"
	ContinuationPredecessorEnvironmentUnrecorded        ContinuationFailureReason = "predecessor_environment_unrecorded"
	ContinuationProfilesUnavailable                     ContinuationFailureReason = "continuation_profiles_unavailable"
	ContinuationPredecessorSessionUnavailable           ContinuationFailureReason = "predecessor_session_unavailable"
	ContinuationValidationFailed                        ContinuationFailureReason = "continuation_validation_failed"
	ContinuationPredecessorObservationFailed            ContinuationFailureReason = "predecessor_observation_failed"
	ContinuationPredecessorReconciliationFailed         ContinuationFailureReason = "predecessor_reconciliation_failed"
	ContinuationPredecessorSubmissionUnavailable        ContinuationFailureReason = "predecessor_submission_unavailable"
	ContinuationPredecessorRunnerLeaseObservationFailed ContinuationFailureReason = "predecessor_runner_lease_observation_failed"
	ContinuationPredecessorRunnerLeaseBusy              ContinuationFailureReason = "predecessor_runner_lease_busy"
	ContinuationPredecessorOutcomeUnavailable           ContinuationFailureReason = "predecessor_outcome_unavailable"
	ContinuationPredecessorSessionHandoffFailed         ContinuationFailureReason = "predecessor_session_handoff_failed"
)

// continuationStepError adds invocation-local provenance without adding a
// sentinel or changing the original cause graph or its public classification.
type continuationStepError struct {
	reason ContinuationFailureReason
	cause  error
}

func (e *continuationStepError) Error() string { return e.cause.Error() }
func (e *continuationStepError) Unwrap() error { return e.cause }

func withContinuationFailure(reason ContinuationFailureReason, err error) error {
	if err == nil {
		return nil
	}
	var existing *continuationStepError
	if errors.As(err, &existing) {
		return err
	}
	return &continuationStepError{reason: reason, cause: err}
}

// ContinuationReason reads source-owned diagnostic provenance. A caller must
// still classify the complete error; this does not soften joined faults.
func ContinuationReason(err error) ContinuationFailureReason {
	var failure *continuationStepError
	if errors.As(err, &failure) {
		return failure.reason
	}
	return ""
}
