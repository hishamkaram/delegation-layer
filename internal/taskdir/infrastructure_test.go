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

func infrastructureReader(t *testing.T, store *Store) *ReadOnlyStore {
	t.Helper()
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	return reader
}

func TestInfrastructurePassiveMissingJournal(t *testing.T) {
	store := testStore(t)
	reader := infrastructureReader(t, store)
	if _, err := reader.InfrastructureRecordNamesContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing journal: %v", err)
	}
	if _, err := reader.ReadInfrastructureRecordContext(t.Context(), "intent.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing record: %v", err)
	}
	if _, err := store.OpenInfrastructureContext(t.Context(), false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open missing journal: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(store.Root, infrastructureDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive access initialized journal: %v", err)
	}
	// An existing journal without an operation lock also remains untouched.
	must(t, os.Mkdir(filepath.Join(store.Root, infrastructureDirectory), 0o700))
	names, err := reader.InfrastructureRecordNamesContext(t.Context())
	must(t, err)
	if len(names) != 0 {
		t.Fatalf("passive enumeration created records: %v", names)
	}
	if _, err := os.Lstat(filepath.Join(store.Root, infrastructureDirectory, ".operation.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive access created lock: %v", err)
	}
}

func TestInfrastructureCreateOnceCanonicalRecords(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenInfrastructureContext(t.Context(), true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	record := controlTestRecord{Request: "epoch-one"}
	created, err := control.PutContext(t.Context(), "intent.json", record)
	must(t, err)
	if !created {
		t.Fatal("first record not created")
	}
	created, err = control.PutContext(t.Context(), "intent.json", record)
	must(t, err)
	if created {
		t.Fatal("equivalent record recreated")
	}
	created, err = control.PutContext(t.Context(), "intent.json", controlTestRecord{Request: "epoch-two"})
	if created || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting record: created=%v error=%v", created, err)
	}
	reader := infrastructureReader(t, store)
	data, err := reader.ReadInfrastructureRecordContext(t.Context(), "intent.json")
	must(t, err)
	expected, err := task.MarshalCanonical(record)
	must(t, err)
	if string(data) != string(expected) {
		t.Fatalf("canonical winner changed: %q", data)
	}
}

func TestInfrastructurePassiveReadBoundaries(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenInfrastructureContext(t.Context(), true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	reader := infrastructureReader(t, store)
	base := filepath.Join(store.Root, infrastructureDirectory)
	must(t, os.WriteFile(filepath.Join(base, "oversize.json"), make([]byte, task.MaxControlRecordSize+1), 0o600))
	must(t, os.Mkdir(filepath.Join(base, "directory.json"), 0o700))
	outside := filepath.Join(store.Root, "outside.json")
	must(t, os.WriteFile(outside, []byte("outside"), 0o600))
	must(t, os.Symlink(outside, filepath.Join(base, "symlink.json")))
	for _, name := range []string{"oversize.json", "directory.json", "symlink.json", "../outside.json"} {
		if _, err := reader.ReadInfrastructureRecordContext(t.Context(), name); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s accepted or misclassified missing: %v", name, err)
		}
	}
	if _, err := reader.ReadInfrastructureRecordContext(t.Context(), "missing.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing record: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.InfrastructureRecordNamesContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("enumeration cancellation: %v", err)
	}
	if _, err := reader.ReadInfrastructureRecordContext(ctx, "missing.json"); !errors.Is(err, context.Canceled) {
		t.Fatalf("read cancellation: %v", err)
	}
	if _, err := store.OpenInfrastructureContext(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("open cancellation: %v", err)
	}
	must(t, reader.Close())
	if _, err := reader.InfrastructureRecordNamesContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed enumeration: %v", err)
	}
	if _, err := reader.ReadInfrastructureRecordContext(t.Context(), "missing.json"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed read: %v", err)
	}
	must(t, control.Close())
	if _, err := control.PutContext(t.Context(), "closed.json", controlTestRecord{}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed write: %v", err)
	}
}

func TestInfrastructureReadsStayAtPinnedRoot(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenInfrastructureContext(t.Context(), true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	_, err = control.PutContext(t.Context(), "intent.json", controlTestRecord{Request: "original"})
	must(t, err)
	reader := infrastructureReader(t, store)
	retained := store.Root + "-retained"
	must(t, os.Rename(store.Root, retained))
	// The renamed root remains under testStore's temporary parent, whose cleanup
	// runs after the reader, control directory and store are closed.
	replacement := filepath.Join(store.Root, infrastructureDirectory)
	must(t, os.MkdirAll(replacement, 0o700))
	must(t, os.WriteFile(filepath.Join(replacement, "replacement.json"), []byte("replacement"), 0o600))
	names, err := reader.InfrastructureRecordNamesContext(t.Context())
	must(t, err)
	if !slices.Contains(names, "intent.json") || slices.Contains(names, "replacement.json") {
		t.Fatalf("read retargeted: %v", names)
	}
	data, err := reader.ReadInfrastructureRecordContext(t.Context(), "intent.json")
	must(t, err)
	if string(data) != "{\"request\":\"original\"}\n" {
		t.Fatalf("read retargeted: %q", data)
	}
}
