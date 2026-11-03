package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

type infrastructureArtifactsFixture struct {
	origin   string
	target   string
	selected infrastructure.Artifacts
	original infrastructure.Artifacts
}

func stagedInfrastructureArtifacts(t *testing.T) infrastructureArtifactsFixture {
	t.Helper()
	origin, _ := infrastructureSourceFixture(t)
	runtime, err := prepareInfrastructureRuntimeSource(t.Context(), origin, buildInfrastructureRuntime(t))
	if err != nil {
		t.Fatal(err)
	}
	pair, err := readInfrastructurePairSource(t.Context(), origin)
	if err != nil {
		t.Fatal(err)
	}
	target := supervisorfixture.CanonicalTempDir(t)
	staged, err := runtime.stage(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	client, daemon, err := pair.stage(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	return infrastructureArtifactsFixture{
		origin: origin, target: target,
		selected: infrastructure.Artifacts{Runner: staged.executable, RunnerSHA256: staged.sha256, Client: client, ClientSHA256: pair.record.ClientSHA256, Daemon: daemon, DaemonSHA256: pair.record.DaemonSHA256},
		original: infrastructure.Artifacts{Runner: runtime.executable, RunnerSHA256: runtime.sha256, Client: pair.saved.ClientExecutable, ClientSHA256: pair.record.ClientSHA256, Daemon: pair.saved.DaemonExecutable, DaemonSHA256: pair.record.DaemonSHA256},
	}
}

func TestValidateInfrastructureArtifactsChecksTargetCustody(t *testing.T) {
	fixture := stagedInfrastructureArtifacts(t)
	if err := ValidateInfrastructureArtifacts(t.Context(), fixture.target, fixture.selected); err != nil {
		t.Fatal(err)
	}
	requireInfrastructureArtifactSelectionAbsent(t, fixture.target)
	checkInfrastructureArtifactWrongRoots(t, fixture)
	checkInfrastructureArtifactInvalidRunner(t, fixture)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ValidateInfrastructureArtifacts(ctx, fixture.target, fixture.selected); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	// Immutable B custody must no longer depend on A's retained paths.
	if err := os.RemoveAll(filepath.Join(fixture.origin, ".supervisor")); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInfrastructureArtifacts(t.Context(), fixture.target, fixture.selected); err != nil {
		t.Fatalf("B still depends on A paths: %v", err)
	}
	checkInfrastructureArtifactInventoryUnchanged(t, fixture)
	checkChangedInfrastructureArtifactBytes(t, fixture, fixture.selected.Runner)
	checkChangedInfrastructureArtifactBytes(t, fixture, fixture.selected.Client)
}

func checkInfrastructureArtifactWrongRoots(t *testing.T, fixture infrastructureArtifactsFixture) {
	t.Helper()
	for _, member := range []string{"runtime", "pair", "mixed-pair"} {
		t.Run(member, func(t *testing.T) {
			artifacts := fixture.selected
			switch member {
			case "runtime":
				artifacts.Runner, artifacts.RunnerSHA256 = fixture.original.Runner, fixture.original.RunnerSHA256
			case "pair":
				artifacts.Client, artifacts.Daemon = fixture.original.Client, fixture.original.Daemon
			case "mixed-pair":
				artifacts.Client = fixture.original.Client
			}
			if err := ValidateInfrastructureArtifacts(t.Context(), fixture.target, artifacts); err == nil {
				t.Fatalf("A %s paths accepted for B custody", member)
			}
		})
	}
	requireInfrastructureArtifactSelectionAbsent(t, fixture.target)
}

func checkInfrastructureArtifactInvalidRunner(t *testing.T, fixture infrastructureArtifactsFixture) {
	t.Helper()
	fake := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, fake, "fake managed image")
	forged, err := InstallStateRunner(fixture.target, fake)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := fixture.selected
	artifacts.Runner = forged
	artifacts.RunnerSHA256, err = stateExecutableSourceDigest(fake)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateInfrastructureArtifacts(t.Context(), fixture.target, artifacts); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("forged hash filename accepted: %v", err)
	}
	artifacts = fixture.selected
	artifacts.RunnerSHA256 = artifacts.ClientSHA256
	if err = ValidateInfrastructureArtifacts(t.Context(), fixture.target, artifacts); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("changed runtime identity accepted: %v", err)
	}
}

func checkInfrastructureArtifactInventoryUnchanged(t *testing.T, fixture infrastructureArtifactsFixture) {
	t.Helper()
	client, daemon := statePairStagingSources(t, "different inventory client", "different inventory daemon")
	if err := installStateSupervisorPair(t.Context(), fixture.target, client, daemon); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(fixture.target, ".supervisor")
	before, err := os.ReadFile(filepath.Join(base, stateSupervisorPairRecordName))
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateInfrastructureArtifacts(t.Context(), fixture.target, fixture.selected); err != nil {
		t.Fatalf("unpublished staged pair depended on inventory selection: %v", err)
	}
	requireStatePairSelectionUnchanged(t, base, before, true)
	if _, found, readErr := pueue.ReadPrivateSupervisorBinding(fixture.target); readErr != nil || found {
		t.Fatalf("validation published target binding: %v %v", found, readErr)
	}
}

func requireInfrastructureArtifactSelectionAbsent(t *testing.T, root string) {
	t.Helper()
	requireStatePairSelectionUnchanged(t, filepath.Join(root, ".supervisor"), nil, false)
	if _, found, err := pueue.ReadPrivateSupervisorBinding(root); err != nil || found {
		t.Fatalf("validation published target binding: %v %v", found, err)
	}
}

func TestValidateInfrastructureArtifactsZeroDoesNotCreateState(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	if err := ValidateInfrastructureArtifacts(t.Context(), root, infrastructure.Artifacts{}); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("zero artifacts accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal created target artifacts: %v", err)
	}
}

func checkChangedInfrastructureArtifactBytes(t *testing.T, fixture infrastructureArtifactsFixture, path string) {
	t.Helper()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("changed artifact bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = ValidateInfrastructureArtifacts(t.Context(), fixture.target, fixture.selected); !errors.Is(err, pueue.ErrConfiguration) {
		t.Errorf("changed artifact bytes accepted: %v", err)
	}
	if err = os.WriteFile(path, original, 0o700); err != nil {
		t.Fatal(err)
	}
}
