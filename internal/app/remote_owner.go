package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func supervisorEnsureOwner(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("supervisor")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	options := supervisorControlOptions(deps)
	ctx, cancel := supervisorcore.ControlContext(options)
	defer cancel()
	store, err := openStoreContext(ctx, root, deps.storeDependencies(), true)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	control := supervisorcore.ControlRuntime{Options: options, Lookup: deps.supervisorLookup(), Routing: deps.infrastructureRouting()}
	client, err := control.PreparePrivateAdmission(ctx, store)
	if client != nil {
		err = errors.Join(err, client.Close())
	}
	response.RootID = store.RootID
	if err != nil {
		return failed(response, err, 1)
	}
	response.Status = "owner_ready"
	return commandResult{response: response}
}

func requireRemoteResident(ctx context.Context, root string, deps Dependencies) (resultErr error) {
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.storeDependencies().registry())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	process, err := infrastructure.ObserveOwnerProcess(ctx, reader)
	if err != nil {
		return errors.New("remote supervisor is not ready; run supervisor ensure-owner in a prepared persistent user context")
	}
	ref, err := process.Reference()
	if err != nil {
		return err
	}
	return lifetime.RequirePersistentProcess(ref)
}
