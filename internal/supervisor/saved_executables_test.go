package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func installedSelectionFixture(t *testing.T) (string, task.SupervisorRef, installation.ExecutableLookup) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "state")
	binding := task.SupervisorRef{
		ConfigPath: filepath.Join(root, ".supervisor", "pueue.yml"), ConfigDigest: strings.Repeat("a", 64),
		ResolvedConfigSHA256: strings.Repeat("b", 64), Endpoint: "unix:" + filepath.Join(root, "run", "p.sock"), ObservedVersion: "pueue 4.0.4",
		ClientExecutable: filepath.Join(base, "recorded", "pueue"), ClientSHA256: strings.Repeat("c", 64),
		DaemonExecutable: filepath.Join(base, "recorded", "pueued"), DaemonSHA256: strings.Repeat("d", 64),
	}
	return root, binding, installation.ExecutableLookup{InitialExecutable: filepath.Join(base, "installed", "pueue")}
}

func TestResolveSavedSupervisorClientPreservesRecordedInstallation(t *testing.T) {
	root, saved, lookup := installedSelectionFixture(t)
	selectionExecutable(t, saved.ClientExecutable)
	got, err := ResolveSavedSupervisorClient(root, saved, lookup)
	if err != nil || got != saved.ClientExecutable {
		t.Fatalf("recorded client=%q err=%v", got, err)
	}
	if _, err = os.Stat(saved.DaemonExecutable); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon fixture unexpectedly available: %v", err)
	}
}

func TestResolveSavedSupervisorClientSelectsInstalledRoleForLegacyCopy(t *testing.T) {
	root, saved, lookup := installedSelectionFixture(t)
	saved.ClientExecutable = filepath.Join(root, ".supervisor", "pueue-pair-"+strings.Repeat("e", 64), "pueue")
	selectionExecutable(t, saved.ClientExecutable)
	installed := filepath.Join(filepath.Dir(lookup.InitialExecutable), "pueue")
	selectionExecutable(t, installed)
	got, err := ResolveSavedSupervisorClient(root, saved, lookup)
	if err != nil || got != installed {
		t.Fatalf("installed client=%q err=%v", got, err)
	}
}

func TestResolveSavedSupervisorClientFallsBackOnlyForAbsence(t *testing.T) {
	root, saved, lookup := installedSelectionFixture(t)
	installed := filepath.Join(filepath.Dir(lookup.InitialExecutable), "pueue")
	selectionExecutable(t, installed)
	got, err := ResolveSavedSupervisorClient(root, saved, lookup)
	if err != nil || got != installed {
		t.Fatalf("missing client fallback=%q err=%v", got, err)
	}
	if err = os.MkdirAll(saved.ClientExecutable, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err = ResolveSavedSupervisorClient(root, saved, lookup); err == nil || got != "" {
		t.Fatalf("invalid recorded member fell back: %q %v", got, err)
	}
}

func selectionExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture, never executed or read"), 0o700); err != nil {
		t.Fatal(err)
	}
}
