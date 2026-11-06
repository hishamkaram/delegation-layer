package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInspectionOperationIDsAbsentParentCreatesNothing(t *testing.T) {
	store := testStore(t)
	before := snapshotTree(t, store.Root)
	ids, err := store.InspectionOperationIDs(t.Context())
	if err != nil || ids == nil || len(ids) != 0 {
		t.Fatalf("absent inspections = %v, %v; want nonnil empty list", ids, err)
	}
	if after := snapshotTree(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatalf("listing changed root: before %v, after %v", before, after)
	}
	requireMaintenanceReleased(t, store)
}

func TestInspectionOperationIDsIncludesOrphansWithoutReadingRecords(t *testing.T) {
	store := testStore(t)
	var expected []string
	for range 2 {
		id := newTaskID(t)
		operation, err := store.OpenInspectionContext(t.Context(), id, true)
		must(t, err)
		must(t, operation.Close())
		must(t, os.WriteFile(filepath.Join(store.Root, "inspections", id, "request.json"), []byte("unreadable request"), 0o600))
		expected = append(expected, id)
	}
	orphan := newTaskID(t)
	must(t, os.Mkdir(filepath.Join(store.Root, "inspections", orphan), 0o700))
	expected = append(expected, orphan)
	slices.Sort(expected)
	before := snapshotTree(t, store.Root)
	ids, err := store.InspectionOperationIDs(t.Context())
	if err != nil || !slices.Equal(ids, expected) {
		t.Fatalf("inspection candidates = %v, %v; want %v", ids, err, expected)
	}
	if after := snapshotTree(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatalf("candidate listing modified records: before %v, after %v", before, after)
	}
	requireMaintenanceReleased(t, store)
}

func TestInspectionOperationIDsRefusesHiddenWorkEntries(t *testing.T) {
	for _, kind := range []string{"symlink", "malformed", "unsafe-mode", "regular-file"} {
		t.Run(kind, func(t *testing.T) {
			store := testStore(t)
			id := newTaskID(t)
			operation, err := store.OpenInspectionContext(t.Context(), id, true)
			must(t, err)
			must(t, operation.Close())
			parent := filepath.Join(store.Root, "inspections")
			candidate := filepath.Join(parent, newTaskID(t))
			switch kind {
			case "symlink":
				must(t, os.Symlink(filepath.Join(parent, id), candidate))
			case "malformed":
				must(t, os.Mkdir(filepath.Join(parent, "not-an-operation"), 0o700))
			case "unsafe-mode":
				must(t, os.Mkdir(candidate, 0o700))
				must(t, os.Chmod(candidate, 0o755))
			case "regular-file":
				must(t, os.WriteFile(candidate, nil, 0o600))
			}
			ids, err := store.InspectionOperationIDs(t.Context())
			if ids != nil || !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("unsafe candidate = %v, %v; want evidence refusal and no partial list", ids, err)
			}
			requireMaintenanceReleased(t, store)
		})
	}
}

func TestInspectionOperationIDsHonorsCancellationAndClosedRoot(t *testing.T) {
	store := testStore(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if ids, err := store.InspectionOperationIDs(canceled); ids != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled listing = %v, %v", ids, err)
	}
	must(t, store.Close())
	if ids, err := store.InspectionOperationIDs(t.Context()); ids != nil || err == nil {
		t.Fatalf("closed root listing = %v, %v", ids, err)
	}
}
