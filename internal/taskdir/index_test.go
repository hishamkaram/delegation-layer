package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func indexReader(t *testing.T, store *Store) *ReadOnlyStore {
	t.Helper()
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	return reader
}

func TestIndexMetadataIsNarrowAndDistinguishesPartialAdmission(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	reader := indexReader(t, store)
	// Metadata search does not read content or claim prepared-set validity.
	must(t, os.Remove(filepath.Join(td.Dir, "brief.md")))
	value, err := reader.ReadTaskIndexMetadata(t.Context(), td.TaskID)
	must(t, err)
	if value.TaskID != td.TaskID || value.Provider != "fixture:test" || !value.CreatedAtKnown || value.RequestDigest == "" || value.MetadataDigest == "" {
		t.Fatalf("lost validated metadata: %+v", value)
	}
	must(t, os.Remove(filepath.Join(td.Dir, "meta.json")))
	partial, err := reader.ReadTaskIndexMetadata(t.Context(), td.TaskID)
	must(t, err)
	if partial.CreatedAtKnown || partial.MetadataDigest != "" || partial.RequestDigest != value.RequestDigest {
		t.Fatalf("partial admission fabricated creation time: %+v", partial)
	}
	for _, name := range []string{"brief.md", "meta.json", "submit.json", "provider.start"} {
		if _, err = os.Lstat(filepath.Join(td.Dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("metadata observation repaired %s: %v", name, err)
		}
	}
}

func TestIndexMetadataValidatesRecordBindingAndRejectsSymlinks(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	reader := indexReader(t, store)
	metaPath := filepath.Join(td.Dir, "meta.json")
	data := readTestFile(t, metaPath)
	var meta task.MetaRecord
	must(t, task.DecodeStrict(data, &meta))
	meta.TaskID = strings.Repeat("a", 32)
	changed, err := task.MarshalCanonical(meta)
	must(t, err)
	writeTestFile(t, metaPath, changed)
	if _, err = reader.ReadTaskIndexMetadata(t.Context(), td.TaskID); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("foreign metadata accepted: %v", err)
	}
	must(t, os.Remove(metaPath))
	other := filepath.Join(t.TempDir(), "metadata")
	must(t, os.WriteFile(other, data, 0o600))
	must(t, os.Symlink(other, metaPath))
	if _, err = reader.ReadTaskIndexMetadata(t.Context(), td.TaskID); err == nil {
		t.Fatal("followed metadata symlink")
	}
}

func TestIndexScanCountsInvalidEntriesWithinBatchBudget(t *testing.T) {
	store := testStore(t)
	preparedTask(t, store)
	for _, id := range []string{strings.Repeat("1", 32), strings.Repeat("2", 32), "not-an-id"} {
		must(t, os.Mkdir(filepath.Join(store.Root, "tasks", id), 0o700))
	}
	must(t, os.Symlink(t.TempDir(), filepath.Join(store.Root, "tasks", strings.Repeat("3", 32))))
	reader := indexReader(t, store)
	scan, err := reader.OpenIndexScan(t.Context(), IndexLocal)
	must(t, err)
	t.Cleanup(func() { must(t, scan.Close()) })
	var total, faults int
	for {
		values, done, scanErr := scan.Next(t.Context(), 2)
		must(t, scanErr)
		if len(values) > 2 {
			t.Fatalf("batch exceeded entry budget: %d", len(values))
		}
		total += len(values)
		for _, value := range values {
			if value.Err != nil {
				faults++
			}
		}
		if done {
			break
		}
	}
	if total != 5 || faults != 2 {
		t.Fatalf("entries=%d faults=%d; want 5/2", total, faults)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err = scan.Next(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled enumeration = %v", err)
	}
}

func TestIndexRemoteObservationNamesRetainOnlyNewestGenerations(t *testing.T) {
	store := testStore(t)
	id := strings.Repeat("a", 32)
	directory := filepath.Join(store.Root, "remote-refs", id)
	must(t, os.MkdirAll(directory, 0o700))
	for _, name := range []string{"intent.json", "observation-00000000000000000001.json", "observation-00000000000000000003.json", "observation-00000000000000000002.json"} {
		must(t, os.WriteFile(filepath.Join(directory, name), []byte("{}"), 0o600))
	}
	reader := indexReader(t, store)
	names, err := reader.RemoteIndexObservationNames(t.Context(), id)
	must(t, err)
	if len(names) != 2 || names[0] != "observation-00000000000000000003.json" || names[1] != "observation-00000000000000000002.json" {
		t.Fatalf("wrong retained generations: %v", names)
	}
}
