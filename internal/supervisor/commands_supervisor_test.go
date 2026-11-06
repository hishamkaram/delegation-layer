package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func TestConfiguredSupervisorDoesNotFallBackToPath(t *testing.T) {
	pathDir := t.TempDir()
	for _, name := range []string{"pueue", "pueued"} {
		path := filepath.Join(pathDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", pathDir)
	for _, missing := range []string{"pueue", "pueued"} {
		t.Run(missing, func(t *testing.T) {
			directory := t.TempDir()
			for _, name := range []string{"pueue", "pueued"} {
				if name != missing {
					supervisorfixture.WriteExecutable(t, filepath.Join(directory, name), "#!/bin/sh\n")
				}
			}
			lookup := ExecutableLookup{InitialExecutable: filepath.Join(directory, "pueue")}
			if _, _, err := ResolvePrivateSupervisorExecutables("", lookup); !errors.Is(err, pueue.ErrConfiguration) {
				t.Fatalf("configured supervisor missing %s fell back to PATH: %v", missing, err)
			}
		})
	}
}

func TestPrivateSupervisorPairDoesNotMixExecutables(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "pueue"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveSupervisorPair(directory); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("partial supervisor bundle was accepted: %v", err)
	}
}

func TestBundledSupervisorPairResolvesFromHomebrewLibexec(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "Cellar", "delegation-layer", "1.0", "bin")
	libexec := filepath.Join(root, "Cellar", "delegation-layer", "1.0", "libexec")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(libexec, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pueue", "pueued"} {
		if err := os.WriteFile(filepath.Join(libexec, name), []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	expectedClient, err := filepath.EvalSymlinks(filepath.Join(libexec, "pueue"))
	if err != nil {
		t.Fatal(err)
	}
	expectedDaemon, err := filepath.EvalSymlinks(filepath.Join(libexec, "pueued"))
	if err != nil {
		t.Fatal(err)
	}

	client, daemon, found, err := resolveBundledLibexecPair(bin)
	if err != nil || !found || client != expectedClient || daemon != expectedDaemon {
		t.Fatalf("Homebrew libexec pair was not resolved: client=%q daemon=%q found=%t err=%v", client, daemon, found, err)
	}
}

func TestManagedRunnerResolvesFromSymlinkedHomebrewExecutable(t *testing.T) {
	root := t.TempDir()
	cellarBin := filepath.Join(root, "Cellar", "delegation-layer", "1.0", "bin")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(cellarBin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	delegate := filepath.Join(cellarBin, "delegate")
	runner := filepath.Join(cellarBin, "delegate-run")
	supervisorfixture.WriteExecutable(t, delegate, "delegate")
	supervisorfixture.WriteExecutable(t, runner, "delegate-run")
	linkedDelegate := filepath.Join(bin, "delegate")
	if err := os.Symlink(delegate, linkedDelegate); err != nil {
		t.Fatal(err)
	}

	got := managedRunnerFromExecutable(linkedDelegate)
	want, err := filepath.EvalSymlinks(runner)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("managed runner = %q, want canonical sibling %q", got, want)
	}

	linkedRunner := filepath.Join(bin, "delegate-run")
	if linkErr := os.Symlink(runner, linkedRunner); linkErr != nil {
		t.Fatal(linkErr)
	}
	selected, err := ResolveRunner(linkedRunner, "")
	if err != nil || selected != want {
		t.Fatalf("explicit installed alias = %q, want canonical runner %q: %v", selected, want, err)
	}
}
