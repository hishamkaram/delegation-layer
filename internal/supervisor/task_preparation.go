package supervisor

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func PrepareTaskSupervisor(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, baseOptions pueue.Options, initialExecutable, runnerExecutable string) (*pueue.Client, error) {
	if store == nil || meta == nil || !pueue.IsPrivateConfig(store.Root, binding.ConfigPath) {
		return nil, nil
	}
	if ctx == nil {
		return nil, context.Canceled
	}
	ctx, cancel := context.WithTimeout(ctx, pueue.RunnerCommandUpgradeTimeout)
	defer cancel()
	options := OptionsWithEnvironment(baseOptions, meta.Environment)
	client, err := PreparePrivateAdmissionSupervisorContext(ctx, initialExecutable, store, options)
	if err != nil {
		return nil, err
	}
	return prepareTaskSupervisorRunner(ctx, store, client, binding, runnerExecutable)
}

func prepareTaskSupervisorRunner(ctx context.Context, store *taskdir.Store, client *pueue.Client, binding task.SupervisorRef, runnerExecutable string) (*pueue.Client, error) {
	target, err := PlanQueuedRunnerRepairTarget(store, runnerExecutable)
	if err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	if err = EnsurePrivateQueuedRunnerRepairContext(ctx, store, client, target); err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	if err = client.ScopePrivateTask(ctx, store.Root, binding); err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	return client, nil
}
