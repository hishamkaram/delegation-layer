package app

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
)

func supervisorAdmissionSelection(a Arguments, deps Dependencies) supervisorcore.AdmissionSelection {
	return supervisorcore.AdmissionSelection{Runner: a.Runner, RunnerOwnership: a.runnerOwnership, ConfigPath: a.PueueConfig, EnvironmentConfigPath: supervisorcore.EnvironmentConfigPath(deps.Environment), SavedSupervisor: a.savedSupervisor}
}

func supervisorAdmissionOptions(deps Dependencies, control pueue.Options) supervisorcore.AdmissionOptions {
	return supervisorcore.AdmissionOptions{RunnerExecutable: deps.RunnerExecutable, InitialExecutable: deps.InitialSupervisorExecutable, Environment: deps.Environment, Control: control, Routing: deps.infrastructureRouting(), OpenStore: openNativeSupervisorStore}
}
