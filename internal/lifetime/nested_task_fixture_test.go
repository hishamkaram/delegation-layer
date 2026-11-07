package lifetime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// The external test package can call the task-store owner without creating a
// lifetime -> taskdir production dependency or reimplementing its lock policy.
func TestNestedTaskAbsenceFixture(t *testing.T) {
	mode := os.Getenv("DELEGATE_NESTED_FIXTURE_MODE")
	if mode == "task-entry" {
		runNestedTaskEntryFixture(t)
		return
	}
	if mode != "task-absence" && mode != "task-identity" {
		t.Skip("entrypoint for canonical never-launched acceptance observation")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, os.Getenv("DELEGATE_NESTED_FIXTURE_ROOT"), app.NativePredicates())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	task, err := store.OpenTask(os.Getenv("DELEGATE_NESTED_FIXTURE_TASK"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := task.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	observation := struct {
		RootID       string `json:"root_id"`
		TaskID       string `json:"task_id"`
		NeverStarted bool   `json:"never_started"`
		RefusalSHA   string `json:"refusal_sha256"`
		Prepared     bool   `json:"prepared"`
		SpecSHA      string `json:"spec_sha256"`
		MetaSHA      string `json:"meta_sha256"`
	}{RootID: store.RootID, TaskID: task.TaskID}
	if mode == "task-identity" {
		_, _, observation.SpecSHA, observation.MetaSHA, err = task.PreparedIdentity()
		observation.Prepared = err == nil
	} else {
		observation.NeverStarted, observation.RefusalSHA, err = tasklifecycle.ProveScheduledTaskUnlaunchedContext(ctx, task)
	}
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Fprintln(os.Stdout, "NESTED_TASK_OBSERVATION\t"+string(data)); err != nil {
		t.Fatal(err)
	}
}

type nestedTaskEntryBinding struct {
	rootID string
	device uint64
	inode  uint64
}

type nestedTaskEntryObservation struct {
	RootID      string `json:"root_id"`
	TaskID      string `json:"task_id"`
	EntryExists bool   `json:"entry_exists"`
	RootDevice  uint64 `json:"root_device"`
	RootInode   uint64 `json:"root_inode"`
}

func runNestedTaskEntryFixture(t *testing.T) {
	t.Helper()
	device, err := strconv.ParseUint(os.Getenv("DELEGATE_NESTED_FIXTURE_ROOT_DEVICE"), 10, 64)
	if err != nil {
		t.Fatal("invalid original root device")
	}
	inode, err := strconv.ParseUint(os.Getenv("DELEGATE_NESTED_FIXTURE_ROOT_INODE"), 10, 64)
	if err != nil {
		t.Fatal("invalid original root inode")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	expected := nestedTaskEntryBinding{rootID: os.Getenv("DELEGATE_NESTED_FIXTURE_ROOT_ID"), device: device, inode: inode}
	observation, err := readNestedTaskEntry(ctx, os.Getenv("DELEGATE_NESTED_FIXTURE_ROOT"), os.Getenv("DELEGATE_NESTED_FIXTURE_TASK"), expected)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Fprintln(os.Stdout, "NESTED_TASK_OBSERVATION\t"+string(data)); err != nil {
		t.Fatal(err)
	}
}

func readNestedTaskEntry(ctx context.Context, root, id string, expected nestedTaskEntryBinding) (observation nestedTaskEntryObservation, resultErr error) {
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, app.NativePredicates())
	if err != nil {
		return observation, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, reader.Close())
		if resultErr != nil {
			observation = nestedTaskEntryObservation{}
		}
	}()
	before, err := reader.PinRootContext(ctx)
	if err != nil {
		return observation, err
	}
	if before.RootID() != expected.rootID || before.Device() != expected.device || before.Inode() != expected.inode {
		return observation, task.ErrIdentityMismatch
	}
	evidence, err := reader.ReadTaskEvidence(ctx, id)
	if err != nil {
		return observation, err
	}
	after, err := reader.PinRootContext(ctx)
	if err != nil {
		return observation, err
	}
	if !before.Equal(after) {
		return observation, task.ErrIdentityMismatch
	}
	return nestedTaskEntryObservation{
		RootID: before.RootID(), TaskID: id, EntryExists: evidence.Exists,
		RootDevice: before.Device(), RootInode: before.Inode(),
	}, nil
}

func nestedEntryTestStore(t *testing.T) (*taskdir.Store, nestedTaskEntryBinding) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return store, nestedTaskEntryBinding{rootID: pin.RootID(), device: pin.Device(), inode: pin.Inode()}
}

func TestNestedTaskEntryObservesAbsentEntryWithoutOpeningTask(t *testing.T) {
	store, binding := nestedEntryTestStore(t)
	id := strings.Repeat("a", 32)
	observed, err := readNestedTaskEntry(t.Context(), store.Root, id, binding)
	if err != nil {
		t.Fatal(err)
	}
	want := nestedTaskEntryObservation{RootID: store.RootID, TaskID: id, RootDevice: binding.device, RootInode: binding.inode}
	if observed != want {
		t.Fatalf("absent entry observation = %+v, want %+v", observed, want)
	}
	if _, err = os.Lstat(filepath.Join(store.Root, "tasks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only observation created task state: %v", err)
	}
}

func TestNestedTaskEntryRetainsPresentPartialAndUnsafeEntries(t *testing.T) {
	for _, kind := range []string{"partial", "file", "symlink", "dangling", "unsafe-parent"} {
		t.Run(kind, func(t *testing.T) {
			store, binding := nestedEntryTestStore(t)
			id := strings.Repeat("a", 32)
			tasks := filepath.Join(store.Root, "tasks")
			if kind == "unsafe-parent" {
				if err := os.Symlink(t.TempDir(), tasks); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(tasks, 0o700); err != nil {
					t.Fatal(err)
				}
				createNestedPartialEntry(t, filepath.Join(tasks, id), kind)
			}
			if observed, err := readNestedTaskEntry(t.Context(), store.Root, id, binding); err == nil {
				t.Fatalf("%s entry produced a successful observation: %+v", kind, observed)
			}
		})
	}
}

func createNestedPartialEntry(t *testing.T, path, kind string) {
	t.Helper()
	var err error
	switch kind {
	case "partial":
		err = os.Mkdir(path, 0o700)
	case "file":
		err = os.WriteFile(path, []byte("partial"), 0o600)
	case "symlink":
		err = os.Symlink(t.TempDir(), path)
	case "dangling":
		err = os.Symlink(filepath.Join(t.TempDir(), "missing"), path)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestNestedTaskEntryRejectsChangedBindingInvalidIDAndCanceledRead(t *testing.T) {
	store, binding := nestedEntryTestStore(t)
	id := strings.Repeat("a", 32)
	changed := binding
	changed.inode++
	if _, err := readNestedTaskEntry(t.Context(), store.Root, id, changed); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("changed root binding = %v", err)
	}
	if _, err := readNestedTaskEntry(t.Context(), store.Root, "invalid", binding); !errors.Is(err, task.ErrInvalidTaskID) {
		t.Fatalf("invalid task ID = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readNestedTaskEntry(ctx, store.Root, id, binding); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = %v", err)
	}
}

func TestNestedTaskEntryRejectsReplacementWithIdenticalRootMetadata(t *testing.T) {
	store, binding := nestedEntryTestStore(t)
	metadata, err := os.ReadFile(filepath.Join(store.Root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(store.Root, store.Root+"-retained"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(store.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, "root.json"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = readNestedTaskEntry(t.Context(), store.Root, strings.Repeat("a", 32), binding); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("replacement with copied root metadata was accepted: %v", err)
	}
}
