package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAdmissionCreationKeepsGenerationLeaseThroughCommit(t *testing.T) {
	f, _ := newWarmContinuationFixture(t)
	// Validated generation/capability input only: no real daemon or group
	// mutation is needed to test the shared generation boundary.
	if err := publishPrivateOrdinaryScheduling(f.store.Root, f.target, pueue.StagedLaunchCapability{AddStashed: true, EnqueueByID: true}); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(f.store.Root, ".supervisor", "bootstrap.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	exited := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)
	t.Cleanup(func() { finish(); joinAdmissionCreation(t, exited) })
	go func() {
		defer close(exited)
		done <- WithAdmissionCreation(t.Context(), f.store.Root, f.target, f.target, func(context.Context) error { close(entered); <-release; return nil })
	}()
	select {
	case <-entered:
	case err = <-done:
		t.Fatalf("creation gate failed before commit: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("creation watchdog")
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		finish()
		<-done
		t.Fatalf("exclusive handoff entered during commit: %v", err)
	}
	finish()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("creation cleanup watchdog")
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("creation retained generation after commit: %v", err)
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
}

func joinAdmissionCreation(t *testing.T, exited <-chan struct{}) {
	t.Helper()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Error("creation owner did not join")
	}
}

func TestAdmissionCreationPrivateAliasCannotBecomeExternal(t *testing.T) {
	f, _ := newWarmContinuationFixture(t)
	runtime := f.target
	runtime.ConfigPath = filepath.Join(f.store.Root, "migration-transport.yml")
	called := false
	err := WithAdmissionCreation(t.Context(), f.store.Root, f.target, runtime, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, pueue.ErrBinding) || called {
		t.Fatalf("alias became external creation: called=%t err=%v", called, err)
	}
}

func TestAdmissionCreationRejectsDrainBeforeCreate(t *testing.T) {
	f, _ := newWarmContinuationFixture(t)
	pin, err := f.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	writeContinuationFixtureRecord(t, filepath.Join(f.store.Root, ".supervisor", "bootstrap.pending"), map[string]any{
		"schema_version": task.SchemaVersion, "drain_v1": map[string]any{"root": map[string]any{"path": pin.Root(), "id": pin.RootID(), "device": pin.Device(), "inode": pin.Inode()}, "predecessor": f.target, "operation_id": "upgrade-v1:99999999999999999999999999999999", "mode": "operation_required", "phase": "draining"},
	})
	called := false
	err = WithAdmissionCreation(t.Context(), f.store.Root, f.target, f.target, func(context.Context) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("creation crossed committed Drain: called=%t err=%v", called, err)
	}
}
