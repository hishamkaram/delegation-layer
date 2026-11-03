package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func leasePath(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "instance.lock")
}

func acquire(t *testing.T, path string) *Lease {
	t.Helper()
	lease, ok, err := TryAcquireLease(t.Context(), path)
	if err != nil || !ok {
		t.Fatalf("acquire: %v, %v", ok, err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	return lease
}

func assertAvailable(t *testing.T, path string, want bool) {
	t.Helper()
	lease, ok, err := TryAcquireLease(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if lease != nil {
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if ok != want {
		t.Fatalf("available = %v, want %v", ok, want)
	}
}

func TestLeaseInheritedReferenceRetainsExclusion(t *testing.T) {
	path := leasePath(t)
	parent := acquire(t, path)
	assertAvailable(t, path, false)
	var duplicate *os.File
	err := parent.WithHeldFile(t.Context(), func(file *os.File) error {
		fd, err := unix.Dup(int(file.Fd()))
		if err != nil {
			return err
		}
		duplicate = os.NewFile(uintptr(fd), path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := AdoptInheritedLease(duplicate, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := child.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := child.WithHeldFile(t.Context(), func(file *os.File) error {
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
		if err != nil {
			return err
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Error("adopted descriptor lacks CLOEXEC")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	assertAvailable(t, path, false)
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	assertAvailable(t, path, true)
	if err := child.WithHeldFile(t.Context(), func(*os.File) error { t.Error("closed lease callback ran"); return nil }); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed borrow: %v", err)
	}
	assertAvailable(t, path, true)
}

func TestLeaseRejectsChangedPath(t *testing.T) {
	path := leasePath(t)
	lease := acquire(t, path)
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lease.WithHeldFile(t.Context(), func(*os.File) error { t.Error("replaced path accepted"); return nil }); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("replacement: %v", err)
	}
}

func TestAdoptRejectsInvalidDescriptors(t *testing.T) {
	for _, kind := range []string{"wrong-inode", "public-mode", "directory", "read-only"} {
		t.Run(kind, func(t *testing.T) {
			path := leasePath(t)
			held := acquire(t, path)
			file := invalidLeaseDescriptor(t, path, kind)
			if adopted, err := AdoptInheritedLease(file, path); err == nil {
				if closeErr := adopted.Close(); closeErr != nil {
					t.Error(closeErr)
				}
				t.Fatal("invalid inherited descriptor accepted")
			}
			if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("failed adoption did not consume descriptor: %v", err)
			}
			if err := held.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCloseSerializesWithBorrower(t *testing.T) {
	path := leasePath(t)
	lease := acquire(t, path)
	entered, release, borrowed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		borrowed <- lease.WithHeldFile(context.Background(), func(file *os.File) error {
			close(entered)
			<-release
			_, err := file.Stat()
			return err
		})
	}()
	<-entered
	// The borrower holds the actual closure guard, rather than a launch barrier.
	if lease.mu.TryLock() {
		lease.mu.Unlock()
		t.Error("borrower did not hold closure guard")
	}
	closed := make(chan error, 1)
	go func() { closed <- lease.Close() }()
	assertAvailable(t, path, false)
	close(release)
	if err := <-borrowed; err != nil {
		t.Error(err)
	}
	if err := <-closed; err != nil {
		t.Error(err)
	}
	assertAvailable(t, path, true)
}

func TestLeaseRequiresExistingPrivateParentAndContext(t *testing.T) {
	path := filepath.Join(filepath.Dir(leasePath(t)), "missing", "instance.lock")
	if _, _, err := TryAcquireLease(t.Context(), path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing parent: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created parent: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := TryAcquireLease(ctx, leasePath(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestAdoptionDoesNotAcquireFreeFlock(t *testing.T) {
	path := leasePath(t)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately violate the trusted-held-inheritance precondition to prove
	// this constructor never turns observation into election.
	lease, err := AdoptInheritedLease(file, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})
	assertAvailable(t, path, true)
}

func TestLeaseDescriptorFreeBorrowHonorsClosure(t *testing.T) {
	lease := acquire(t, leasePath(t))
	called := false
	if err := lease.WithHeld(t.Context(), func() error {
		called = true
		if lease.mu.TryLock() {
			lease.mu.Unlock()
			t.Error("descriptor-free operation escaped the closure guard")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("checked operation was not called")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.WithHeld(t.Context(), func() error { t.Error("closed lease authorized operation"); return nil }); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed descriptor-free borrow: %v", err)
	}
	if err := lease.WithHeld(t.Context(), nil); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("missing operation: %v", err)
	}
}

func invalidLeaseDescriptor(t *testing.T, path, kind string) *os.File {
	t.Helper()
	other := path
	flags := os.O_RDWR
	if kind == "wrong-inode" {
		other += ".other"
		if err := os.WriteFile(other, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "public-mode" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "directory" {
		other = filepath.Dir(path)
		flags = os.O_RDONLY
	}
	if kind == "read-only" {
		flags = os.O_RDONLY
	}
	file, err := os.OpenFile(other, flags, 0)
	if err != nil {
		t.Fatal(err)
	}
	return file
}
