package admission

import (
	"context"
	"errors"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func Prepare(selection supervisorcore.AdmissionSelection, options PreparationOptions, store *taskdir.Store, req task.TaskRecord) (prepared PreparedAdmission, resultErr error) {
	return PrepareContext(context.Background(), selection, options, store, req)
}

func PrepareContext(ctx context.Context, selection supervisorcore.AdmissionSelection, options PreparationOptions, store *taskdir.Store, req task.TaskRecord) (prepared PreparedAdmission, resultErr error) {
	selection, err := resolveAdmissionSelection(ctx, selection, store, req.TaskID)
	if err != nil {
		return PreparedAdmission{}, err
	}
	var supervisorOwner *supervisorcore.ClientOwner
	defer func() {
		if resultErr != nil {
			supervisorcore.CloseOwnerMaintenance(supervisorOwner, resultErr)
		}
	}()
	// Bind captured control before merging the selected provider scope. An
	// uncaptured composition keeps pueue's standalone default.
	if options.Supervisor.Environment != nil {
		options.Supervisor.Control = supervisorcore.OptionsForEnvironment(options.Supervisor.Control, options.Supervisor.Environment)
	}
	candidate, err := options.Profiles.Prepare(store.Root, req)
	if err != nil {
		return PreparedAdmission{}, WithStage("prepare-provider", err)
	}
	runnerOwnership := supervisorcore.RequestedRunnerOwnership(selection)
	deferRuntime := runnerOwnership == task.RunnerOwnershipManaged && candidate.Admit != nil
	profile, err := ProfileForAdmission(candidate, req, deferRuntime)
	if err != nil {
		return PreparedAdmission{}, WithStage("finalize-provider", err)
	}
	selection.RunnerOwnership = runnerOwnership
	runnerPreparation, err := supervisorcore.PrepareAdmissionExecutablesContext(ctx, selection, options.Supervisor, store.Root)
	if err != nil {
		return PreparedAdmission{}, WithStage(runnerPreparation.FailureStage, err)
	}
	runner := runnerPreparation.Executable
	selection.Runner = runner
	supervisorOwner, err = supervisorcore.AcquireClient(prepareAdmissionSupervisorContext(ctx, selection, options.Supervisor, store, candidate, runner))
	if err != nil {
		return PreparedAdmission{}, err
	}
	supervisor := supervisorOwner.Client()
	prepared = PreparedAdmission{Profile: profile, Supervisor: supervisor, RunnerExecutable: runner, RunnerOwnership: runnerOwnership}
	if candidate.Inspection != nil && !deferRuntime {
		prepared.Profile, candidate, err = finalizeInspection(ctx, selection, options, store, req, candidate, supervisor)
		if err != nil {
			return PreparedAdmission{}, err
		}
	}
	supervisorcore.ResumeRetainedQueueMaintenanceContext(ctx, store.Root, supervisor)
	prepared.Capability, err = readCapabilityEvidence(options.DescribeProvider, req, candidate, prepared.Profile)
	if err != nil {
		return PreparedAdmission{}, err
	}
	if err = prepared.Profile.ValidateStatePlacement(store.Root); err != nil {
		return PreparedAdmission{}, WithStage("validate-state-placement", err)
	}
	supervisorOwner.Release()
	return prepared, nil
}

func resolveAdmissionSelection(ctx context.Context, selection supervisorcore.AdmissionSelection, store *taskdir.Store, taskID string) (supervisorcore.AdmissionSelection, error) {
	if ctx == nil {
		return selection, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return selection, err
	}
	if selection.SavedSupervisor == nil || selection.SavedSource != (infrastructure.SavedBindingSource{}) || selection.ContinuationSource != (infrastructure.SavedBindingSource{}) || !pueue.IsPrivateConfig(store.Root, selection.SavedSupervisor.ConfigPath) {
		return selection, nil
	}
	source, err := admissionSavedSource(store, taskID, *selection.SavedSupervisor)
	if err != nil {
		return selection, WithStage("prepare-supervisor-options", err)
	}
	selection.SavedSource = source
	return selection, nil
}

func prepareAdmissionSupervisorContext(ctx context.Context, selection supervisorcore.AdmissionSelection, configuration supervisorcore.AdmissionOptions, store *taskdir.Store, candidate commonprovider.ProfileCandidate, runner string) (*pueue.Client, error) {
	options, err := SupervisorOptionsForCandidate(configuration.Control, candidate)
	if err != nil {
		return nil, WithStage("prepare-supervisor-options", err)
	}
	private, err := supervisorcore.AdmissionUsesPrivateSupervisor(selection, store.Root)
	if err != nil {
		return nil, WithStage("prepare-supervisor-options", err)
	}
	configuration.Control = options
	client, err := supervisorcore.PrepareAdmissionSupervisorContext(ctx, selection, configuration, store, candidate.Inspection != nil, runner, private)
	if err != nil {
		return client, WithStage("start-supervisor", err)
	}
	return client, nil
}

func admissionSavedSource(store *taskdir.Store, taskID string, binding task.SupervisorRef) (source infrastructure.SavedBindingSource, resultErr error) {
	td, err := store.OpenTask(taskID)
	if err != nil {
		return source, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	_, meta, _, _, err := td.PreparedIdentity()
	if err != nil {
		return source, err
	}
	source, err = supervisorcore.NewSavedBindingSource(meta.RootID, infrastructure.SavedBindingTaskMetadata, td.TaskID, "", meta.SupervisorConfig)
	if err != nil {
		return source, err
	}
	return source, source.ValidateBinding(binding)
}
