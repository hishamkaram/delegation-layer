package supervisor

import (
	"context"
)

func PrepareAdmissionExecutables(selection AdmissionSelection, runtime AdmissionOptions, root string) (RunnerPreparation, error) {
	return PrepareAdmissionExecutablesContext(context.Background(), selection, runtime, root)
}

func PrepareAdmissionExecutablesContext(ctx context.Context, selection AdmissionSelection, runtime AdmissionOptions, root string) (RunnerPreparation, error) {
	if ctx == nil {
		return RunnerPreparation{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return RunnerPreparation{}, err
	}
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
	return PreparePrivateAdmissionExecutablesContext(ctx, selection, runtime, root, runner)
}

func PreparePrivateAdmissionExecutablesContext(ctx context.Context, selection AdmissionSelection, runtime AdmissionOptions, root, runner string) (RunnerPreparation, error) {
	if ctx == nil {
		return RunnerPreparation{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return RunnerPreparation{}, err
	}
	if _, _, err := AdmissionSupervisorExecutablePair(selection, root, runtime); err != nil {
		return RunnerPreparation{FailureStage: "prepare-supervisor-executables"}, err
	}
	return RunnerPreparation{Executable: runner}, nil
}
