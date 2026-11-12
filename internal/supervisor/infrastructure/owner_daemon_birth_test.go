package infrastructure

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

// These controlled futures protect the registry contract only. They are not
// native process births or daemon Wait evidence; canonical Start tests own that.
func daemonBirthFixture(t *testing.T) (*ownerServerFixture, chan struct{}, lifetime.ProcessRef) {
	t.Helper()
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	done := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			close(done)
		}
	})
	expected, err := lifetime.NewProcessRef(101, "recorded-birth-one")
	if err != nil {
		t.Fatal(err)
	}
	return f, done, expected
}

func TestDaemonWaitBirthPendingAndCanceledObservationRetainFuture(t *testing.T) {
	f, done, expected := daemonBirthFixture(t)
	w := f.server.waits
	ready := make(chan struct{})
	var reads atomic.Int32
	birth := func() (lifetime.ProcessRef, bool, error) {
		reads.Add(1)
		select {
		case <-ready:
			return expected, true, nil
		default:
			return lifetime.ProcessRef{}, false, nil
		}
	}
	if err := w.RegisterDaemonWaitBirth(f.root, expected.PID(), done, birth); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := w.RequireDaemonWaitCustody(ctx, f.root, expected); !errors.Is(err, ErrOwnerBusy) {
		t.Fatalf("pending birth was awaited or treated as custody: %v", err)
	}
	canceled, cancelObservation := context.WithCancel(t.Context())
	cancelObservation()
	before := reads.Load()
	if err := w.RequireDaemonWaitCustody(canceled, f.root, expected); !errors.Is(err, context.Canceled) || reads.Load() != before {
		t.Fatalf("canceled observer consumed the future: %v", err)
	}
	close(ready)
	if err := w.RequireDaemonWaitCustody(t.Context(), f.root, expected); err != nil {
		t.Fatalf("original observer cancellation left custody permanently unbound: %v", err)
	}
	other, err := lifetime.NewProcessRef(expected.PID(), "recorded-other-birth")
	if err != nil {
		t.Fatal(err)
	}
	if err = w.RequireDaemonWaitCustody(t.Context(), f.root, other); !errors.Is(err, ErrDaemonWaitCustody) {
		t.Fatalf("same PID with a different birth was accepted: %v", err)
	}
	if err = w.RequireDaemonWaitCustody(t.Context(), "/foreign-root", expected); !errors.Is(err, ErrIdentity) {
		t.Fatalf("foreign root borrowed custody: %v", err)
	}
}

func TestDaemonWaitBirthRegistrationIsExactAndCannotBeRebound(t *testing.T) {
	f, done, expected := daemonBirthFixture(t)
	w := f.server.waits
	birth := func() (lifetime.ProcessRef, bool, error) { return expected, true, nil }
	if err := w.RegisterDaemonWait(f.root, expected.PID(), done); err != nil {
		t.Fatal(err)
	}
	if err := w.RequireDaemonWaitCustody(t.Context(), f.root, expected); !errors.Is(err, ErrDaemonWaitCustody) {
		t.Fatalf("diagnostic PID/completion became birth authority: %v", err)
	}
	if err := w.RegisterDaemonWaitBirth(f.root, expected.PID(), done, birth); err != nil {
		t.Fatal(err)
	}
	if err := w.RegisterDaemonWait(f.root, expected.PID(), done); err != nil {
		t.Fatal(err)
	}
	if err := w.RequireDaemonWaitCustody(t.Context(), f.root, expected); err != nil {
		t.Fatalf("legacy duplicate erased exact birth: %v", err)
	}
	if err := w.RegisterDaemonWaitBirth(f.root, expected.PID()+1, done, birth); !errors.Is(err, ErrIdentity) {
		t.Fatalf("completion handle rebound to another PID: %v", err)
	}
	if err := w.RegisterDaemonWaitBirth(f.root, expected.PID(), done, birth); !errors.Is(err, ErrIdentity) {
		t.Fatalf("existing immutable getter was replaceable: %v", err)
	}
	if err := w.RegisterDaemonWaitBirth(f.root, expected.PID(), done, nil); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil birth getter accepted: %v", err)
	}
	w.closeRegistration()
	if err := w.RequireDaemonWaitCustody(t.Context(), f.root, expected); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("draining registry still granted eligibility: %v", err)
	}
}

func TestDaemonWaitBirthFaultAndCompletionCannotForgetActualWait(t *testing.T) {
	f, done, expected := daemonBirthFixture(t)
	w := f.server.waits
	cause := errors.New("recorded birth observation failure")
	if err := w.RegisterDaemonWaitBirth(f.root, expected.PID(), done, func() (lifetime.ProcessRef, bool, error) { return lifetime.ProcessRef{}, true, cause }); err != nil {
		t.Fatal(err)
	}
	if err := w.RequireDaemonWaitCustody(t.Context(), f.root, expected); !errors.Is(err, cause) || !errors.Is(err, ErrDaemonWaitCustody) {
		t.Fatalf("birth fault disappeared or became custody: %v", err)
	}
	if len(w.active) != 1 {
		t.Fatal("failed observation forgot the actual completion handle")
	}
	close(done)
	if err := w.RequireDaemonWaitCustody(t.Context(), f.root, expected); !errors.Is(err, ErrDaemonWaitCustody) || len(w.active) != 0 {
		t.Fatalf("completed Wait was reused for a live daemon: %v", err)
	}
}

func TestDaemonWaitBirthCannotBeBorrowedFromAnotherOrRetiredRegistry(t *testing.T) {
	f, done, expected := daemonBirthFixture(t)
	w := f.server.waits
	if err := w.RegisterDaemonWaitBirth(f.root, expected.PID(), done, func() (lifetime.ProcessRef, bool, error) { return expected, true, nil }); err != nil {
		t.Fatal(err)
	}
	other, err := newDaemonWaitOwner(w.origin)
	if err != nil {
		t.Fatal(err)
	}
	other.resident = f.entry // Same native boundary, independent empty registry.
	if err = other.RequireDaemonWaitCustody(t.Context(), f.root, expected); !errors.Is(err, ErrDaemonWaitCustody) {
		t.Fatalf("another registry adopted an existing Wait: %v", err)
	}
	path := f.entry.endpoint.Identity().Path
	moved := path + ".displaced"
	if err = os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	err = w.RequireDaemonWaitCustody(t.Context(), f.root, expected)
	if restoreErr := os.Rename(moved, path); restoreErr != nil {
		f.retain = true
		t.Fatalf("restore owned endpoint before cleanup: %v", restoreErr)
	}
	if err == nil {
		t.Fatal("displaced publication/socket still granted restart eligibility")
	}
	f.entry.mu.Lock()
	f.entry.closed = true
	f.entry.mu.Unlock()
	err = w.RequireDaemonWaitCustody(t.Context(), f.root, expected)
	f.entry.mu.Lock()
	f.entry.closed = false
	f.entry.mu.Unlock()
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("retired serving entry still granted custody: %v", err)
	}
}
