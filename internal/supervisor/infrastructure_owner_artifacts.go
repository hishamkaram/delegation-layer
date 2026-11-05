package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// PrepareOutsideInfrastructureArtifacts is the trusted outside producer's
// artifact step. Its caller classifies placement before native birth and supplies
// the managed default runner, independently of a task's custom runner. Existing
// target inventory wins for ordinary preparation, including interrupted inventory
// without a binding. Explicit manual migration selects the current installation.
// Only content is staged here; canonical preparation owns the active binding.
func PrepareOutsideInfrastructureArtifacts(ctx context.Context, target *taskdir.Store, defaultRunner string, lookup ExecutableLookup, initial infrastructure.InitialOperation) (infrastructure.Artifacts, error) {
	if ctx == nil || target == nil {
		return infrastructure.Artifacts{}, os.ErrInvalid
	}
	pin, err := target.PinRootContext(ctx)
	if err != nil {
		return infrastructure.Artifacts{}, err
	}
	client, daemon, err := outsideInfrastructurePair(pin, lookup, initial)
	if err != nil {
		return infrastructure.Artifacts{}, err
	}
	pair, err := stateSupervisorPairIdentity(ctx, client, daemon)
	if err != nil {
		return infrastructure.Artifacts{}, err
	}
	runtime, err := prepareInfrastructureRuntimeSource(ctx, pin.Root(), defaultRunner)
	if err != nil {
		return infrastructure.Artifacts{}, err
	}
	if err = checkInfrastructureTarget(ctx, target, pin); err != nil {
		return infrastructure.Artifacts{}, err
	}
	base := filepath.Join(pin.Root(), ".supervisor")
	if err = stageStateSupervisorPairContentAddressed(ctx, base, client, daemon, pair); err != nil {
		return infrastructure.Artifacts{}, err
	}
	directory := filepath.Join(base, stateSupervisorPairDirectoryPrefix+pair.PairSHA256)
	artifacts := infrastructure.Artifacts{
		Runner: runtime.executable, RunnerSHA256: runtime.sha256,
		Client: filepath.Join(directory, "pueue"), ClientSHA256: pair.ClientSHA256,
		Daemon: filepath.Join(directory, "pueued"), DaemonSHA256: pair.DaemonSHA256,
	}
	if err = ValidateInfrastructureArtifacts(ctx, pin.Root(), artifacts); err != nil {
		return infrastructure.Artifacts{}, err
	}
	return artifacts, checkInfrastructureTarget(ctx, target, pin)
}

// StageInfrastructureOwnerArtifacts derives immutable copies from the current
// resident's own runtime and published pair. The fixed-entry host is its caller;
// RPC input never supplies executable paths. The child owns the borrowed target
// Store and keeps its infrastructure lease through staging and native birth.
// This runs before target generation/bootstrap locks, and never publishes a
// target inventory or binding. Source generation ownership ends before copying.
// Required hashes constrain this one verified source snapshot before any target
// content is staged; they never select executable paths or an epoch pair.
func StageInfrastructureOwnerArtifacts(ctx context.Context, sourceRoot string, accepted infrastructure.Artifacts, requirements infrastructure.ArtifactRequirements, target *taskdir.Store) (infrastructure.Artifacts, error) {
	if ctx == nil || target == nil {
		return infrastructure.Artifacts{}, os.ErrInvalid
	}
	if err := requirements.Validate(); err != nil {
		return infrastructure.Artifacts{}, err
	}
	self, err := os.Executable()
	if err != nil || self != accepted.Runner {
		return infrastructure.Artifacts{}, errors.Join(infrastructure.ErrIdentity, err)
	}
	image, ok := debug.ReadBuildInfo()
	if !ok || !ManagedRunnerBuildInfo(image) {
		return infrastructure.Artifacts{}, infrastructure.ErrIdentity
	}
	runtime := infrastructureRuntimeSource{root: sourceRoot, executable: accepted.Runner, sha256: accepted.RunnerSHA256}
	if err = runtime.validate(ctx); err != nil {
		return infrastructure.Artifacts{}, err
	}
	return stageInfrastructureOwnerSource(ctx, runtime, requirements, target)
}

// The runtime has already been selected and verified by the resident boundary.
// Capture the currently published pair once, independently of the epoch pair.
func stageInfrastructureOwnerSource(ctx context.Context, runtime infrastructureRuntimeSource, requirements infrastructure.ArtifactRequirements, target *taskdir.Store) (infrastructure.Artifacts, error) {
	if err := requirements.Validate(); err != nil {
		return infrastructure.Artifacts{}, err
	}
	pin, err := target.PinRootContext(ctx)
	if err != nil {
		return infrastructure.Artifacts{}, err
	}
	pair, err := readInfrastructurePairSource(ctx, runtime.root)
	if err != nil {
		return infrastructure.Artifacts{}, err
	}
	return stageInfrastructureArtifactCopies(ctx, runtime, pair, requirements, target, pin)
}

// Copies retain the caller's actual Store observation across the existing
// pathname-based installers. Pins detect replacement; they do not turn these
// installers into atomic operations against hostile same-UID path swaps.
func stageInfrastructureArtifactCopies(ctx context.Context, runtime infrastructureRuntimeSource, pair infrastructurePairSource, requirements infrastructure.ArtifactRequirements, target *taskdir.Store, pin taskdir.RootPin) (infrastructure.Artifacts, error) {
	if err := checkInfrastructureTarget(ctx, target, pin); err != nil {
		return infrastructure.Artifacts{}, err
	}
	if err := runtime.validate(ctx); err != nil {
		return infrastructure.Artifacts{}, err
	}
	if err := pair.validate(ctx); err != nil {
		return infrastructure.Artifacts{}, err
	}
	source := infrastructure.Artifacts{
		RunnerSHA256: runtime.sha256,
		ClientSHA256: pair.saved.ClientSHA256,
		DaemonSHA256: pair.saved.DaemonSHA256,
	}
	if err := requirements.ValidateArtifacts(source); err != nil {
		return infrastructure.Artifacts{}, err
	}
	staged, err := runtime.stage(ctx, pin.Root())
	if err != nil {
		return infrastructure.Artifacts{}, err
	}
	if err = checkInfrastructureTarget(ctx, target, pin); err != nil {
		return infrastructure.Artifacts{}, err
	}
	client, daemon, err := pair.stage(ctx, pin.Root())
	if err != nil {
		return infrastructure.Artifacts{}, err
	}
	artifacts := infrastructure.Artifacts{
		Runner: staged.executable, RunnerSHA256: staged.sha256,
		Client: client, ClientSHA256: pair.saved.ClientSHA256,
		Daemon: daemon, DaemonSHA256: pair.saved.DaemonSHA256,
	}
	if err = ValidateInfrastructureArtifacts(ctx, pin.Root(), artifacts); err != nil {
		return infrastructure.Artifacts{}, err
	}
	return artifacts, checkInfrastructureTarget(ctx, target, pin)
}

func checkInfrastructureTarget(ctx context.Context, target *taskdir.Store, expected taskdir.RootPin) error {
	if target == nil {
		return os.ErrInvalid
	}
	current, err := target.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !current.Equal(expected) {
		return infrastructure.ErrIdentity
	}
	return nil
}
