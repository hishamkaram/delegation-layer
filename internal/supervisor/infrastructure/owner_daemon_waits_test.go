package infrastructure

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func daemonWaitOwnerFixture(t *testing.T) *daemonWaitOwner {
	t.Helper()
	origin, err := NewOrigin("/fixture", strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := newDaemonWaitOwner(origin)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func TestDaemonWaitOwnerBindsRootAndCompletionHandle(t *testing.T) {
	waits := daemonWaitOwnerFixture(t)
	first, second := make(chan struct{}), make(chan struct{})
	defer close(first)
	defer close(second)
	if err := waits.RegisterDaemonWait("/other", 42, first); !errors.Is(err, ErrIdentity) {
		t.Fatalf("foreign root registered: %v", err)
	}
	if err := waits.RegisterDaemonWait("/fixture", 42, nil); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil completion registered: %v", err)
	}
	for _, done := range []<-chan struct{}{first, first, second} {
		if err := waits.RegisterDaemonWait("/fixture", 42, done); err != nil {
			t.Fatal(err)
		}
	}
	if len(waits.active) != 2 {
		t.Fatalf("distinct Waits sharing a reused PID were conflated: %d", len(waits.active))
	}
	if err := waits.RegisterDaemonWait("/fixture", 43, first); !errors.Is(err, ErrIdentity) {
		t.Fatalf("same handle accepted a different PID: %v", err)
	}
	waits.closeRegistration()
	if err := waits.RegisterDaemonWait("/fixture", 42, first); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("late registration accepted: %v", err)
	}
}

func TestDaemonWaitOwnerJoinsAllHandlesAfterRegistrationCloses(t *testing.T) {
	waits := daemonWaitOwnerFixture(t)
	first, second := make(chan struct{}), make(chan struct{})
	for _, done := range []<-chan struct{}{first, second} {
		if err := waits.RegisterDaemonWait("/fixture", 42, done); err != nil {
			t.Fatal(err)
		}
	}
	if err := waits.join(t.Context()); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("join before handler drain/registration close succeeded: %v", err)
	}
	waits.closeRegistration()
	close(first)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := waits.join(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("uncompleted Wait was not retained: %v", err)
	}
	if len(waits.active) != 1 {
		t.Fatalf("timed-out join lost pending completion: %d", len(waits.active))
	}
	close(second)
	if err := waits.join(t.Context()); err != nil || len(waits.active) != 0 {
		t.Fatalf("completed join = %v, retained=%d", err, len(waits.active))
	}
}

func TestDaemonWaitOwnerConcurrentRegistrationAndPruning(t *testing.T) {
	waits := daemonWaitOwnerFixture(t)
	completed := make(chan struct{})
	close(completed)
	var workers sync.WaitGroup
	for range 20 {
		workers.Go(func() {
			if err := waits.RegisterDaemonWait("/fixture", 42, completed); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	waits.closeRegistration()
	if err := waits.join(t.Context()); err != nil || len(waits.active) != 0 {
		t.Fatalf("completed handles retained: %v, %d", err, len(waits.active))
	}
}
