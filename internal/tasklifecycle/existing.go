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

// retryExistingTask resubmits an unstarted task in its own saved launch scope,
// selected over the caller's scope. Supervisor control keeps the caller's
// environment; maintenance started by the retry uses the selected scope.
func retryExistingTask(options ExistingTaskOptions, store *taskdir.Store, td *taskdir.TaskDir, oldReq *task.TaskRecord, oldMeta *task.MetaRecord, startExists bool, result ExistingTaskResult) ExistingTaskResult {
	if startExists {
		return existingFailure(result, ExistingStarted, task.ErrAlreadyStarted)
	}
	caller := options.Creation.Admission.Profiles.Environment()
	profiles, selectErr := options.Creation.Admission.Profiles.ForRecord(oldMeta.Environment, oldMeta.EnvironmentRecorded)
	if selectErr != nil {
		return existingFailure(result, ExistingEnvironment, selectErr)
	}
	options.Creation.Admission.Supervisor = supervisorcore.AdmissionOptionsForScope(options.Creation.Admission.Supervisor, caller, profiles.Environment())
	supervisorOptions := options.Creation.Admission.Supervisor.Control
	profile, profileErr := profiles.Match(store.Root, *oldReq, *oldMeta)
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
