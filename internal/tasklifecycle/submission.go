package tasklifecycle

import (
	"context"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func SubmitSaved(td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, supervisor task.SupervisorRef, options SubmissionOptions, recoveryRoot string, allowManagedRunnerUpgrade bool) (result SubmissionResult) {
	supervisorOptions := options.Supervisor.Options
	if meta != nil {
		supervisorOptions = supervisorcore.OptionsWithEnvironment(supervisorOptions, meta.Environment)
	}
	client, err := supervisorcore.NewClient(context.Background(), recoveryRoot, supervisor, supervisorOptions, recoveryRoot != "", options.Supervisor.InitialExecutable, options.Supervisor.OpenStore)
	if err != nil {
		return submissionFailure(SubmissionClient, err)
	}
	defer func() { supervisorcore.CloseMaintenance(client, result.Err()) }()
	runner, err := supervisorcore.ResolveTaskRunner(options.RequestedRunner, options.DefaultRunner, meta)
	if err != nil {
		return submissionFailure(SubmissionRunner, err)
	}
	if allowManagedRunnerUpgrade {
		err = admissioncore.ValidateManagedRetrySubmissionRunner(options.OpenValidationStore, td, req, meta, runner)
	} else {
		err = admissioncore.ValidateSubmissionRunner(options.OpenValidationStore, td, req, meta, runner)
	}
	if err != nil {
		return submissionFailure(SubmissionRunnerValidation, err)
	}
	if req.PriorSession != nil {
		if err = td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
			return submissionFailure(SubmissionSession, err)
		}
	}
	permit, err := td.PrepareSubmission(supervisor)
	if err != nil {
		return submissionFailure(SubmissionPermit, err)
	}
	defer func() { result.PermitReleaseError = permit.Release() }()
	return SubmitWithClient(td, req, meta, client, runner, permit)
}

func SubmitWithProfile(td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, client *pueue.Client, profile commonprovider.PreparedProfile, options SubmissionOptions) (result SubmissionResult) {
	defer func() { result.TaskCloseError = td.Close() }()
	if err := profile.ValidateAdmission(*req); err != nil {
		return submissionFailure(SubmissionProfile, err)
	}
	runner, err := supervisorcore.ResolveTaskRunner(options.RequestedRunner, options.DefaultRunner, meta)
	if err != nil {
		return submissionFailure(SubmissionRunner, err)
	}
	if err = admissioncore.ValidateSubmissionRunner(options.OpenValidationStore, td, req, meta, runner); err != nil {
		return submissionFailure(SubmissionRunnerValidation, err)
	}
	if req.PriorSession != nil {
		if err = td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
			return submissionFailure(SubmissionSession, err)
		}
	}
	permit, err := td.PrepareSubmission(client.Binding())
	if err != nil {
		return submissionFailure(SubmissionPermit, err)
	}
	defer func() { result.PermitReleaseError = permit.Release() }()
	return SubmitWithClient(td, req, meta, client, runner, permit)
}
