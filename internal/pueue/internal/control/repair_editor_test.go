package control

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func repairEditorTestStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStoreWithPredicatesContext(t.Context(), filepath.Join(canonicalTemp(t), "state"), predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store
}

func repairEditorSupervisorDirectory(t *testing.T, store *taskdir.Store) string {
	t.Helper()
	path := filepath.Join(store.Root, privateSupervisorDirectory)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func repairEditorTestParent(t *testing.T, path string) *controlRepairParent {
	t.Helper()
	parent := filepath.Dir(path)
	var stat unix.Stat_t
	if err := unix.Lstat(parent, &stat); err != nil {
		t.Fatal(err)
	}
	return &controlRepairParent{Path: parent, Identity: controlLeaseIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}}
}

func repairEditorTestTree(t *testing.T) (string, string) {
	t.Helper()
	return repairEditorTestTreeIn(t, canonicalTemp(t))
}

func repairEditorTestTreeIn(t *testing.T, parent string) (string, string) {
	t.Helper()
	base := filepath.Join(parent, "quote' ! café")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "7"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "7", "command")
	if err := os.WriteFile(path, []byte("old command"), 0o644); err != nil {
		t.Fatal(err)
	}
	return base, path
}

func TestRepairCommandNarrowsNativeUmaskEntries(t *testing.T) {
	base, path := repairEditorTestTree(t)
	if err := os.Chmod(filepath.Dir(path), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := replaceRepairCommand(base, "7", "old command", "replacement", *repairEditorTestParent(t, base)); err != nil {
		t.Fatal(err)
	}
	for selected, expectedMode := range map[string]os.FileMode{filepath.Dir(path): 0o700, path: 0o600} {
		info, err := os.Stat(selected)
		if err != nil || info.Mode().Perm() != expectedMode {
			t.Fatalf("native entry was not narrowed before I/O: %v %v", info, err)
		}
	}
}

func TestRepairCommandRejectsMismatchAndUnsafeEntries(t *testing.T) {
	for _, scenario := range []string{"mismatch", "symlink-command", "symlink-task", "symlink-root", "hardlink", "special-command", "special-task", "public-parent"} {
		t.Run(scenario, func(t *testing.T) {
			base, path := repairEditorTestTree(t)
			applyUnsafeRepairEntry(t, scenario, base, path)
			if err := replaceRepairCommand(base, "7", "old command", "replacement", *repairEditorTestParent(t, base)); err == nil {
				t.Fatal("unsafe or changed native editor input accepted")
			}
			if scenario == "mismatch" {
				if content, err := os.ReadFile(path); err != nil || string(content) != "changed command" {
					t.Fatalf("comparison failure changed native command: %q %v", content, err)
				}
			}
		})
	}
}

func applyUnsafeRepairEntry(t *testing.T, scenario, base, path string) {
	t.Helper()
	switch scenario {
	case "mismatch":
		if err := os.WriteFile(path, []byte("changed command"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "symlink-command", "symlink-task", "symlink-root":
		selected := path
		switch scenario {
		case "symlink-task":
			selected = filepath.Dir(path)
		case "symlink-root":
			selected = base
		}
		if err := os.Rename(selected, selected+"-target"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(selected+"-target", selected); err != nil {
			t.Fatal(err)
		}
	case "hardlink":
		if err := os.Link(path, path+"-other"); err != nil {
			t.Fatal(err)
		}
	case "special-command", "special-task", "public-parent":
		selected, mode := path, os.FileMode(0o600)|os.ModeSetuid
		switch scenario {
		case "special-task":
			selected, mode = filepath.Dir(path), 0o700|os.ModeSticky
		case "public-parent":
			selected, mode = filepath.Dir(base), 0o755
		}
		if err := os.Chmod(selected, mode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepairConfigPublishesBoundManagedImage(t *testing.T) {
	base := canonicalTemp(t)
	source := []byte(privateConfigYAML(base))
	path := filepath.Join(base, "source.yml")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	editor, err := canonicalExecutable(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	derived, err := privateRepairConfig(base, path, task.ComputeSHA256(source), editor)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(derived), "pueue-repair-") {
		t.Fatal("repair reused legacy derived config")
	}
	if _, err = privateRepairConfig(base, path, strings.Repeat("0", 64), editor); !errors.Is(err, ErrBinding) {
		t.Fatalf("changed source accepted: %v", err)
	}
}

func TestRepairParentRejectsReplacedNativeDirectory(t *testing.T) {
	store := repairEditorTestStore(t)
	path := repairEditorSupervisorDirectory(t, store)
	parent, err := canonicalRepairParent(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = replaceRepairCommand(filepath.Join(parent.Path, "repair"), "task", "old", "new", parent); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("same-path replacement acquired repair authority: %v", err)
	}
}

func TestRepairActuatorOverridesHostileTemp(t *testing.T) {
	store := repairEditorTestStore(t)
	repairEditorSupervisorDirectory(t, store)
	parent, err := canonicalRepairParent(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	original := []string{"TMPDIR=/unavailable/hostile-temp", repairParentEnv + "=unsafe"}
	derived := repairActuatorEnvironment(original, &parent)
	if original[0] != "TMPDIR=/unavailable/hostile-temp" || environmentValue(derived, "TMPDIR") != parent.Path || environmentValue(derived, repairParentEnv) != parent.Path {
		t.Fatal("repair environment did not derive the private native parent immutably")
	}
}
