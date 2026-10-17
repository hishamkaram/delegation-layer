package tasklifecycle

import (
	"errors"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func dispatchFailure(result DispatchResult, phase DispatchPhase, stage string, err error) DispatchResult {
	result.Phase, result.Failure = phase, admissioncore.WithStage(stage, err)
	return result
}

func (result DispatchResult) Err() error {
	err := result.Failure
	if result.Existing != nil {
		err = result.Existing.Err()
	}
	if result.Creation != nil {
		err = result.Creation.Err()
	}
	err = joinWorkflowClose(err, result.EnvironmentRestoreError)
	if result.Workspace != nil {
		err = joinWorkflowClose(err, result.Workspace.AbortError)
		err = joinWorkflowClose(err, result.Workspace.DescriptionError)
		err = joinWorkflowClose(err, result.Workspace.CloseError)
	}
	return joinWorkflowClose(err, result.StoreCloseError)
}

func (result ExistingTaskResult) Err() error {
	err := result.Failure
	if result.Creation != nil {
		err = result.Creation.Err()
	}
	if result.Submission != nil {
		err = result.Submission.Err()
		if result.Retried {
			err = SubmissionFailure(err, result.Submission.Admission == task.AdmissionAdmitted)
		}
	}
	err = joinWorkflowClose(err, result.EnvironmentRestoreError)
	err = joinWorkflowClose(err, result.TaskCloseError)
	if err != nil {
		err = admissioncore.WithStage("existing-task", err)
	}
	return err
}

func joinWorkflowClose(primary, closeErr error) error {
	if closeErr != nil {
		return errors.Join(primary, closeErr)
	}
	return primary
}
