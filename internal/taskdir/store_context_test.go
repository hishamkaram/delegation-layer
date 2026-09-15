package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
)

func TestCanceledStoreOpenDoesNotInitializeOrProbe(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-store")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store, err := OpenStoreWithPredicatesContext(ctx, missing, predicate.Default())
	if store != nil {
		t.Cleanup(func() {
			if closeErr := store.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if store != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open returned store=%v error=%v", store, err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled open created a store: %v", err)
	}
}

func TestStoreProbeDeadlineLeavesNoProbeAndAllowsRetry(t *testing.T) {
	store := testStore(t)
	if err := store.maintLock.LockEXNonblocking(); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			if err := store.maintLock.Unlock(); err != nil {
				t.Error(err)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	opened, err := OpenStoreWithPredicatesContext(ctx, store.Root, predicate.Default())
	if opened != nil {
		unexpected := opened
		t.Cleanup(func() {
			if closeErr := unexpected.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if opened != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired store open returned store=%v error=%v", opened, err)
	}
	entries, err := os.ReadDir(filepath.Join(store.Root, ".probe"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("expired probe left artifacts: entries=%v error=%v", entries, err)
	}
	if unlockErr := store.maintLock.Unlock(); unlockErr != nil {
		t.Fatal(unlockErr)
	}
	locked = false
	opened, err = OpenStoreWithPredicatesContext(context.Background(), store.Root, predicate.Default())
	if err != nil {
		t.Fatalf("fresh open after deadline failed: %v", err)
	}
	if closeErr := opened.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}
