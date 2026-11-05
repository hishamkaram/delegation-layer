package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func PrepareAdmissionSupervisor(selection AdmissionSelection, runtime AdmissionOptions, store *taskdir.Store, requiresInspection bool, runner string, privateSupervisor bool) (*pueue.Client, error) {
	return PrepareAdmissionSupervisorContext(context.Background(), selection, runtime, store, requiresInspection, runner, privateSupervisor)
}

// The caller owns admission resources carried by parent. Each control phase
// retains its existing budget while deriving from that same context.
func PrepareAdmissionSupervisorContext(parent context.Context, selection AdmissionSelection, runtime AdmissionOptions, store *taskdir.Store, requiresInspection bool, runner string, privateSupervisor bool) (*pueue.Client, error) {
	if parent == nil {
		return nil, context.Canceled
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	var err error
	var supervisor *pueue.Client
	if privateSupervisor {
		control, cancel := ControlContextWithParent(parent, runtime.Control)
		owner := ControlRuntime{Options: runtime.Control, Lookup: runtime.Lookup(), Routing: runtime.Routing}
		if selection.SavedSupervisor == nil {
			supervisor, err = owner.PreparePrivateAdmission(control, store)
		} else if selection.ContinuationSource != (infrastructure.SavedBindingSource{}) {
			supervisor, err = owner.PrepareContinuation(control, store, selection.ContinuationSource)
		} else {
			supervisor, err = owner.RecoverSaved(control, store, *selection.SavedSupervisor, selection.SavedSource)
		}
		cancel()
	} else {
		supervisor, err = BindInitialWithStartContext(parent, selection, runtime, store.Root, !requiresInspection)
	}
	if err != nil {
		return supervisor, err
	}
	if !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return supervisor, nil
	}
	if selection.SavedSupervisor != nil {
		control, cancel := ControlContextWithParent(parent, runtime.Control)
		err = pueue.CheckPrivateAdmissionRuntime(control, store.Root, supervisor.RuntimeBinding())
		if err == nil {
			err = EnsureAdmissionSchedulingContext(control, store.Root, supervisor)
		}
		cancel()
		if err != nil {
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
	if err = ensureAdmissionRunnerRepair(parent, store, supervisor, queueRepairRunner); err != nil {
		return supervisor, fmt.Errorf("repair queued task runner paths: %w", err)
	}
	return supervisor, nil
}

func ensureAdmissionRunnerRepair(parent context.Context, store *taskdir.Store, supervisor *pueue.Client, runner string) error {
	target, err := PlanQueuedRunnerRepairTarget(store, runner)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, pueue.RunnerCommandUpgradeTimeout)
	defer cancel()
	return EnsurePrivateQueuedRunnerRepairContext(ctx, store, supervisor, target)
}

func BindInitialWithStart(selection AdmissionSelection, runtime AdmissionOptions, root string, startPrivate bool) (*pueue.Client, error) {
	return BindInitialWithStartContext(context.Background(), selection, runtime, root, startPrivate)
}

func BindInitialWithStartContext(ctx context.Context, selection AdmissionSelection, runtime AdmissionOptions, root string, startPrivate bool) (*pueue.Client, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if selection.SavedSupervisor != nil {
		return (ControlRuntime{Options: runtime.Control, Lookup: runtime.Lookup(), Routing: runtime.Routing, OpenStore: runtime.OpenStore}).OpenSaved(ctx, root, *selection.SavedSupervisor, selection.SavedSource)
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
		// Preparing config without Start is still generation work. Both cases
		// must arrive through the same independently owned preparation.
		return BindPrivateWithContext(ctx, root, runtime)
	}
	executable, err := ResolveInitialExecutable(runtime.Lookup())
	if err != nil {
		return nil, err
	}
	return pueue.Bind(ctx, executable, configPath, runtime.Control)
}

func BindPrivateWithContext(ctx context.Context, root string, runtime AdmissionOptions) (client *pueue.Client, resultErr error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if root == "" {
		return nil, fmt.Errorf("%w: state root is required for the private supervisor", pueue.ErrConfiguration)
	}
	if runtime.OpenStore == nil {
		return nil, ErrInfrastructureAuthorityUnavailable
	}
	operation, cancel := ControlContextWithParent(ctx, runtime.Control)
	defer cancel()
	store, err := runtime.OpenStore(operation, root)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !CleanupRetainsStore(resultErr, store) {
			resultErr = errors.Join(resultErr, store.Close())
		}
	}()
	return (ControlRuntime{Options: runtime.Control, Lookup: runtime.Lookup(), Routing: runtime.Routing}).PreparePrivateAdmission(operation, store)
}
