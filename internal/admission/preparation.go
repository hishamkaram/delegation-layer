package admission

import (
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func Prepare(selection supervisorcore.AdmissionSelection, options PreparationOptions, store *taskdir.Store, req task.TaskRecord) (prepared PreparedAdmission, resultErr error) {
	var supervisorOwner *supervisorcore.ClientOwner
	defer func() {
		if resultErr != nil {
			supervisorcore.CloseOwnerMaintenance(supervisorOwner, resultErr)
		}
	}()
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
	runnerPreparation, err := supervisorcore.PrepareAdmissionExecutables(selection, options.Supervisor, store.Root)
	if err != nil {
		return PreparedAdmission{}, WithStage(runnerPreparation.FailureStage, err)
	}
	runner := runnerPreparation.Executable
	selection.Runner = runner
	supervisorOptions, err := SupervisorOptionsForCandidate(options.Supervisor.Control, candidate)
	if err != nil {
		return PreparedAdmission{}, WithStage("prepare-supervisor-options", err)
	}
	privateSupervisor, err := supervisorcore.AdmissionUsesPrivateSupervisor(selection, store.Root)
	if err != nil {
		return PreparedAdmission{}, WithStage("prepare-supervisor-options", err)
	}
	supervisorConfiguration := options.Supervisor
	supervisorConfiguration.Control = supervisorOptions
	supervisorOwner, err = supervisorcore.AcquireClient(supervisorcore.PrepareAdmissionSupervisor(selection, supervisorConfiguration, store, candidate.Inspection != nil, runner, privateSupervisor))
	if err != nil {
		return PreparedAdmission{}, WithStage("start-supervisor", err)
	}
	supervisor := supervisorOwner.Client()
	prepared = PreparedAdmission{Profile: profile, Supervisor: supervisor, RunnerExecutable: runner, RunnerOwnership: runnerOwnership}
	if candidate.Inspection != nil && !deferRuntime {
		prepared.Profile, candidate, err = finalizeInspection(selection, options, store, req, candidate, supervisor)
		if err != nil {
			return PreparedAdmission{}, err
		}
	}
	supervisorcore.ResumeRetainedQueueMaintenance(store.Root, supervisor)
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
