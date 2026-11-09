package remotedata

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestLayoutPathsAndClassifier(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	rid, id := strings.Repeat("a", 32), strings.Repeat("b", 32)
	for _, tc := range []struct {
		layout   task.RemoteDataLayout
		family   Family
		relative string
		role     Role
	}{{0, Inputs, filepath.Join(".state.remote-inputs", rid, id), Destination}, {2, Inputs, filepath.Join(".state.remote-data", rid, id, "inputs"), Destination}, {2, Snapshot, filepath.Join(".state.remote-data", rid, id, "snapshot"), Origin}, {2, Outputs, filepath.Join(".state.remote-data", rid, id, "outputs"), Destination}} {
		path, err := Path(tc.layout, state, rid, id, tc.family)
		if err != nil || path != filepath.Join(base, tc.relative) {
			t.Fatalf("path=%q %v", path, err)
		}
		loc, managed, err := Classify(filepath.Join(path, "workspace", "nested"))
		if err != nil || !managed || loc.Root != state || loc.RootID != rid || loc.TaskID != id || loc.Layout != tc.layout || loc.Family != tc.family || loc.Role != tc.role {
			t.Fatalf("classify=%+v %v %v", loc, managed, err)
		}
	}
	for _, path := range []string{base, filepath.Join(base, ".state.remote-data", "bad", "bad", "inputs"), "relative"} {
		if _, yes, e := Classify(path); e != nil || yes {
			t.Fatalf("unmanaged path %q: %v %v", path, yes, e)
		}
	}
}

func TestOpenAndRemoveOnlySelectedRole(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	rid, id := strings.Repeat("a", 32), strings.Repeat("b", 32)
	for _, family := range []Family{Snapshot, Inputs, Outputs} {
		r, err := Open(2, state, rid, id, family, true)
		if err != nil {
			t.Fatal(err)
		}
		f, err := r.Create("owned")
		if err != nil {
			t.Fatal(err)
		}
		if err = errors.Join(f.Close(), r.Close()); err != nil {
			t.Fatal(err)
		}
	}
	if err := Remove(2, state, rid, id, Snapshot); err != nil {
		t.Fatal(err)
	}
	for _, family := range []Family{Inputs, Outputs} {
		r, err := Open(2, state, rid, id, family, false)
		if err != nil {
			t.Fatal("sibling removed", err)
		}
		if err = r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(2, state, rid, id, Snapshot, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot survived %v", err)
	}
}

func TestOpenRejectsSymlinkAndNonprivateDirectory(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	rid, id := strings.Repeat("a", 32), strings.Repeat("b", 32)
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(base, ".state.remote-data")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(2, state, rid, id, Inputs, true); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Remove(filepath.Join(base, ".state.remote-data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, ".state.remote-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(2, state, rid, id, Inputs, true); err == nil {
		t.Fatal("nonprivate accepted")
	}
}

func TestRemovalRetryBarriersVisibleAbsence(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := root.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = root.Mkdir("family", 0o700); err != nil {
		t.Fatal(err)
	}
	fault := errors.New("directory barrier failed")
	if err = removeFamily(root, "family", func(*os.Root) error { return fault }); !errors.Is(err, fault) {
		t.Fatalf("initial barrier fault lost: %v", err)
	}
	if _, err = root.Lstat("family"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture did not remove family before barrier: %v", err)
	}
	called := false
	if err = removeFamily(root, "family", func(*os.Root) error { called = true; return fault }); !called || !errors.Is(err, fault) {
		t.Fatalf("retry acknowledged absence without its barrier: called=%v error=%v", called, err)
	}
}

func TestMissingAncestorBarriersNearestValidatedParent(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "owned")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = syncOwnedAbsence(base, filepath.Join(parent, "absent", "task"), func(root *os.Root) error {
		called = true
		info, statErr := root.Stat(".")
		if statErr != nil || !os.SameFile(info, want) {
			return errors.Join(statErr, errors.New("wrong absence barrier parent"))
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("nearest parent barrier: called=%v error=%v", called, err)
	}
}
