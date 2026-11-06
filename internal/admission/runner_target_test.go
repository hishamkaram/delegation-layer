package admission

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestManagedSuccessorTargetLeavesUnchangedImageIndependentOfLookup(t *testing.T) {
	recorded := filepath.Join(t.TempDir(), "already-running-image")
	_, err := managedSuccessorTarget(filepath.Join(t.TempDir(), "missing-install"), recorded, recorded)
	if err != nil {
		t.Fatalf("unchanged native-owned image required another lookup: %v", err)
	}
}

func TestManagedSuccessorTargetComesFromConfiguredRole(t *testing.T) {
	directory := supervisorfixture.CanonicalPath(t, t.TempDir())
	configured, other := filepath.Join(directory, "installed-worker"), filepath.Join(directory, "other-worker")
	for _, path := range []string{configured, other} {
		if err := os.WriteFile(path, []byte("not executed"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	target, err := managedSuccessorTarget(configured, "/old-recorded-worker", other)
	if err != nil {
		t.Fatal(err)
	}
	if err = target.ValidateExecutable(other); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("candidate supplied its own selected role: %v", err)
	}
	if err = target.ValidateExecutable(configured); err != nil {
		t.Fatal(err)
	}
}
