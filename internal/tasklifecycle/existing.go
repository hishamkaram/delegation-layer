package tasklifecycle

import (
	"context"
	"errors"
	"os"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func DispatchExisting(options ExistingTaskOptions, store *taskdir.Store, td *taskdir.TaskDir, req task.TaskRecord) (result ExistingTaskResult) {
	defer func() { result.TaskCloseError = td.Close() }()
	oldReq, oldMeta, err := td.PreparedRecords()
	if err != nil {
		created := CreateTask(options.Selection, options.Creation, store, req, nil, options.BriefPath)
		result.Creation = &created
		return result
	}

	if err = ValidateExistingDispatchRequest(options.RequestedBudget, options.Selection.ConfigPath, store, req, oldReq, oldMeta); err != nil {
		return existingFailure(result, ExistingPreparation, err)
	}
	result.RootID, result.TaskID = oldReq.RootID, oldReq.TaskID
	inspection, inspectErr := td.Inspect()
	result.Inspection = inspection
	if inspectErr != nil {
		return existingFailure(result, ExistingPreparation, inspectErr)
	}
	if supervisorcore.TaskTerminal(inspection) {
		if releaseErr := ReleaseContinuationSession(td, oldReq, inspection.Outcome); releaseErr != nil {
			return existingFailure(result, ExistingTerminalRelease, releaseErr)
		}
		return result
	}
	submit, submitErr := td.ReadSubmission()
	if submitErr == nil {
		if err = supervisorcore.RepairQueueForTask(context.Background(), store, oldMeta.SupervisorConfig, oldMeta, options.Creation.Admission.Supervisor); err != nil {
			return existingFailure(result, ExistingPreparation, err)
		}
		submitted := ReconcileSubmitted(store.Root, td, oldReq, oldMeta, submit, existingSupervisorOptions(options, options.Creation.Admission.Supervisor.Control))
		result.Submission = &submitted
		return result
	}
	if !errors.Is(submitErr, os.ErrNotExist) {
		return existingFailure(result, ExistingPreparation, submitErr)
	}
	if options.Selection.Runner != "" && options.Selection.Runner != oldMeta.RunnerExecutable {
		return existingFailure(result, ExistingExplicitRunner, task.ErrIdentityMismatch)
	}
	if err = supervisorcore.RepairQueueForTask(context.Background(), store, oldMeta.SupervisorConfig, oldMeta, options.Creation.Admission.Supervisor); err != nil {
		return existingFailure(result, ExistingPreparation, err)
	}
	return retryExistingTask(options, store, td, oldReq, oldMeta, inspection.StartExists, result)
}

func retryExistingTask(options ExistingTaskOptions, store *taskdir.Store, td *taskdir.TaskDir, oldReq *task.TaskRecord, oldMeta *task.MetaRecord, startExists bool, result ExistingTaskResult) (returned ExistingTaskResult) {
	if startExists {
		return existingFailure(result, ExistingStarted, task.ErrAlreadyStarted)
	}
	supervisorOptions := supervisorcore.OptionsForCurrentEnvironment(options.Creation.Admission.Supervisor.Control)
	if options.ApplyEnvironment == nil {
		return existingFailure(result, ExistingEnvironment, task.ErrEvidenceFault)
	}
	restoreEnvironment, restoreErr := options.ApplyEnvironment(oldMeta.Environment, oldMeta.EnvironmentRecorded)
	if restoreErr != nil {
		return existingFailure(result, ExistingEnvironment, restoreErr)
	}
	defer func() {
		if restoreEnvironment != nil {
			returned.EnvironmentRestoreError = restoreEnvironment()
		}
	}()
	profile, profileErr := options.Creation.Admission.Profiles.Match(store.Root, *oldReq, *oldMeta)
	if profileErr != nil {
		return existingFailure(result, ExistingProfile, profileErr)
	}
	selection, allowManagedRunnerUpgrade, runnerErr := supervisorcore.RefreshManagedRetryRunner(options.Selection, options.Creation.Admission.Supervisor.RunnerExecutable, store.Root, store, td, oldMeta)
	if runnerErr != nil {
		return existingFailure(result, ExistingRunnerRefresh, runnerErr)
	}
	options.Selection = selection
	submitted := SubmitSaved(td, oldReq, oldMeta, oldMeta.SupervisorConfig, existingSubmissionOptions(options, supervisorcore.OptionsWithEnvironment(supervisorOptions, profile.Plan.Environment)), store.Root, allowManagedRunnerUpgrade)
	result.Submission = &submitted
	result.Retried = true
	return result
}
