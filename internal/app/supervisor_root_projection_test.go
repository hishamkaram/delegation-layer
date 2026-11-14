package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSupervisorRootAbsenceRequiresMissingOperationAndNamespace(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	present := t.TempDir()
	dangling := filepath.Join(t.TempDir(), "dangling")
	if err := os.Symlink(absent, dangling); err != nil {
		t.Fatal(err)
	}
	ancestor := filepath.Join(privateSupervisorRootFixtureDirectory(t), "file")
	if err := os.WriteFile(ancestor, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		root string
		err  error
		want bool
	}{
		{"absent", absent, fmt.Errorf("open state: %w", os.ErrNotExist), true},
		{"successful operation", absent, nil, false},
		{"nonabsence fault", absent, os.ErrPermission, false},
		{"joined cleanup fault", absent, errors.Join(os.ErrNotExist, errors.New("cleanup failed")), false},
		{"joined cancellation", absent, errors.Join(os.ErrNotExist, context.Canceled), false},
		{"joined deadline", absent, errors.Join(os.ErrNotExist, context.DeadlineExceeded), false},
		{"existing root", present, os.ErrNotExist, false},
		{"dangling root entry", dangling, os.ErrNotExist, false},
		{"non-directory root ancestor", filepath.Join(ancestor, "state"), os.ErrNotExist, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisorRootAbsent(tc.root, tc.err); got != tc.want {
				t.Fatalf("root absence=%t, want %t for %v", got, tc.want, tc.err)
			}
		})
	}
}

func TestSupervisorCommandsRejectDanglingRootEntry(t *testing.T) {
	for _, action := range []string{"status", "migrate"} {
		t.Run(action, func(t *testing.T) { requireDanglingSupervisorRootRefused(t, action) })
	}
}

func requireDanglingSupervisorRootRefused(t *testing.T, action string) {
	t.Helper()
	parent := privateSupervisorRootFixtureDirectory(t)
	root, target := filepath.Join(parent, "state"), filepath.Join(parent, "missing")
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	beforeTarget, err := os.Readlink(root)
	if err != nil {
		t.Fatal(err)
	}
	response, code := runSupervisorRootProjection(t, root, action)
	if code != 1 {
		t.Fatalf("dangling root entry exit=%d, want 1: %+v", code, response)
	}
	requireUnavailableSupervisorProjection(t, response, action)
	after, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	afterTarget, err := os.Readlink(root)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() || beforeTarget != afterTarget || !os.SameFile(before, after) {
		t.Fatal("supervisor projection changed the dangling root entry")
	}
	if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("supervisor projection initialized the missing symlink target: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state" {
		t.Fatalf("supervisor projection changed the root parent entries: %v", entries)
	}
}

func TestSupervisorCommandsRejectNonDirectoryRootAncestor(t *testing.T) {
	for _, action := range []string{"status", "migrate"} {
		t.Run(action, func(t *testing.T) {
			parent := privateSupervisorRootFixtureDirectory(t)
			ancestor := filepath.Join(parent, "file")
			if err := os.WriteFile(ancestor, []byte("not a directory\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := snapshotSupervisorProjectionRoot(t, parent)
			root := filepath.Join(ancestor, "state")
			if _, err := os.Lstat(root); !errors.Is(err, syscall.ENOTDIR) {
				t.Fatalf("non-directory root fixture returned %v, want ENOTDIR", err)
			}
			response, code := runSupervisorRootProjection(t, root, action)
			if code != 1 {
				t.Fatalf("non-directory root ancestor exit=%d, want 1: %+v", code, response)
			}
			requireUnavailableSupervisorProjection(t, response, action)
			requireSupervisorProjectionRootUnchanged(t, parent, before)
		})
	}
}

func privateSupervisorRootFixtureDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestSupervisorMigrateRejectsDamagedStateRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	before := snapshotSupervisorProjectionRoot(t, root)
	response, code := runSupervisorRootProjection(t, root, "migrate")
	if code != 1 {
		t.Fatalf("damaged state root exit=%d, want 1: %+v", code, response)
	}
	requireUnavailableSupervisorProjection(t, response, "migrate")
	requireSupervisorProjectionRootUnchanged(t, root, before)
}

func TestSupervisorMigrateKeepsUninitializedRootPassive(t *testing.T) {
	for _, state := range []string{"absent", "store without private config"} {
		t.Run(state, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if state == "store without private config" {
				store, err := taskdir.InitStore(root)
				if err != nil {
					t.Fatal(err)
				}
				if err = store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotSupervisorProjectionRoot(t, root)
			response, code := runSupervisorRootProjection(t, root, "migrate")
			control := response.SupervisorControl
			if code != 0 || response.Error != "" || control == nil {
				t.Fatalf("uninitialized migration was not a read-only no-op: exit=%d %+v", code, response)
			}
			if control.Action != "migrate" || control.State != "not_initialized" || control.LegacyReachability != "not_present" || control.ActiveWork != "none" || control.NextAction != "none" {
				t.Fatalf("uninitialized migration projection: %+v", control)
			}
			requireSupervisorProjectionRootUnchanged(t, root, before)
		})
	}
}

func runSupervisorRootProjection(t *testing.T, root, action string) (Response, int) {
	t.Helper()
	var output, diagnostic bytes.Buffer
	code := Run([]string{"supervisor", action, "--root", root, "--json"}, &output, &diagnostic, Dependencies{})
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatalf("decode supervisor response: exit=%d diagnostic=%q: %v", code, diagnostic.String(), err)
	}
	return response, code
}

func requireUnavailableSupervisorProjection(t *testing.T, response Response, action string) {
	t.Helper()
	control := response.SupervisorControl
	if control == nil {
		t.Fatalf("unavailable root omitted supervisor projection: %+v", response)
	}
	if response.Error == "" || control.Action != action || control.State != "unavailable" || control.LegacyReachability != "unknown" || control.ActiveWork != "unknown" || control.NextAction != "stop_and_report" {
		t.Fatalf("damaged root did not retain unavailable/stop_and_report: %+v", response)
	}
}

type supervisorProjectionEntry struct {
	mode fs.FileMode
	data string
}

func snapshotSupervisorProjectionRoot(t *testing.T, root string) map[string]supervisorProjectionEntry {
	t.Helper()
	entries := make(map[string]supervisorProjectionEntry)
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return entries
	} else if err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := supervisorProjectionEntry{mode: info.Mode()}
		if !entry.IsDir() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			value.data = string(data)
		}
		entries[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func requireSupervisorProjectionRootUnchanged(t *testing.T, root string, before map[string]supervisorProjectionEntry) {
	t.Helper()
	if after := snapshotSupervisorProjectionRoot(t, root); !reflect.DeepEqual(before, after) {
		t.Fatal("supervisor projection changed persisted root entries, permissions or bytes")
	}
}
