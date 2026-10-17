package supervisor

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func PrepareAdmissionExecutables(selection AdmissionSelection, runtime AdmissionOptions, root string) (RunnerPreparation, error) {
	runnerOwnership := RequestedRunnerOwnership(selection)
	privateSupervisor, err := AdmissionUsesPrivateSupervisor(selection, root)
	if err != nil {
		return RunnerPreparation{FailureStage: "prepare-supervisor-executables"}, err
	}
	runner, err := ResolveAdmissionRunner(selection.Runner, runnerOwnership, runtime)
	if err != nil {
		return RunnerPreparation{FailureStage: "resolve-runner"}, err
	}
	if !privateSupervisor {
		return RunnerPreparation{Executable: runner}, nil
	}
	return PreparePrivateAdmissionExecutablesContext(context.Background(), selection, runtime, root, runner, runnerOwnership)
}

func PreparePrivateAdmissionExecutablesContext(ctx context.Context, selection AdmissionSelection, runtime AdmissionOptions, root, runner, runnerOwnership string) (RunnerPreparation, error) {
	controlContext, cancel := ControlContextWithParent(ctx, runtime.Control)
	defer cancel()
	terminalRunner, err := ResolveRunner("", runtime.RunnerExecutable)
	if err != nil {
		return RunnerPreparation{FailureStage: "resolve-runner"}, err
	}
	if _, err = PrepareStateTerminalEventRunner(controlContext, root, terminalRunner); err != nil {
		return RunnerPreparation{FailureStage: "prepare-runner"}, err
	}
	if runnerOwnership == task.RunnerOwnershipManaged {
		runner, err = InstallStateRunner(root, runner)
		if err != nil {
			return RunnerPreparation{FailureStage: "prepare-runner"}, err
		}
	}
	clientExecutable, daemonExecutable, err := AdmissionSupervisorExecutablePair(selection, root, runtime)
	if err != nil {
		return RunnerPreparation{FailureStage: "prepare-supervisor-executables"}, err
	}
	if err = PrepareStateSupervisorPair(controlContext, root, clientExecutable, daemonExecutable); err != nil {
		return RunnerPreparation{FailureStage: "prepare-supervisor-executables"}, err
	}
	return RunnerPreparation{Executable: runner}, nil
}
