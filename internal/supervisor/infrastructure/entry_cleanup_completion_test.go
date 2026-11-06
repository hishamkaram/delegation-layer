package infrastructure

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// This completion port has no native actor. The fixture supplies the real
// journal, instance lease and endpoint whose disposal it must order.
type residentCompletionFixtureError struct {
	done    chan struct{}
	entered chan struct{}
	once    sync.Once
	err     error
	pending bool
}

func (*residentCompletionFixtureError) Error() string           { return "owned resident completion" }
func (*residentCompletionFixtureError) RetainsResidentCleanup() {}
func (c *residentCompletionFixtureError) CleanupDone() <-chan struct{} {
	if c.entered != nil {
		c.once.Do(func() { close(c.entered) })
	}
	return c.done
}
func (c *residentCompletionFixtureError) CleanupErr() error    { return c.err }
func (c *residentCompletionFixtureError) CleanupPending() bool { return c.pending }

func TestResidentRuntimeJoinsCompletionBeforeEntryDisposal(t *testing.T) {
	f, waits := newDrainedResidentFixture(t)
	completion := &residentCompletionFixtureError{done: make(chan struct{}), entered: make(chan struct{})}
	var release sync.Once
	finished := make(chan struct{})
	var resultErr error
	t.Cleanup(func() {
		release.Do(func() { close(completion.done) })
		awaitResidentFixture(t, finished)
	})
	go func() {
		defer close(finished)
		resultErr = finishResidentRuntime(t.Context(), f.entry, waits, completion)
	}()
	select {
	case <-completion.entered:
	case <-finished:
		t.Fatal("runtime disposed of the entry without joining owned cleanup")
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not observe the owned completion")
	}
	f.entry.mu.Lock()
	closed, lease := f.entry.closed, f.entry.lease
	f.entry.mu.Unlock()
	if closed || lease == nil {
		t.Fatal("unfinished cleanup released the actual resident entry")
	}
	if err := lease.WithHeld(t.Context(), func() error { return nil }); err != nil {
		t.Fatalf("unfinished cleanup lost the native instance lease: %v", err)
	}
	lateFault := errors.New("late cleanup fault")
	completion.err = lateFault
	release.Do(func() { close(completion.done) })
	awaitResidentFixture(t, finished)
	if !errors.Is(resultErr, lateFault) || !f.entry.closed || f.entry.lease != nil {
		t.Fatalf("runtime lost the late fault or failed final disposal: %v", resultErr)
	}
}

func TestResidentRuntimeCompletedAttemptRetainsUnclosedResources(t *testing.T) {
	f, waits := newDrainedResidentFixture(t)
	done := make(chan struct{})
	close(done)
	fault := errors.New("owned guard release failed")
	completion := &residentCompletionFixtureError{done: done, err: fault, pending: true}
	err := finishResidentRuntime(t.Context(), f.entry, waits, completion)
	var retained *residentEntryCleanupError
	if !errors.Is(err, fault) || !errors.As(err, &retained) || retained.entry != f.entry || f.entry.closed || f.entry.lease == nil {
		t.Fatalf("completed attempt released remaining actual resources: %v", err)
	}
}

func TestResidentRuntimeReleasedHandlesStillReportReleaseFault(t *testing.T) {
	f, waits := newDrainedResidentFixture(t)
	done := make(chan struct{})
	close(done)
	fault := errors.New("native release reported failure after retiring its Go handle")
	err := finishResidentRuntime(t.Context(), f.entry, waits, &residentCompletionFixtureError{done: done, err: fault})
	if !errors.Is(err, fault) || !f.entry.closed || f.entry.lease != nil {
		t.Fatalf("retired handles erased the release fault or blocked owned disposal: %v", err)
	}
}

func TestResidentRuntimeCanceledJoinRetainsEntry(t *testing.T) {
	f, waits := newDrainedResidentFixture(t)
	done := make(chan struct{})
	defer close(done)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := finishResidentRuntime(ctx, f.entry, waits, &residentCompletionFixtureError{done: done})
	if !errors.Is(err, context.Canceled) || f.entry.closed || f.entry.lease == nil {
		t.Fatalf("canceled observation disposed of live resident resources: %v", err)
	}
}

func TestResidentRuntimeInvalidCompletionRetainsEntry(t *testing.T) {
	for _, cause := range []error{&residentCompletionFixtureError{}, (*residentCompletionFixtureError)(nil)} {
		f, waits := newDrainedResidentFixture(t)
		err := finishResidentRuntime(t.Context(), f.entry, waits, cause)
		if !errors.Is(err, ErrIdentity) || f.entry.closed || f.entry.lease == nil {
			t.Fatalf("invalid cleanup completion granted entry disposal: %v", err)
		}
	}
}

func TestResidentCleanupNestedCompletionCannotAuthorizeOuterMarker(t *testing.T) {
	done := make(chan struct{})
	close(done)
	inner := &residentCompletionFixtureError{done: done}
	outer := &retainedPreparationTestError{cause: inner}
	owners, err := residentCleanupCompletions(outer)
	if !errors.Is(err, ErrIdentity) {
		t.Fatalf("inner completion authorized unknown outer resources: %v", err)
	}
	if len(owners) != 1 || owners[0] != inner {
		t.Fatal("invalid outer marker hid the independently owned inner completion")
	}
}

func awaitResidentFixture(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("owned resident completion did not join")
	}
}

// After a verified drain, this test owns entry disposal. The server borrowed
// its listener and must not touch it again after the entry closes it.
func newDrainedResidentFixture(t *testing.T) (*ownerServerFixture, *daemonWaitOwner) {
	t.Helper()
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	waits := f.server.waits
	if err := f.server.stopAcceptAndDrain(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.server.drained:
		f.server = nil
	default:
		t.Fatal("resident fixture has not drained")
	}
	return f, waits
}
