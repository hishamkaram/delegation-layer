package tasklifecycle

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func existingFailure(result ExistingTaskResult, stage ExistingStage, err error) ExistingTaskResult {
	result.Stage, result.Failure = stage, err
	return result
}

func existingSupervisorOptions(options ExistingTaskOptions, control pueue.Options) SupervisorOptions {
	runtime := options.Creation.Admission.Supervisor
	return SupervisorOptions{Options: control, Lookup: runtime.Lookup(), OpenStore: runtime.OpenStore, Routing: runtime.Routing}
}

func existingSubmissionOptions(options ExistingTaskOptions, control pueue.Options) SubmissionOptions {
	return SubmissionOptions{Supervisor: existingSupervisorOptions(options, control), RequestedRunner: options.Selection.Runner, DefaultRunner: options.Creation.Admission.Supervisor.RunnerExecutable, OpenValidationStore: options.Creation.OpenValidationStore}
}
