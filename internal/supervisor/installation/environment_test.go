package installation

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func TestSupervisorLookupResolvesPathFromSelectedScope(t *testing.T) {
	ambientDirectory := writeScopeSupervisorPair(t, "ambient")
	selectedDirectory := writeScopeSupervisorPair(t, "selected")
	t.Setenv("PATH", ambientDirectory)
	ambient := os.Environ()

	selected := ExecutableLookup{Environment: []string{"PATH=" + selectedDirectory}}
	initial, err := ResolveInitialExecutable(selected)
	if err != nil || initial != filepath.Join(selectedDirectory, "pueue") {
		t.Fatalf("initial executable = %q/%v, want selected scope", initial, err)
	}
	client, daemon, err := ResolvePrivateSupervisorExecutables(selected)
	if err != nil || client != filepath.Join(selectedDirectory, "pueue") || daemon != filepath.Join(selectedDirectory, "pueued") {
		t.Fatalf("private PATH fallback = %q/%q/%v, want selected scope", client, daemon, err)
	}
	if _, err = ResolveInitialExecutable(ExecutableLookup{Environment: []string{}}); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("empty scope resolved through the process PATH: %v", err)
	}
	standalone, err := ResolveInitialExecutable(ExecutableLookup{})
	if err != nil || standalone != filepath.Join(ambientDirectory, "pueue") {
		t.Fatalf("standalone lookup = %q/%v, want process PATH", standalone, err)
	}
	if !slices.Equal(os.Environ(), ambient) {
		t.Fatal("scoped lookup changed the process environment")
	}
}

func writeScopeSupervisorPair(t *testing.T, label string) string {
	t.Helper()
	directory := supervisorfixture.CanonicalTempDir(t)
	for _, name := range []string{"pueue", "pueued"} {
		supervisorfixture.WriteExecutable(t, filepath.Join(directory, name), label+" "+name)
	}
	return directory
}
