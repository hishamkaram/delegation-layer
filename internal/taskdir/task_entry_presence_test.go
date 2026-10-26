package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

type entrySnapshot struct {
	mode  os.FileMode
	size  int64
	mtime int64
	link  string
}

// snapshotTree records every entry below root without following symlinks.
func snapshotTree(t *testing.T, root string) map[string]entrySnapshot {
	t.Helper()
	snap := map[string]entrySnapshot{}
	must(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		entry := entrySnapshot{mode: info.Mode(), size: info.Size(), mtime: info.ModTime().UnixNano()}
		if info.Mode()&os.ModeSymlink != 0 {
			if entry.link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		snap[path] = entry
		return nil
	}))
	return snap
}

func newTaskID(t *testing.T) string {
	t.Helper()
	id, err := randomid.NewTaskID()
	must(t, err)
	return id
}

// tasksDir creates the private tasks directory, which InitStore leaves to the
// first CreateTask.
func tasksDir(t *testing.T, s *Store) string {
	t.Helper()
	dir := filepath.Join(s.Root, "tasks")
	must(t, os.Mkdir(dir, 0o700))
	return dir
}

func requireEntryExists(t *testing.T, s *Store, id string, want bool) {
	t.Helper()
	got, err := s.TaskEntryExists(id)
	must(t, err)
	if got != want {
		t.Fatalf("TaskEntryExists(%s) = %t, want %t", id, got, want)
	}
}

func TestTaskEntryExistsReportsAbsenceAndLeavesStoreUnchanged(t *testing.T) {
	s := testStore(t)
	tasks := tasksDir(t, s)
	id := newTaskID(t)
	before := snapshotTree(t, s.Root)
	requireEntryExists(t, s, id, false)
	if after := snapshotTree(t, s.Root); !reflect.DeepEqual(before, after) {
		t.Fatalf("absence check changed the store:\nbefore=%v\nafter=%v", before, after)
	}
	if _, err := os.Lstat(filepath.Join(tasks, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absence check created the entry: %v", err)
	}
}

func TestTaskEntryExistsTreatsMissingTasksDirectoryAsAbsent(t *testing.T) {
	s := testStore(t)
	if _, err := os.Lstat(filepath.Join(s.Root, "tasks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh store already has a tasks directory: %v", err)
	}
	before := snapshotTree(t, s.Root)
	requireEntryExists(t, s, newTaskID(t), false)
	if after := snapshotTree(t, s.Root); !reflect.DeepEqual(before, after) {
		t.Fatalf("missing tasks directory check changed the store:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestTaskEntryExistsReportsPreparedTask(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	before := snapshotTree(t, s.Root)
	requireEntryExists(t, s, td.TaskID, true)
	if after := snapshotTree(t, s.Root); !reflect.DeepEqual(before, after) {
		t.Fatalf("presence check changed the store:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestTaskEntryExistsReportsPartialDirectoryMissingLocks(t *testing.T) {
	s := testStore(t)
	id := newTaskID(t)
	must(t, os.Mkdir(filepath.Join(tasksDir(t, s), id), 0o700))
	if _, err := s.OpenTask(id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenTask on a partial directory = %v, want not-exist", err)
	}
	before := snapshotTree(t, s.Root)
	requireEntryExists(t, s, id, true)
	if after := snapshotTree(t, s.Root); !reflect.DeepEqual(before, after) {
		t.Fatalf("presence check changed the store:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestTaskEntryExistsReportsRegularFileAtID(t *testing.T) {
	s := testStore(t)
	id := newTaskID(t)
	must(t, os.WriteFile(filepath.Join(tasksDir(t, s), id), []byte("x"), 0o600))
	requireEntryExists(t, s, id, true)
}

func TestTaskEntryExistsNeverFollowsSymlinkAtID(t *testing.T) {
	s := testStore(t)
	tasks := tasksDir(t, s)
	outside := t.TempDir()
	cases := map[string]string{
		"dangling": filepath.Join(outside, "missing"),
		"live":     outside,
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			id := newTaskID(t)
			link := filepath.Join(tasks, id)
			must(t, os.Symlink(target, link))
			before := snapshotTree(t, outside)
			requireEntryExists(t, s, id, true)
			if after := snapshotTree(t, outside); !reflect.DeepEqual(before, after) {
				t.Fatalf("presence check touched the symlink target:\nbefore=%v\nafter=%v", before, after)
			}
			if got, err := os.Readlink(link); err != nil || got != target {
				t.Fatalf("symlink changed: %q, %v", got, err)
			}
		})
	}
}

func TestTaskEntryExistsRejectsInvalidID(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"", "short", "../" + newTaskID(t), "ABCDEF0123456789ABCDEF0123456789", newTaskID(t) + "0"} {
		got, err := s.TaskEntryExists(id)
		if !errors.Is(err, task.ErrInvalidTaskID) || got {
			t.Fatalf("TaskEntryExists(%q) = %t, %v; want false, ErrInvalidTaskID", id, got, err)
		}
	}
}

func TestTaskEntryExistsPreservesClosedStoreError(t *testing.T) {
	s := testStore(t)
	must(t, s.Close())
	got, err := s.TaskEntryExists(newTaskID(t))
	if err == nil || got {
		t.Fatalf("TaskEntryExists on a closed store = %t, %v; want error", got, err)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed store was reported as absence: %v", err)
	}
}

func TestTaskEntryExistsPreservesUnsafeTasksDirectoryErrors(t *testing.T) {
	t.Run("symlinked tasks directory", func(t *testing.T) {
		s := testStore(t)
		tasks := filepath.Join(s.Root, "tasks")
		real := filepath.Join(t.TempDir(), "elsewhere")
		must(t, os.Mkdir(real, 0o700))
		must(t, os.Symlink(real, tasks))
		got, err := s.TaskEntryExists(newTaskID(t))
		if err == nil || got {
			t.Fatalf("symlinked tasks directory = %t, %v; want error", got, err)
		}
	})
	t.Run("unsafe tasks directory mode", func(t *testing.T) {
		s := testStore(t)
		tasks := tasksDir(t, s)
		must(t, os.Chmod(tasks, 0o755))
		t.Cleanup(func() { must(t, os.Chmod(tasks, 0o700)) })
		got, err := s.TaskEntryExists(newTaskID(t))
		if !errors.Is(err, task.ErrEvidenceFault) || got {
			t.Fatalf("unsafe tasks mode = %t, %v; want false, ErrEvidenceFault", got, err)
		}
	})
	t.Run("tasks is a regular file", func(t *testing.T) {
		s := testStore(t)
		must(t, os.WriteFile(filepath.Join(s.Root, "tasks"), nil, 0o600))
		got, err := s.TaskEntryExists(newTaskID(t))
		if err == nil || got {
			t.Fatalf("regular-file tasks = %t, %v; want error", got, err)
		}
	})
}
