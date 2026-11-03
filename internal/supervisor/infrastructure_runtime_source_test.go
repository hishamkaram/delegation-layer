package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func buildInfrastructureRuntime(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locating repository for managed runner build")
	}
	repository := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	binary := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	// This finite build owns its Start and Wait through CombinedOutput. No
	// provider or resident process is started by the fixture.
	command := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "./cmd/delegate-run")
	command.Dir = repository
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("building actual managed runner: %v: %s", err, output)
	}
	return binary
}

func TestInfrastructureRuntimeSourceFreezesManagedBytes(t *testing.T) {
	candidate := buildInfrastructureRuntime(t)
	expected, err := stateExecutableSourceDigest(candidate)
	if err != nil {
		t.Fatal(err)
	}
	origin, target := supervisorfixture.CanonicalTempDir(t), supervisorfixture.CanonicalTempDir(t)
	source, err := prepareInfrastructureRuntimeSource(t.Context(), origin, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if source.sha256 != expected || source.executable == candidate {
		t.Fatal("source did not freeze selected runtime")
	}
	// Replace the mutable installation after trusted copying, before target stage.
	if err = os.WriteFile(candidate, []byte("replaced installation"), 0o700); err != nil {
		t.Fatal(err)
	}
	staged, err := source.stage(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if staged.sha256 != expected || staged.executable == source.executable {
		t.Fatal("target did not derive independent frozen custody")
	}
	if err = os.RemoveAll(filepath.Join(origin, ".supervisor")); err != nil {
		t.Fatal(err)
	}
	if err = staged.validate(t.Context()); err != nil {
		t.Fatalf("target still depends on origin paths: %v", err)
	}
	// Replacing an owned source is detected before any third-root artifact exists.
	if err = os.WriteFile(staged.executable, []byte("changed custody"), 0o700); err != nil {
		t.Fatal(err)
	}
	rejectedTarget := supervisorfixture.CanonicalTempDir(t)
	if _, err = staged.stage(t.Context(), rejectedTarget); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("changed custody accepted: %v", err)
	}
	requireNoInfrastructureRuntimeArtifacts(t, rejectedTarget)
}

func TestInfrastructureRuntimeSourceRejectsUnmanagedCandidates(t *testing.T) {
	fake := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, fake, "arbitrary executable")
	ordinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err = filepath.EvalSymlinks(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{fake, ordinary} {
		t.Run(filepath.Base(candidate), func(t *testing.T) {
			root := supervisorfixture.CanonicalTempDir(t)
			if _, prepareErr := prepareInfrastructureRuntimeSource(t.Context(), root, candidate); !errors.Is(prepareErr, pueue.ErrConfiguration) {
				t.Fatalf("unmanaged candidate accepted: %v", prepareErr)
			}
			requireNoInfrastructureRuntimeArtifacts(t, root)
		})
	}
	// A protected content-addressed filename alone is not managed provenance.
	root := supervisorfixture.CanonicalTempDir(t)
	forged, err := InstallStateRunner(root, fake)
	if err != nil {
		t.Fatal(err)
	}
	if !CurrentManagedRunnerExecutable(root, forged) {
		t.Fatal("fixture did not exercise hash-shaped filename shortcut")
	}
	if _, err = prepareInfrastructureRuntimeSource(t.Context(), root, forged); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("forged state filename accepted: %v", err)
	}
}

func TestInfrastructureRuntimeSourceRejectsInvalidInputs(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	if _, err := (infrastructureRuntimeSource{}).stage(t.Context(), root); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("zero source: %v", err)
	}
	requireNoInfrastructureRuntimeArtifacts(t, root)
	fake := filepath.Join(supervisorfixture.CanonicalTempDir(t), "fake")
	supervisorfixture.WriteExecutable(t, fake, "fake")
	alias := filepath.Join(filepath.Dir(fake), "alias")
	if err := os.Symlink(fake, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareInfrastructureRuntimeSource(t.Context(), root, alias); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("aliased candidate: %v", err)
	}
	if _, err := prepareInfrastructureRuntimeSource(t.Context(), root, "relative"); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("relative candidate: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := prepareInfrastructureRuntimeSource(ctx, root, fake); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled source: %v", err)
	}
	requireNoInfrastructureRuntimeArtifacts(t, root)
}

func requireNoInfrastructureRuntimeArtifacts(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused source created artifacts: %v", err)
	}
}
