package infrastructurehost

import (
	"context"
	"os"
	"runtime"
	"runtime/debug"

	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

// Host is fixed application composition, never a Dependencies override.
// supervisor/infrastructure owns native authority and serving lifetime;
// this host supplies application policy and installed-runner validation.

var _ infrastructure.EntryHost = Host{}

func (Host) ValidateRunner(ctx context.Context, root, runner string, image *debug.BuildInfo) error {
	if ctx == nil {
		return os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !installation.ManagedRunnerBuildInfo(image) {
		return infrastructure.ErrIdentity
	}
	if err := pueue.ValidateUnixSocketPathBytes(runtime.GOOS, infrastructure.OwnerEndpointPathBytes(root)); err != nil {
		return err
	}
	selected, err := installation.ResolveRunner(runner, "")
	if err != nil {
		return err
	}
	if selected != runner {
		return infrastructure.ErrIdentity
	}
	return ctx.Err()
}

func (h Host) ensureOwnRoot(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, guards infrastructure.ResidentGuards, wait infrastructure.DaemonWaitOwner) (disposition infrastructure.Disposition, resultErr error) {
	preparation, err := h.openPreparation(ctx, operation, selected, guards, wait)
	defer func() {
		disposition = preparation.disposition()
		resultErr = preparation.close(resultErr)
	}()
	if err != nil {
		return infrastructure.Disposition{}, err
	}
	client, prepareErr := supervisor.PreparePrivateAdmissionSupervisorContext(ctx, preparation.lookup, preparation.store, preparation.options)
	if client != nil && prepareErr == nil {
		preparation.observation.RecordReady()
	}
	return infrastructure.Disposition{}, supervisor.ClosePrivateAdmissionTransition(client, prepareErr)
}
