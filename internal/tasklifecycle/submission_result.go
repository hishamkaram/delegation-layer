package tasklifecycle

import (
	"errors"
)

func submissionFailure(stage SubmissionStage, err error) SubmissionResult {
	return SubmissionResult{Stage: stage, Failure: err}
}

// Err preserves the same primary -> permit release -> task close join order.
func (result SubmissionResult) Err() error {
	err := result.Failure
	if result.PermitReleaseError != nil {
		err = errors.Join(err, result.PermitReleaseError)
	}
	if result.TaskCloseError != nil {
		err = errors.Join(err, result.TaskCloseError)
	}
	return err
}
