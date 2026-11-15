package scheduler

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/internal/sqlitestore"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestOpenReadOnlyFacadeFailureReturnsNilStore(t *testing.T) {
	for _, test := range []struct {
		name           string
		cause          error
		classification Compatibility
	}{
		{"empty", fs.ErrNotExist, CompatibilityAbsent},
		{"foreign", task.ErrUnsupportedSchema, CompatibilityIncompatible},
		{"newer", task.ErrUnsupportedSchema, CompatibilityNewerSchema},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := readOnlyFacadeState(t, test.name)
			preserveReadOnlyFacadeState(t, state.Root)
			reader := readOnlyFacadeReader(t, state.Root)
			opened, err := OpenReadOnly(testContext(t), reader)
			if !errors.Is(err, test.cause) {
				t.Fatalf("read-only error = %v, want %v", err, test.cause)
			}
			if got := CompatibilityOf(err); got != test.classification {
				t.Fatalf("read-only compatibility = %s, want %s", got, test.classification)
			}
			if opened != nil {
				t.Fatalf("failed read-only open returned a non-nil Store interface (%T)", opened)
			}
			if got := ObserveCompatibility(testContext(t), state.Root, predicate.Default()); got != test.classification {
				t.Fatalf("passive compatibility = %s, want %s", got, test.classification)
			}
		})
	}
}

func TestOpenReadOnlyFacadeSuccessPreservesState(t *testing.T) {
	state := readOnlyFacadeState(t, "current")
	preserveReadOnlyFacadeState(t, state.Root)
	reader := readOnlyFacadeReader(t, state.Root)
	opened, err := OpenReadOnly(testContext(t), reader)
	if err != nil || opened == nil {
		t.Fatalf("current read-only store = %T, %v", opened, err)
	}
	t.Cleanup(func() {
		if closeErr := opened.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	page, err := opened.ListSchedules(testContext(t), contract.SchedulePage{Limit: 1})
	if err != nil || len(page.Schedules) != 0 {
		t.Fatalf("current read-only schedules = %+v, %v", page, err)
	}
	if got := ObserveCompatibility(testContext(t), state.Root, predicate.Default()); got != CompatibilityCompatible {
		t.Fatalf("current passive compatibility = %s, want compatible", got)
	}
}

func readOnlyFacadeState(t *testing.T, kind string) *taskdir.Store {
	t.Helper()
	state := stateStore(t)
	switch kind {
	case "empty", "foreign":
		if err := os.Mkdir(sqlitestore.Directory(state.Root), 0o700); err != nil {
			t.Fatal(err)
		}
		var data []byte
		if kind == "foreign" {
			data = corruptDatabase
		}
		if err := writePrivateFile(sqlitestore.DatabasePath(state.Root), data); err != nil {
			t.Fatal(err)
		}
	case "newer", "current":
		migrated(t, state)
		if kind == "newer" {
			if err := rawStatements(testContext(t), state.Root, fmt.Sprintf("PRAGMA user_version = %d", unsupportedVersion)); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatalf("unknown read-only fixture %q", kind)
	}
	return state
}

func readOnlyFacadeReader(t *testing.T, root string) *taskdir.ReadOnlyStore {
	t.Helper()
	reader, err := taskdir.OpenReadOnlyStoreContext(testContext(t), root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return reader
}

func preserveReadOnlyFacadeState(t *testing.T, root string) {
	t.Helper()
	before := snapshot(t, root)
	// The task-state snapshot excludes this namespace, so capture it separately
	// to cover database bytes, modes and the complete set of SQLite sidecars.
	directory := sqlitestore.Directory(root)
	beforeScheduler := snapshot(t, directory)
	t.Cleanup(func() {
		if !maps.Equal(before, snapshot(t, root)) {
			t.Error("passive scheduler observation changed task state")
		}
		if !maps.Equal(beforeScheduler, snapshot(t, directory)) {
			t.Error("passive scheduler observation changed database bytes or sidecars")
		}
	})
}
