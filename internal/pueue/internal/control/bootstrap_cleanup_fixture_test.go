package control

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
	"golang.org/x/sys/unix"
)

type bootstrapCleanupFixture struct {
	path    string
	lock    *bootstrapLock
	owner   *BootstrapCleanupError
	handles []*Pending
}

func newBootstrapCleanupFixture(t *testing.T) *bootstrapCleanupFixture {
	t.Helper()
	base, err := os.MkdirTemp("", "delegate-bootstrap-cleanup-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &bootstrapCleanupFixture{}
	t.Cleanup(func() {
		for _, handle := range fixture.handles {
			transport.CompletePending(handle, CommandResult{})
		}
		if fixture.owner != nil {
			awaitBootstrapCleanup(t, fixture.owner)
			// Command faults are asserted by the test; Close still releases custody.
			if closeErr := fixture.owner.Close(); closeErr != nil && fixture.owner.CleanupPending() {
				t.Errorf("unresolved bootstrap cleanup: %v", closeErr)
			}
		}
		if releaseErr := closeBootstrapLock(fixture.lock); releaseErr != nil {
			t.Error(releaseErr)
			return
		}
		if fixture.owner != nil && fixture.owner.CleanupPending() {
			t.Errorf("retained bootstrap fixture at %s", base)
			return
		}
		if removeErr := os.RemoveAll(base); removeErr != nil {
			t.Error(removeErr)
		}
	})
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	fixture.path = filepath.Join(base, "bootstrap.lock")
	fixture.lock, err = acquireBootstrapLock(t.Context(), fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *bootstrapCleanupFixture) pending() *Pending {
	handle := transport.NewPending(nil)
	f.handles = append(f.handles, handle)
	return handle
}

func (f *bootstrapCleanupFixture) assertLocked(t *testing.T) {
	t.Helper()
	lock, err := acquireExistingSupervisorLock(t.Context(), f.path, unix.LOCK_EX|unix.LOCK_NB)
	if closeErr := closeBootstrapLock(lock); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(err, errSupervisorLockBusy) {
		t.Fatalf("bootstrap native lock was not retained: %v", err)
	}
}

func (f *bootstrapCleanupFixture) assertReleased(t *testing.T) {
	t.Helper()
	lock, err := acquireExistingSupervisorLock(t.Context(), f.path, unix.LOCK_EX|unix.LOCK_NB)
	if err = errors.Join(err, closeBootstrapLock(lock)); err != nil {
		t.Fatalf("bootstrap native lock remains held: %v", err)
	}
}

func awaitBootstrapCleanup(t *testing.T, owner *BootstrapCleanupError) {
	t.Helper()
	select {
	case <-owner.Done():
	case <-time.After(time.Second):
		t.Fatal("bootstrap natural cleanup did not complete")
	}
}
