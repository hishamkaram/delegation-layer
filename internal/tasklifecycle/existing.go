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
	return DispatchExistingContext(context.Background(), options, store, td, req)
}

func DispatchExistingContext(ctx context.Context, options ExistingTaskOptions, store *taskdir.Store, td *taskdir.TaskDir, req task.TaskRecord) (result ExistingTaskResult) {
	defer func() { result.TaskCloseError = td.Close() }()
	oldReq, oldMeta, err := td.PreparedRecords()
	if err != nil {
		created := CreateTaskContext(ctx, options.Selection, options.Creation, store, req, nil, options.BriefPath)
		result.Creation = &created
		return result
	}

	if err = ValidateExistingDispatchRequestContext(ctx, options.RequestedBudget, options.Selection.ConfigPath, store, req, oldReq, oldMeta); err != nil {
		return existingFailure(result, ExistingPreparation, err)
	}
	result.RootID, result.TaskID = oldReq.RootID, oldReq.TaskID
	inspection, inspectErr := td.Inspect()
	result.Inspection = inspection
	if inspectErr != nil {
		return existingFailure(result, ExistingPreparation, inspectErr)
	}
	if supervisorcore.TaskTerminal(inspection) {
		if releaseErr := ReleaseContinuationSessionContext(ctx, td, oldReq, inspection.Outcome); releaseErr != nil {
			return existingFailure(result, ExistingTerminalRelease, releaseErr)
		}
		return result
	}
	submit, submitErr := td.ReadSubmission()
	if submitErr == nil {
		if err = supervisorcore.RepairQueueForSubmittedTask(ctx, store, submit, oldMeta, options.Creation.Admission.Supervisor); err != nil {
			return existingFailure(result, ExistingPreparation, err)
		}
		control := existingSupervisorOptions(options, options.Creation.Admission.Supervisor.Control)
		control.AdmissionStore = store
		submitted := ReconcileSubmittedContext(ctx, store.Root, td, oldReq, oldMeta, submit, control)
		result.Submission = &submitted
		return result
	}
	if !errors.Is(submitErr, os.ErrNotExist) {
		return existingFailure(result, ExistingPreparation, submitErr)
	}
	if options.Selection.Runner != "" && options.Selection.Runner != oldMeta.RunnerExecutable {
		return existingFailure(result, ExistingExplicitRunner, task.ErrIdentityMismatch)
	}
	if err = supervisorcore.RepairQueueForTask(ctx, store, oldMeta.SupervisorConfig, oldMeta, options.Creation.Admission.Supervisor); err != nil {
		return existingFailure(result, ExistingPreparation, err)
	}
	return retryExistingTask(ctx, options, store, td, oldReq, oldMeta, inspection.StartExists, result)
}

// retryExistingTask resubmits an unstarted task in its own saved launch scope,
// selected over the caller's scope. Supervisor control keeps the caller's
// environment; maintenance started by the retry uses the selected scope.
func retryExistingTask(ctx context.Context, options ExistingTaskOptions, store *taskdir.Store, td *taskdir.TaskDir, oldReq *task.TaskRecord, oldMeta *task.MetaRecord, startExists bool, result ExistingTaskResult) ExistingTaskResult {
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
	profile, profileErr := profiles.MatchContext(ctx, store.Root, *oldReq, *oldMeta)
	if profileErr != nil {
		return existingFailure(result, ExistingProfile, profileErr)
	}
	selection, allowManagedRunnerUpgrade, runnerErr := supervisorcore.RefreshManagedRetryRunnerContext(ctx, options.Selection, options.Creation.Admission.Supervisor.RunnerExecutable, store.Root, td, oldMeta)
	if runnerErr != nil {
		return existingFailure(result, ExistingRunnerRefresh, runnerErr)
	}
	options.Selection = selection
	submissionOptions := existingSubmissionOptions(options, supervisorcore.OptionsWithEnvironment(supervisorOptions, profile.Plan.Environment))
	submissionOptions.Supervisor.AdmissionStore = store
	submitted := SubmitSavedContext(ctx, td, oldReq, oldMeta, oldMeta.SupervisorConfig, submissionOptions, store.Root, allowManagedRunnerUpgrade)
	result.Submission = &submitted
	result.Retried = true
	return result
}
