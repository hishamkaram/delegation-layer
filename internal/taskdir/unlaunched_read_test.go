package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func readOnlyRefusedTask(t *testing.T, store *Store) (*TaskDir, string) {
	t.Helper()
	td := supervisorTask(t, store)
	permit, err := td.PrepareStop(strings.Repeat("d", 32), "user", time.Time{})
	must(t, err)
	request, err := permit.Request()
	must(t, err)
	must(t, permit.Consume())
	must(t, permit.Release())
	data, err := task.MarshalCanonical(request)
	must(t, err)
	return td, task.ComputeSHA256(data)
}

func TestReadUnlaunchedTaskDoesNotCreateMissingOwner(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "refused-before-settlement"}[refused], func(t *testing.T) {
			store := testStore(t)
			td := preparedTask(t, store)
			if refused {
				td, _ = readOnlyRefusedTask(t, store)
			}
			before := snapshotTree(t, store.Root)
			proven, reference, err := td.ReadUnlaunchedTaskReferenceContext(t.Context())
			if err != nil || proven || reference != "" {
				t.Fatalf("missing owner proof = %t, %q, %v", proven, reference, err)
			}
			if after := snapshotTree(t, store.Root); !reflect.DeepEqual(before, after) {
				t.Fatalf("observation created storage: before %v, after %v", before, after)
			}
			requireMaintenanceReleased(t, store)
		})
	}
}

func TestReadUnlaunchedTaskReusesCanonicalSettledRefusal(t *testing.T) {
	store := testStore(t)
	td, expected := readOnlyRefusedTask(t, store)
	// Explicit task settlement owns creating the lifetime lock. The observation
	// below must neither create that inode nor invent another refusal.
	settled, _, err := td.ProveUnlaunchedTaskReferenceContext(t.Context())
	must(t, err)
	if !settled {
		t.Fatal("fixture explicit task settlement was not proven")
	}
	before := snapshotTree(t, store.Root)
	proven, reference, err := td.ReadUnlaunchedTaskReferenceContext(t.Context())
	if err != nil || !proven || reference != expected {
		t.Fatalf("existing settled proof = %t, %q, %v; want %q", proven, reference, err, expected)
	}
	if after := snapshotTree(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatalf("settled observation changed storage: before %v, after %v", before, after)
	}
	requireMaintenanceReleased(t, store)
}

func TestReadUnlaunchedTaskRequiresIdleOwnerAndRun(t *testing.T) {
	for _, busy := range []string{"owner", "run"} {
		t.Run(busy, func(t *testing.T) {
			store := testStore(t)
			td, _ := readOnlyRefusedTask(t, store)
			owner, err := td.AcquireLifetimeOwner(t.Context())
			must(t, err)
			t.Cleanup(func() { must(t, owner.Release()) })
			if busy == "run" {
				must(t, owner.Release())
				other, openErr := store.OpenTask(td.TaskID)
				must(t, openErr)
				t.Cleanup(func() { must(t, other.Close()) })
				must(t, other.runLock.LockEX(t.Context()))
				t.Cleanup(func() { must(t, other.runLock.Unlock()) })
			}
			proven, reference, err := td.ReadUnlaunchedTaskReferenceContext(t.Context())
			if err != nil || proven || reference != "" {
				t.Fatalf("busy %s proof = %t, %q, %v", busy, proven, reference, err)
			}
			if busy == "run" {
				probe, probeErr := td.TryAcquireLifetimeOwnerContext(t.Context())
				must(t, probeErr)
				must(t, probe.Release())
			}
		})
	}
}

func TestReadUnlaunchedTaskRefusesInvalidOwnerInodes(t *testing.T) {
	for _, kind := range []string{"symlink", "unsafe-mode", "directory"} {
		t.Run(kind, func(t *testing.T) {
			store := testStore(t)
			td, _ := readOnlyRefusedTask(t, store)
			dir := filepath.Join(td.Dir, lifetimeDirectory)
			must(t, os.Mkdir(dir, 0o700))
			path := filepath.Join(dir, lifetimeOwnerLockName)
			switch kind {
			case "symlink":
				must(t, os.Symlink(filepath.Join(store.Root, ".maintenance.lock"), path))
			case "unsafe-mode":
				must(t, os.WriteFile(path, nil, 0o600))
				must(t, os.Chmod(path, 0o644))
			case "directory":
				must(t, os.Mkdir(path, 0o700))
			}
			proven, reference, err := td.ReadUnlaunchedTaskReferenceContext(t.Context())
			if err == nil || proven || reference != "" {
				t.Fatalf("invalid %s proof = %t, %q, %v", kind, proven, reference, err)
			}
			requireMaintenanceReleased(t, store)
		})
	}
}

func TestReadUnlaunchedTaskCancellationCreatesNothing(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	before := snapshotTree(t, store.Root)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	proven, reference, err := td.ReadUnlaunchedTaskReferenceContext(ctx)
	if !errors.Is(err, context.Canceled) || proven || reference != "" {
		t.Fatalf("canceled proof = %t, %q, %v", proven, reference, err)
	}
	if after := snapshotTree(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatalf("canceled observation changed storage: before %v, after %v", before, after)
	}
	requireMaintenanceReleased(t, store)
}
