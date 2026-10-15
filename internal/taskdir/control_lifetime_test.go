package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestControlLifetimeOwnerExcludesAnotherOwner(t *testing.T) {
	store := testStore(t)
	taskID := strings.Repeat("a", 32)
	first, err := store.OpenInspection(taskID, true)
	must(t, err)
	second, err := store.OpenInspection(taskID, false)
	must(t, err)
	t.Cleanup(func() { must(t, second.Close()) })
	t.Cleanup(func() { must(t, first.Close()) })

	owner, err := first.AcquireLifetimeOwner(t.Context())
	must(t, err)
	if _, err = os.Stat(filepath.Join(first.dir, controlLifetimeOwnerLockName)); err != nil {
		t.Fatalf("lifetime owner lock was not durable: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	other, err := second.AcquireLifetimeOwner(ctx)
	if other != nil {
		t.Fatal("second lifetime owner acquired the lease")
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("second lifetime owner error = %v, want bounded busy deadline", err)
	}
	must(t, owner.Release())
}

func TestControlLifetimeOwnerReleaseIsIdempotent(t *testing.T) {
	store := testStore(t)
	taskID := strings.Repeat("b", 32)
	control, err := store.OpenInspection(taskID, true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })

	owner, err := control.AcquireLifetimeOwner(t.Context())
	must(t, err)
	must(t, owner.Release())
	must(t, owner.Release())

	second, err := control.AcquireLifetimeOwner(t.Context())
	must(t, err)
	must(t, second.Release())
}

func TestControlLifetimeOwnerCloseRace(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenInspection(strings.Repeat("c", 32), true)
	must(t, err)
	owner, err := control.AcquireLifetimeOwner(t.Context())
	must(t, err)

	closeResult := make(chan error, 1)
	releaseResult := make(chan error, 1)
	go func() { closeResult <- control.Close() }()
	go func() { releaseResult <- owner.Release() }()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for name, result := range map[string]<-chan error{
		"close":   closeResult,
		"release": releaseResult,
	} {
		select {
		case err = <-result:
			if err != nil {
				t.Fatalf("%s returned error: %v", name, err)
			}
		case <-timer.C:
			t.Fatalf("%s did not complete", name)
		}
	}
	must(t, owner.Release())
}

func TestControlLifetimeOwnerRejectsCanceledContext(t *testing.T) {
	store := testStore(t)
	control, err := store.OpenInspection(strings.Repeat("d", 32), true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	owner, err := control.AcquireLifetimeOwner(ctx)
	if owner != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquisition = owner %v, err %v", owner, err)
	}
}
