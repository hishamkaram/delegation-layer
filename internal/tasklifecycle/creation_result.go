package tasklifecycle

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func creationFailure(result CreationResult, stage CreationStage, err error) CreationResult {
	result.Stage, result.Failure = stage, err
	return result
}

func (result CreationResult) Err() error {
	if result.Submission == nil {
		return result.Failure
	}
	err := annotateMissingDispatchStage("submitting ordinary task", result.Submission.Err())
	return SubmissionFailure(err, result.Submission.Admission == task.AdmissionAdmitted)
}
