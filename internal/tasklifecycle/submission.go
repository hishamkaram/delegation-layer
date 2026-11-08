package tasklifecycle

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func SubmitSaved(td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, supervisor task.SupervisorRef, options SubmissionOptions, recoveryRoot string, allowManagedRunnerUpgrade bool) (result SubmissionResult) {
	return SubmitSavedContext(context.Background(), td, req, meta, supervisor, options, recoveryRoot, allowManagedRunnerUpgrade)
}

func SubmitSavedContext(ctx context.Context, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, supervisor task.SupervisorRef, options SubmissionOptions, recoveryRoot string, allowManagedRunnerUpgrade bool) (result SubmissionResult) {
	if err := td.RequireTaskLaunchPermissionContext(ctx); err != nil {
		return submissionFailure(SubmissionPermit, err)
	}

	supervisorOptions := options.Supervisor.Options
	if meta != nil {
		supervisorOptions = supervisorcore.OptionsWithEnvironment(supervisorOptions, meta.Environment)
	}
	var source infrastructure.SavedBindingSource
	if meta != nil {
		var err error
		source, err = supervisorcore.NewSavedBindingSource(req.RootID, infrastructure.SavedBindingTaskMetadata, td.TaskID, "", meta.SupervisorConfig)
		if err != nil {
			return submissionFailure(SubmissionClient, err)
		}
	}
	options.Supervisor.Options = supervisorOptions
	client, err := options.Supervisor.savedClient(ctx, recoveryRoot, supervisor, source)
	if err != nil {
		return submissionFailure(SubmissionClient, supervisorcore.ClosePrivateAdmissionTransition(client, err))
	}
	defer func() { supervisorcore.CloseMaintenance(client, result.Err()) }()
	runner, err := installation.ResolveTaskRunner(options.RequestedRunner, options.DefaultRunner, meta)
	if err != nil {
		return submissionFailure(SubmissionRunner, err)
	}
	if allowManagedRunnerUpgrade {
		err = admissioncore.ValidateManagedRetrySubmissionRunnerContext(ctx, options.OpenValidationStore, td, req, meta, runner, options.DefaultRunner)
	} else {
		err = admissioncore.ValidateSubmissionRunnerContext(ctx, options.OpenValidationStore, td, req, meta, runner, options.DefaultRunner)
	}
	if err != nil {
		return submissionFailure(SubmissionRunnerValidation, err)
	}
	if req.PriorSession != nil {
		if err = td.ClaimSessionContext(ctx, req.Provider, req.PriorSession.ConversationID); err != nil {
			return submissionFailure(SubmissionSession, err)
		}
	}
	permit, err := td.PrepareSubmissionContext(ctx, supervisor)
	if err != nil {
		return submissionFailure(SubmissionPermit, err)
	}
	defer func() { result.PermitReleaseError = permit.Release() }()
	return SubmitWithClientContext(ctx, td, req, meta, client, runner, permit)
}

func SubmitWithProfile(td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, client *pueue.Client, profile commonprovider.PreparedProfile, options SubmissionOptions) (result SubmissionResult) {
	return SubmitWithProfileContext(context.Background(), td, req, meta, client, profile, options)
}

func SubmitWithProfileContext(ctx context.Context, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, client *pueue.Client, profile commonprovider.PreparedProfile, options SubmissionOptions) (result SubmissionResult) {
	if err := td.RequireTaskLaunchPermissionContext(ctx); err != nil {
		return submissionFailure(SubmissionPermit, err)
	}

	defer func() { result.TaskCloseError = td.Close() }()
	if err := profile.ValidateAdmission(*req); err != nil {
		return submissionFailure(SubmissionProfile, err)
	}
	runner, err := installation.ResolveTaskRunner(options.RequestedRunner, options.DefaultRunner, meta)
	if err != nil {
		return submissionFailure(SubmissionRunner, err)
	}
	if err = admissioncore.ValidateSubmissionRunnerContext(ctx, options.OpenValidationStore, td, req, meta, runner, options.DefaultRunner); err != nil {
		return submissionFailure(SubmissionRunnerValidation, err)
	}
	if req.PriorSession != nil {
		if err = td.ClaimSessionContext(ctx, req.Provider, req.PriorSession.ConversationID); err != nil {
			return submissionFailure(SubmissionSession, err)
		}
	}
	permit, err := td.PrepareSubmissionContext(ctx, client.Binding())
	if err != nil {
		return submissionFailure(SubmissionPermit, err)
	}
	defer func() { result.PermitReleaseError = permit.Release() }()
	return SubmitWithClientContext(ctx, td, req, meta, client, runner, permit)
}
