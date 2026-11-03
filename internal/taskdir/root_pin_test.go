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
	"golang.org/x/sys/unix"
)

func TestRootPinIsPassiveAndAgreesAcrossOwners(t *testing.T) {
	store := testStore(t)
	reader := infrastructureReader(t, store)
	before := rootPinEntryNames(t, store.Root)
	pin, err := store.PinRootContext(t.Context())
	must(t, err)
	readonly, err := reader.PinRootContext(t.Context())
	must(t, err)
	if !pin.Equal(readonly) || pin.Root() != store.Root || pin.RootID() != store.RootID {
		t.Fatalf("store and reader identity disagree: %#v %#v", pin, readonly)
	}
	// Compare against a separate actual filesystem observation, not a recreated
	// pathname-derived expected pin.
	directory, err := os.Open(store.Root)
	must(t, err)
	defer func() { must(t, directory.Close()) }()
	info, err := directory.Stat()
	must(t, err)
	visible, err := os.Lstat(pin.Root())
	must(t, err)
	if !os.SameFile(info, visible) {
		t.Fatal("pin root path differs from actual directory")
	}
	var native unix.Stat_t
	must(t, unix.Fstat(int(directory.Fd()), &native))
	if pin.Inode() != uint64(native.Ino) || pin.Device() != uint64(native.Dev) {
		t.Fatal("pin did not record the actual native directory identity")
	}
	if after := rootPinEntryNames(t, store.Root); !slices.Equal(before, after) {
		t.Fatalf("observation changed entries: %v -> %v", before, after)
	}
}

func TestRootPinRejectsCancellationAndClosedOwners(t *testing.T) {
	store := testStore(t)
	reader := infrastructureReader(t, store)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.PinRootContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("store cancellation: %v", err)
	}
	if _, err := reader.PinRootContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("reader cancellation: %v", err)
	}
	must(t, reader.Close())
	if _, err := reader.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed reader: %v", err)
	}
	must(t, store.Close())
	if _, err := store.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed store: %v", err)
	}
}

func TestRootPinRejectsReplacementWithIdenticalMetadata(t *testing.T) {
	store := testStore(t)
	reader := infrastructureReader(t, store)
	original, err := store.PinRootContext(t.Context())
	must(t, err)
	metadata, err := os.ReadFile(filepath.Join(store.Root, "root.json"))
	must(t, err)
	retained := store.Root + "-retained"
	must(t, os.Rename(store.Root, retained))
	// Both roots stay inside testStore's temporary parent, whose cleanup follows
	// closure of all stores and readers, including the original pinned handles.
	must(t, os.Mkdir(store.Root, 0o700))
	must(t, os.WriteFile(filepath.Join(store.Root, "root.json"), metadata, 0o600))
	if _, err = store.PinRootContext(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("original store accepted replaced root: %v", err)
	}
	if _, err = reader.PinRootContext(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("original reader accepted replaced root: %v", err)
	}
	clone, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, clone.Close()) })
	cloned, err := clone.PinRootContext(t.Context())
	must(t, err)
	if cloned.Root() != original.Root() || cloned.RootID() != original.RootID() {
		t.Fatal("fixture did not preserve metadata identity")
	}
	if cloned.Equal(original) {
		t.Fatal("native directory replacement compared equal")
	}
	if cloned.Device() == original.Device() && cloned.Inode() == original.Inode() {
		t.Fatal("fixture did not replace native directory")
	}
}

func TestRootPinRejectsMissingAliasedAndUnprotectedPath(t *testing.T) {
	store := testStore(t)
	retained := store.Root + "-retained"
	must(t, os.Rename(store.Root, retained))
	if _, err := store.PinRootContext(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing path causes lost: %v", err)
	}
	must(t, os.Symlink(retained, store.Root))
	if _, err := store.PinRootContext(t.Context()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("alias accepted: %v", err)
	}
	must(t, os.Remove(store.Root))
	must(t, os.Rename(retained, store.Root))
	must(t, os.Chmod(store.Root, 0o755))
	if _, err := store.PinRootContext(t.Context()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unprotected root accepted: %v", err)
	}
}

func rootPinEntryNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	must(t, err)
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}

func TestRootPinRejectsUnverifiableNativeInode(t *testing.T) {
	store := testStore(t)
	directory, err := os.Open(store.Root)
	must(t, err)
	defer func() { must(t, directory.Close()) }()
	var native unix.Stat_t
	must(t, unix.Fstat(int(directory.Fd()), &native))
	if privateRootIdentityMismatch(&native, &native) {
		t.Fatal("actual private directory identity refused")
	}
	zeroInode := native
	zeroInode.Ino = 0
	if !privateRootIdentityMismatch(&zeroInode, &zeroInode) {
		t.Fatal("matching zero inodes were treated as verifiable identity")
	}
	zeroDevice := native
	zeroDevice.Dev = 0
	if privateRootIdentityMismatch(&zeroDevice, &zeroDevice) {
		t.Fatal("device zero was rejected despite verifiable inode")
	}
}
