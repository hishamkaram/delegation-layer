package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
)

func TestExistingStoreMetadataDoesNotProbeOrCreateFiles(t *testing.T) {
	store := testStore(t)
	probe := filepath.Join(store.Root, ".probe")
	must(t, os.Remove(probe))
	must(t, os.WriteFile(probe, []byte("probe must not run"), 0o600))
	before, err := os.ReadDir(store.Root)
	must(t, err)
	opened, err := OpenExistingStoreMetadataContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	if opened.RootID != store.RootID {
		t.Fatal("metadata open changed root identity")
	}
	must(t, opened.Close())
	after, err := os.ReadDir(store.Root)
	must(t, err)
	if len(before) != len(after) {
		t.Fatal("metadata open created root entries")
	}
	for index, entry := range before {
		if entry.Name() != after[index].Name() {
			t.Fatal("metadata open changed root inventory")
		}
	}
	data, err := os.ReadFile(probe)
	must(t, err)
	if string(data) != "probe must not run" {
		t.Fatal("metadata open changed the forbidden probe sentinel")
	}
}

func TestExistingStoreMetadataRefusesMissingMaintenanceLockWithoutCreation(t *testing.T) {
	store := testStore(t)
	path := filepath.Join(store.Root, ".maintenance.lock")
	must(t, os.Remove(path))
	opened, err := OpenExistingStoreMetadataContext(t.Context(), store.Root, predicate.Default())
	if opened != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing maintenance inode was repaired: store=%v err=%v", opened, err)
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata open created missing lock: %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = OpenExistingStoreMetadataContext(canceled, store.Root, predicate.Default()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled metadata open: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err = OpenExistingStoreMetadataContext(t.Context(), missing, predicate.Default()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root was initialized: %v", err)
	}
	if _, err = os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root was created: %v", err)
	}
}
