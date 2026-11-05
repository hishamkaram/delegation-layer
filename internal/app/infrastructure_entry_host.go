package app

import (
	"context"
	"runtime"
	"runtime/debug"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// infrastructureEntryHost is fixed application composition. It is never a
// Dependencies override. The child owns native authority and serving lifetime;
// this host supplies existing application policy and artifact-copy consumers.
type infrastructureEntryHost struct{}

var _ infrastructure.EntryHost = infrastructureEntryHost{}

func (infrastructureEntryHost) ValidateArtifacts(ctx context.Context, root string, artifacts infrastructure.Artifacts, image *debug.BuildInfo) error {
	if !supervisor.ManagedRunnerBuildInfo(image) {
		return infrastructure.ErrIdentity
	}
	if err := pueue.ValidateUnixSocketPathBytes(runtime.GOOS, infrastructure.OwnerEndpointPathBytes(root)); err != nil {
		return err
	}
	return supervisor.ValidateInfrastructureArtifacts(ctx, root, artifacts)
}

func (infrastructureEntryHost) EnsureOwnRoot(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (disposition infrastructure.Disposition, resultErr error) {
	preparation, err := openInfrastructurePreparation(ctx, operation, selected, guards, wait)
	defer func() {
		disposition = preparation.disposition()
		resultErr = preparation.close(resultErr)
	}()
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	ctx = pueue.WithControlLease(ctx, preparation.admission)
	client, prepareErr := supervisor.PreparePrivateAdmissionSupervisorContext(ctx, preparation.lookup, preparation.store, preparation.options)
	if client != nil && prepareErr == nil {
		preparation.observation.RecordReady()
	}
	return infrastructure.Disposition{}, supervisor.ClosePrivateAdmissionTransition(client, prepareErr)
}

func (infrastructureEntryHost) StageTargetArtifacts(ctx context.Context, operation infrastructure.Operation, requirements infrastructure.ArtifactRequirements, target *taskdir.Store) (infrastructure.Artifacts, error) {
	if err := operation.Validate(); err != nil {
		return infrastructure.Artifacts{}, err
	}
	return supervisor.StageInfrastructureOwnerArtifacts(ctx, operation.Intent().Inputs.Root, operation.Artifacts(), requirements, target)
}
