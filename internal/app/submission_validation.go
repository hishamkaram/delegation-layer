package app

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func validateSubmissionRunner(deps Dependencies, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner string) error {
	return admissioncore.ValidateSubmissionRunner(submissionValidationStore(deps), td, req, meta, runner)
}

func validateManagedRetrySubmissionRunner(deps Dependencies, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, runner string) error {
	return admissioncore.ValidateManagedRetrySubmissionRunner(submissionValidationStore(deps), td, req, meta, runner)
}
