package host

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func stateStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store
}

func TestHostLeaseExcludesServeAndRunWithoutOwningE(t *testing.T) {
	store := stateStore(t)
	lease, err := AcquireHostLease(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if other, busyErr := AcquireHostLease(t.Context(), store); !errors.Is(busyErr, task.ErrLockBusy) {
		if other != nil {
			if closeErr := other.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		t.Fatalf("second H owner accepted: %v", busyErr)
	}
	flags, err := unix.FcntlInt(uintptr(lease.lock.Fd()), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("H can leak into a child: flags=%d err=%v", flags, err)
	}
	execution, err := scheduler.AcquireOwnerLease(t.Context(), store)
	if err != nil {
		t.Fatalf("H also acquired E: %v", err)
	}
	if err = execution.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := AcquireHostLease(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHostLeaseKeepsRootsIndependent(t *testing.T) {
	for range 2 {
		lease, err := AcquireHostLease(t.Context(), stateStore(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if closeErr := lease.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
}
