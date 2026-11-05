package supervisor

import (
	"context"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ObserveOutsideInfrastructureRequirements observes the same canonical source
// selectors as outside preparation without staging target content. Trusted app
// composition supplies the managed default runner after classifying placement.
// These constraints can join an existing attempt; only preparation under the
// instance lease validates and copies the eventual sources. A source change
// between observation and preparation refuses compatibility rather than retargeting.
func ObserveOutsideInfrastructureRequirements(ctx context.Context, target *taskdir.Store, runner string, lookup ExecutableLookup, initial infrastructure.InitialOperation) (infrastructure.ArtifactRequirements, error) {
	if ctx == nil || target == nil {
		return infrastructure.ArtifactRequirements{}, os.ErrInvalid
	}
	pin, err := target.PinRootContext(ctx)
	if err != nil {
		return infrastructure.ArtifactRequirements{}, err
	}
	client, daemon, err := outsideInfrastructurePair(pin, lookup, initial)
	if err != nil {
		return infrastructure.ArtifactRequirements{}, err
	}
	pair, err := stateSupervisorPairIdentity(ctx, client, daemon)
	if err != nil {
		return infrastructure.ArtifactRequirements{}, err
	}
	runtimeSHA256, err := observeOutsideInfrastructureRuntime(ctx, target, pin, runner)
	if err != nil {
		return infrastructure.ArtifactRequirements{}, err
	}
	return infrastructure.ArtifactRequirements{RequiredRuntimeSHA256: runtimeSHA256, RequiredClientSHA256: pair.ClientSHA256, RequiredDaemonSHA256: pair.DaemonSHA256}, nil
}

// ObserveOutsideInfrastructureRuntime verifies the actual caller-selected
// managed Runner without reading a target Pueue selection. An authenticated
// existing owner's epoch supplies the independent pair constraints instead.
// This observation stages no content and grants no native placement authority.
func ObserveOutsideInfrastructureRuntime(ctx context.Context, target *taskdir.Store, runner string) (string, error) {
	if ctx == nil || target == nil {
		return "", os.ErrInvalid
	}
	pin, err := target.PinRootContext(ctx)
	if err != nil {
		return "", err
	}
	return observeOutsideInfrastructureRuntime(ctx, target, pin, runner)
}

func observeOutsideInfrastructureRuntime(ctx context.Context, target *taskdir.Store, pin taskdir.RootPin, runner string) (string, error) {
	digest, err := infrastructureRuntimeCandidate(runner)
	if err != nil {
		return "", err
	}
	if err = checkInfrastructureTarget(ctx, target, pin); err != nil {
		return "", err
	}
	return digest, nil
}

// The accepted purpose owns source selection. Manual migration explicitly asks
// for the current installation; all other preparation preserves target inventory.
// This CLI-side observation selects content only, never resident native authority.
func outsideInfrastructurePair(pin taskdir.RootPin, lookup ExecutableLookup, initial infrastructure.InitialOperation) (string, string, error) {
	if err := initial.Validate(pin.RootID()); err != nil {
		return "", "", err
	}
	if initial.Kind == infrastructure.InitialMigrateOwn {
		return ResolveCurrentPrivateSupervisorExecutables(pin.Root(), lookup)
	}
	return ResolvePrivateSupervisorExecutables(pin.Root(), lookup)
}
