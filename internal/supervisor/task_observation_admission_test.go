package supervisor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type observationTreeEntry struct {
	path   string
	mode   fs.FileMode
	sha256 string
}

func observationTree(t *testing.T, root string) []observationTreeEntry {
	t.Helper()
	var result []observationTreeEntry
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		row := observationTreeEntry{path: path, mode: info.Mode()}
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			row.sha256 = task.ComputeSHA256(data)
		}
		result = append(result, row)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// These records exercise the canonical read boundary only. No daemon endpoint,
// executing-image eligibility or native owner permission is claimed by them.
func observationReceipt(store *taskdir.Store) (runnerRepairReceipt, string) {
	binding := generationBinding(store.Root, "observation-data")
	digest := task.ComputeSHA256([]byte("runner-data"))
	return runnerRepairReceipt{SchemaVersion: task.SchemaVersion, Revision: runnerRepairReceiptRevision, Supervisor: binding, RunnerExecutable: filepath.Join(store.Root, ".supervisor", stateRunnerName+"-"+digest), RunnerSHA256: digest}, "receipt.json"
}

func TestRunnerRepairReceiptReadDoesNotInitializeNamespace(t *testing.T) {
	fixture := newHelperAdmissionFixture(t)
	receipt, name := observationReceipt(fixture.store)
	before := observationTree(t, fixture.store.Root)
	current, err := runnerRepairReceiptCurrent(t.Context(), fixture.store, name, receipt)
	if err != nil || current {
		t.Fatalf("absent receipt read: current=%t error=%v", current, err)
	}
	if !slices.Equal(before, observationTree(t, fixture.store.Root)) {
		t.Fatal("missing receipt observation initialized storage")
	}
}

func TestColdTaskObservationWaitsForOuterRootAdmission(t *testing.T) {
	fixture := newHelperAdmissionFixture(t)
	exclusive, err := fixture.store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(exclusive)
	if err != nil {
		t.Fatal(err)
	}
	before := observationTree(t, fixture.store.Root)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	receipt, _ := observationReceipt(fixture.store)
	observed, err := ObserveTaskSupervisorWithAdmissionContext(ctx, fixture.store, receipt.Supervisor, &task.MetaRecord{Environment: []string{}}, TaskSupervisorObservationOptions{Control: pueue.Options{Environment: []string{}}})
	if observed.Client != nil {
		t.Cleanup(func() {
			if closeErr := observed.Client.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cold observation bypassed outer root admission: %v", err)
	}
	if !slices.Equal(before, observationTree(t, fixture.store.Root)) {
		t.Fatal("blocked observation prepared supervisor state")
	}
}

func TestRunnerRepairReceiptCurrentReadIsObservational(t *testing.T) {
	fixture := newHelperAdmissionFixture(t)
	receipt, name := observationReceipt(fixture.store)
	if err := os.Mkdir(filepath.Dir(receipt.RunnerExecutable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receipt.RunnerExecutable, []byte("runner-data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := publishRunnerRepairReceipt(t.Context(), fixture.store, name, receipt); err != nil {
		t.Fatal(err)
	}
	before := observationTree(t, fixture.store.Root)
	current, err := runnerRepairReceiptCurrent(t.Context(), fixture.store, name, receipt)
	if err != nil || !current {
		t.Fatalf("canonical receipt was not current: %v", err)
	}
	if !slices.Equal(before, observationTree(t, fixture.store.Root)) {
		t.Fatal("current receipt observation changed storage")
	}
}

func TestColdTaskObservationRetainsBorrowedRootAdmission(t *testing.T) {
	fixture := newHelperAdmissionFixture(t)
	lease, err := fixture.store.AcquireRootAdmissionContext(t.Context())
	fixture.own(lease)
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ := observationReceipt(fixture.store)
	// The absent saved files force canonical cold preparation to refuse. No CLI
	// is available in the explicitly empty lookup scope, so no actor can start.
	observed, observeErr := ObserveTaskSupervisorWithAdmissionContext(t.Context(), fixture.store, receipt.Supervisor, &task.MetaRecord{Environment: []string{}}, TaskSupervisorObservationOptions{Control: pueue.Options{Environment: []string{}}, Lookup: ExecutableLookup{Environment: []string{}}, RootAdmission: lease})
	if observed.Client != nil {
		t.Cleanup(func() {
			if closeErr := observed.Client.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if observeErr == nil {
		t.Fatal("missing saved generation unexpectedly prepared")
	}
	if err = lease.CheckRootContext(t.Context(), fixture.store); err != nil {
		t.Fatalf("observation released caller's native hold: %v", err)
	}
}
