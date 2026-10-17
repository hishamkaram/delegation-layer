package app

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func submissionValidationStore(deps Dependencies) admissioncore.SubmissionStoreOpener {
	return func(root string) (*taskdir.Store, error) {
		return openStore(root, deps.normalized().storeDependencies(), false)
	}
}

func submissionCommandResult(response Response, submitted tasklifecycle.SubmissionResult) commandResult {
	applyTaskObservation(&response, submitted.Observation, "")
	result := commandResult{response: response, code: 0}
	if submitted.Failure != nil {
		fallback := 1
		if submitted.Stage == tasklifecycle.SubmissionProfile || submitted.Stage == tasklifecycle.SubmissionRunner || submitted.Stage == tasklifecycle.SubmissionRunnerValidation {
			fallback = 2
		}
		code := fallback
		if submitted.Stage != tasklifecycle.SubmissionRecords {
			code = classifyCode(submitted.Failure, fallback)
		}
		result = failed(response, submitted.Failure, code)
	}
	mergeCommandClose(&result, func() error { return submitted.PermitReleaseError })
	mergeCommandClose(&result, func() error { return submitted.TaskCloseError })
	return result
}

func submitWithClient(td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, client *pueue.Client, runner string, permit *taskdir.SubmissionPermit, response Response) commandResult {
	return submissionCommandResult(response, tasklifecycle.SubmitWithClient(td, req, meta, client, runner, permit))
}
