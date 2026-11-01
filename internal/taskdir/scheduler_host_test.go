package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSchedulerHostReadDoesNotInitializeJournal(t *testing.T) {
	store := testStore(t)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	if _, err = reader.SchedulerHostRecordNamesContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing journal observation: %v", err)
	}
	if _, err = reader.ReadSchedulerHostRecordContext(t.Context(), "host.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing record observation: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(store.Root, schedulerHostDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive observation initialized a journal: %v", err)
	}
}

func TestSchedulerHostExplicitRecoveryVerifiesExistingFenceOnly(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenSchedulerHostContext(t.Context(), true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	record := controlTestRecord{Request: "stopped"}
	_, err = control.Put("stop.json", record)
	must(t, err)
	data, err := task.MarshalCanonical(record)
	must(t, err)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	fault := &controlVerifyFault{}
	reader.store.faultInjector = fault
	must(t, reader.VerifySchedulerHostRecordContext(t.Context(), "stop.json", data))
	directory := filepath.Join(store.Root, schedulerHostDirectory)
	assertControlVerifyEvents(t, fault, []string{"directory:" + directory, "parent:" + store.Root})
	if err = reader.VerifySchedulerHostRecordContext(t.Context(), "missing.json", data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing fence verification: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(directory, "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification created a missing fence: %v", err)
	}
	if err = reader.VerifySchedulerHostRecordContext(t.Context(), "stop.json", []byte("{}\n")); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("different fence accepted: %v", err)
	}
}

func TestSchedulerHostRecoveryPreservesBarrierFailure(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenSchedulerHostContext(t.Context(), true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	record := controlTestRecord{Request: "stopped"}
	_, err = control.Put("stop.json", record)
	must(t, err)
	data, err := task.MarshalCanonical(record)
	must(t, err)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	failure := errors.New("stop recovery directory barrier failed")
	reader.store.faultInjector = &controlVerifyFault{failAt: "directory", failure: failure}
	if err = reader.VerifySchedulerHostRecordContext(t.Context(), "stop.json", data); !errors.Is(err, failure) || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("recovery lost the durability failure: %v", err)
	}
}

func TestSchedulerHostReadsUsePinnedRootAndRejectTraversal(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenSchedulerHostContext(t.Context(), true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	_, err = control.Put("host.json", controlTestRecord{Request: "original"})
	must(t, err)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	must(t, os.Rename(store.Root, store.Root+"-retained"))
	must(t, os.MkdirAll(filepath.Join(store.Root, schedulerHostDirectory), 0o700))
	must(t, os.WriteFile(filepath.Join(store.Root, schedulerHostDirectory, "replacement.json"), []byte("{}"), 0o600))
	names, err := reader.SchedulerHostRecordNamesContext(t.Context())
	must(t, err)
	if !slices.Contains(names, "host.json") || slices.Contains(names, "replacement.json") {
		t.Fatalf("read escaped the pinned root: %v", names)
	}
	data, err := reader.ReadSchedulerHostRecordContext(t.Context(), "host.json")
	must(t, err)
	if string(data) != "{\"request\":\"original\"}\n" {
		t.Fatalf("record changed after root replacement: %q", data)
	}
	if _, err = reader.ReadSchedulerHostRecordContext(t.Context(), "../root.json"); err == nil {
		t.Fatal("record traversal accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = reader.SchedulerHostRecordNamesContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("read lost cancellation: %v", err)
	}
}
