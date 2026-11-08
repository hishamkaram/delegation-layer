package taskdir

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type inventoryFixtureBinding struct {
	rootID string
	device uint64
	inode  uint64
}

type inventoryFixtureObservation struct {
	RootID     string   `json:"root_id"`
	RootDevice uint64   `json:"root_device"`
	RootInode  uint64   `json:"root_inode"`
	TaskIDs    []string `json:"task_ids"`
}

// TestTaskInventoryFixture is a finite read-only entrypoint for acceptance
// cleanup. The caller fences creators; this observation grants no disposal
// authority and never opens the mutable Store's maintenance lease.
func TestTaskInventoryFixture(t *testing.T) {
	if os.Getenv("DELEGATE_TASK_INVENTORY_MODE") != "observe" {
		t.Skip("entrypoint for native private task inventory observation")
	}
	device, deviceErr := strconv.ParseUint(os.Getenv("DELEGATE_TASK_INVENTORY_ROOT_DEVICE"), 10, 64)
	inode, inodeErr := strconv.ParseUint(os.Getenv("DELEGATE_TASK_INVENTORY_ROOT_INODE"), 10, 64)
	if err := errors.Join(deviceErr, inodeErr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	observation, err := readInventoryFixture(ctx, os.Getenv("DELEGATE_TASK_INVENTORY_ROOT"), inventoryFixtureBinding{
		rootID: os.Getenv("DELEGATE_TASK_INVENTORY_ROOT_ID"), device: device, inode: inode,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 1024*1024-1024 {
		t.Fatal("task inventory exceeds its output bound")
	}
	if _, err = fmt.Fprintln(os.Stdout, "TASK_INVENTORY_OBSERVATION\t"+string(data)); err != nil {
		t.Fatal(err)
	}
}

func readInventoryFixture(ctx context.Context, root string, expected inventoryFixtureBinding) (observation inventoryFixtureObservation, resultErr error) {
	if err := task.ValidateTaskID(expected.rootID); err != nil {
		return observation, err
	}
	reader, err := OpenReadOnlyStoreContext(ctx, root, predicate.Default())
	if err != nil {
		return observation, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, reader.Close())
		if resultErr != nil {
			observation = inventoryFixtureObservation{}
		}
	}()
	before, err := reader.PinRootContext(ctx)
	if err != nil {
		return observation, err
	}
	if before.RootID() != expected.rootID || before.Device() != expected.device || before.Inode() != expected.inode {
		return observation, task.ErrIdentityMismatch
	}
	ids, err := reader.store.scanPrivateDirectoryIDs(ctx, filepath.Join(reader.Root(), "tasks"), "task")
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
	return inventoryFixtureObservation{
		RootID: before.RootID(), RootDevice: before.Device(), RootInode: before.Inode(), TaskIDs: ids,
	}, nil
}

func inventoryFixtureStore(t *testing.T) (*Store, inventoryFixtureBinding) {
	t.Helper()
	store := testStore(t)
	pin, err := store.PinRootContext(t.Context())
	must(t, err)
	return store, inventoryFixtureBinding{rootID: pin.RootID(), device: pin.Device(), inode: pin.Inode()}
}

func TestInventoryFixtureIncludesPreparedAndEmptyEntriesWithoutMutation(t *testing.T) {
	store, binding := inventoryFixtureStore(t)
	prepared := preparedTask(t, store)
	const first = "11111111111111111111111111111111"
	const second = "22222222222222222222222222222222"
	parent := filepath.Join(store.Root, "tasks")
	must(t, os.MkdirAll(filepath.Join(parent, second), 0o700))
	must(t, os.MkdirAll(filepath.Join(parent, first), 0o700))
	must(t, os.WriteFile(filepath.Join(parent, first, "task.json"), []byte("partially prepared"), 0o600))
	// In particular, observation must not recreate a removed maintenance lease.
	must(t, os.Remove(filepath.Join(store.Root, ".maintenance.lock")))
	before := rootPinEntryNames(t, store.Root)
	actual, err := readInventoryFixture(t.Context(), store.Root, binding)
	must(t, err)
	expected := []string{first, second, prepared.TaskID}
	slices.Sort(expected)
	if !slices.Equal(actual.TaskIDs, expected) || actual.RootID != binding.rootID ||
		actual.RootDevice != binding.device || actual.RootInode != binding.inode {
		t.Fatalf("inventory lost partial entries or original identity: %#v", actual)
	}
	if after := rootPinEntryNames(t, store.Root); !slices.Equal(before, after) {
		t.Fatalf("passive inventory mutated root entries: %v -> %v", before, after)
	}
}

func TestInventoryFixtureEmptyInventorySerializesAsArrayWithoutMutation(t *testing.T) {
	for _, parentExists := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-tasks", true: "empty-tasks"}[parentExists], func(t *testing.T) {
			store, binding := inventoryFixtureStore(t)
			parent := filepath.Join(store.Root, "tasks")
			if parentExists {
				must(t, os.MkdirAll(parent, 0o700))
			} else if err := os.Remove(parent); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			before := rootPinEntryNames(t, store.Root)
			observation, err := readInventoryFixture(t.Context(), store.Root, binding)
			must(t, err)
			data, err := json.Marshal(observation)
			must(t, err)
			var wire map[string]json.RawMessage
			must(t, json.Unmarshal(data, &wire))
			if string(wire["task_ids"]) != "[]" {
				t.Fatalf("empty inventory must be an array: %s", data)
			}
			if after := rootPinEntryNames(t, store.Root); !slices.Equal(before, after) {
				t.Fatalf("empty inventory mutated root entries: %v -> %v", before, after)
			}
		})
	}
}

func TestInventoryFixtureRejectsWrongAndReplacedRootBinding(t *testing.T) {
	store, binding := inventoryFixtureStore(t)
	wrong := binding
	wrong.rootID = "11111111111111111111111111111111"
	if _, err := readInventoryFixture(t.Context(), store.Root, wrong); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("wrong root identity accepted: %v", err)
	}
	metadata, err := os.ReadFile(filepath.Join(store.Root, "root.json"))
	must(t, err)
	must(t, os.Rename(store.Root, store.Root+"-original"))
	must(t, os.Mkdir(store.Root, 0o700))
	must(t, os.WriteFile(filepath.Join(store.Root, "root.json"), metadata, 0o600))
	if _, err = readInventoryFixture(t.Context(), store.Root, binding); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("replaced root with cloned metadata accepted: %v", err)
	}
}

func TestInventoryFixtureRejectsUnsafeEntriesAndCancellation(t *testing.T) {
	for _, kind := range []string{"invalid-name", "symlink", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			store, binding := inventoryFixtureStore(t)
			parent := filepath.Join(store.Root, "tasks")
			must(t, os.MkdirAll(parent, 0o700))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch kind {
			case "invalid-name":
				must(t, os.Mkdir(filepath.Join(parent, "invalid"), 0o700))
			case "symlink":
				must(t, os.Symlink(t.TempDir(), filepath.Join(parent, "11111111111111111111111111111111")))
			case "canceled":
				cancel()
			}
			observation, err := readInventoryFixture(ctx, store.Root, binding)
			if err == nil || observation.RootID != "" || observation.TaskIDs != nil {
				t.Fatalf("unsafe %s yielded inventory: %#v, %v", kind, observation, err)
			}
		})
	}
}
