package supervisor

import (
	"context"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func PrepareAdmissionSupervisor(selection AdmissionSelection, runtime AdmissionOptions, store *taskdir.Store, requiresInspection bool, runner string, privateSupervisor bool) (*pueue.Client, error) {
	var err error
	var supervisor *pueue.Client
	if privateSupervisor && selection.SavedSupervisor == nil {
		supervisor, err = PreparePrivateAdmissionSupervisor(runtime.InitialExecutable, store, runtime.Control)
	} else {
		supervisor, err = BindInitialWithStart(selection, runtime, store.Root, !requiresInspection)
	}
	if err != nil {
		return supervisor, err
	}
	if !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return supervisor, nil
	}
	if selection.SavedSupervisor != nil {
		if err = EnsureAdmissionScheduling(store, supervisor, runtime.Control); err != nil {
			return supervisor, err
		}
	}
	queueRepairRunner, err := AdmissionQueueRepairRunner(selection, runtime, runner)
	if err != nil {
		return supervisor, err
	}
	if queueRepairRunner == "" {
		return supervisor, nil
	}
	if err = EnsurePrivateQueuedRunnerRepair(store, supervisor, queueRepairRunner); err != nil {
		return supervisor, fmt.Errorf("repair queued task runner paths: %w", err)
	}
	return supervisor, nil
}

func BindInitialWithStart(selection AdmissionSelection, runtime AdmissionOptions, root string, startPrivate bool) (*pueue.Client, error) {
	if selection.SavedSupervisor != nil {
		return NewClient(context.Background(), root, *selection.SavedSupervisor, runtime.Control, true, runtime.InitialExecutable, runtime.OpenStore)
	}
	configPath, err := ResolveInitialConfig(selection)
	if err != nil {
		return nil, err
	}
	if configPath != "" && root != "" && pueue.IsPrivateConfig(root, configPath) {
		return nil, fmt.Errorf("%w: explicit supervisor config collides with the private state-rooted supervisor", pueue.ErrConfiguration)
	}
	if configPath == "" {
		if root == "" {
			return nil, fmt.Errorf("%w: state root is required for the private supervisor", pueue.ErrConfiguration)
		}
		if !startPrivate {
			clientExecutable, daemonExecutable, resolveErr := ResolvePrivateSupervisorExecutables(root, runtime.InitialExecutable)
			if resolveErr != nil {
				return nil, resolveErr
			}
			return pueue.PreparePrivate(context.Background(), clientExecutable, daemonExecutable, root, runtime.Control)
		}
		return BindPrivateWithContext(context.Background(), root, runtime)
	}
	executable, err := ResolveInitialExecutable(runtime.InitialExecutable)
	if err != nil {
		return nil, err
	}
	return pueue.Bind(context.Background(), executable, configPath, runtime.Control)
}

func BindPrivateWithContext(ctx context.Context, root string, runtime AdmissionOptions) (*pueue.Client, error) {
	if root == "" {
		return nil, fmt.Errorf("%w: state root is required for the private supervisor", pueue.ErrConfiguration)
	}
	clientExecutable, daemonExecutable, err := ResolvePrivateSupervisorExecutables(root, runtime.InitialExecutable)
	if err != nil {
		return nil, err
	}
	return pueue.BindPrivate(ctx, clientExecutable, daemonExecutable, root, runtime.Control)
}
