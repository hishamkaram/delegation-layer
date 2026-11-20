package infrastructure

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

var _ pueue.DaemonWaitQuiescenceOwner = (*daemonWaitOwner)(nil)

func TestRootDaemonWaitQuiescenceRequiresActualCurrentWaitCompletion(t *testing.T) {
	f, done, expected := daemonBirthFixture(t)
	w := f.server.waits
	if err := w.RegisterDaemonWait(f.root, expected.PID(), done); err != nil {
		t.Fatal(err)
	}
	pendingErr := w.RequireRootDaemonWaitQuiescence(t.Context(), f.root)
	var origin interface{ RootDaemonWaitQuiescencePending() }
	if !errors.Is(pendingErr, ErrOwnerPending) || !errors.As(pendingErr, &origin) {
		t.Fatalf("unfinished Wait lost its owner-authenticated pending origin: %v", pendingErr)
	}
	close(done)
	if err := w.RequireRootDaemonWaitQuiescence(t.Context(), f.root); err != nil {
		t.Fatalf("actual Wait completion and clean history refused: %v", err)
	}
	// This observation retains canonical handles; it does not close registration
	// or turn an empty collection into fabricated per-process closure evidence.
	if err := w.requireRegistrationOpen(); err != nil {
		t.Fatal(err)
	}
}

func TestRootDaemonWaitQuiescenceRefusesHistoricalCustodian(t *testing.T) {
	f := waitHistoryFixtureWithSecond(t, false, true)
	if err := f.server.waits.RequireRootDaemonWaitQuiescence(t.Context(), f.root); err == nil {
		t.Fatal("empty current registry hid a live historical custodian")
	}
}

func TestRootDaemonWaitQuiescencePreservesServingAndRegistrationBoundaries(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	w := f.server.waits
	if err := w.RequireRootDaemonWaitQuiescence(t.Context(), "/foreign-root"); !errors.Is(err, ErrIdentity) {
		t.Fatalf("foreign root borrowed quiescence: %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := w.RequireRootDaemonWaitQuiescence(canceled, f.root); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled observer acquired quiescence: %v", err)
	}
	w.closeRegistration()
	if err := w.RequireRootDaemonWaitQuiescence(t.Context(), f.root); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed registry supplied new Start authority: %v", err)
	}
}
