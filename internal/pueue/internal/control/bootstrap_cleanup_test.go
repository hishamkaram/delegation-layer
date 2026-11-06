package control

import (
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
)

func TestBootstrapCleanupSettledResultsReturnedSynchronously(t *testing.T) {
	firstErr := errors.New("first completed command")
	secondErr := errors.New("second completed command")
	first := transport.NewCompletedPending(nil, CommandResult{Err: firstErr})
	second := transport.NewCompletedPending(nil, CommandResult{Err: secondErr})
	resultErr := errors.Join(&InFlightError{Pending: first}, &InFlightError{Pending: second})
	var lock *bootstrapLock
	finishPrivateBootstrap(&lock, &resultErr)
	var owner *BootstrapCleanupError
	if !errors.As(resultErr, &owner) {
		t.Fatal("settled diagnostics lost their cleanup owner")
	}
	if !errors.Is(resultErr, ErrInFlight) {
		t.Fatal("settled completion changed the original uncertainty classification")
	}
	for _, want := range []error{firstErr, secondErr} {
		if !errors.Is(owner.Err(), want) {
			t.Errorf("synchronous diagnostics lost %v: %v", want, owner.Err())
		}
	}
	if errors.Is(resultErr, firstErr) || errors.Is(resultErr, secondErr) || owner.CleanupPending() {
		t.Fatal("settled diagnostics changed the callback fault graph or retained custody")
	}
}

func TestBootstrapCleanupSettledCloseFaultReturnedSynchronously(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	lock := &bootstrapLock{file: file}
	resultErr := error(&InFlightError{Pending: transport.NewCompletedPending(nil, CommandResult{})})
	finishPrivateBootstrap(&lock, &resultErr)
	if !errors.Is(resultErr, os.ErrClosed) {
		t.Fatalf("settled bootstrap close fault was deferred or lost: %v", resultErr)
	}
	var owner *BootstrapCleanupError
	if !errors.As(resultErr, &owner) || owner.CleanupPending() {
		t.Fatal("failed synchronous close was not recorded as a retired Go handle")
	}
	select {
	case <-owner.Done():
	default:
		t.Fatal("settled cleanup was deferred")
	}
	if !errors.Is(owner.Close(), os.ErrClosed) {
		t.Fatal("cleanup status lost synchronous close fault")
	}
}

func TestBootstrapCleanupSettledNativeLockClosesBeforeReturn(t *testing.T) {
	fixture := newBootstrapCleanupFixture(t)
	pending := fixture.pending()
	transport.CompletePending(pending, CommandResult{})
	resultErr := error(&InFlightError{Pending: pending})
	finishPrivateBootstrap(&fixture.lock, &resultErr)
	fixture.assertReleased(t)
	if errors.As(resultErr, &fixture.owner) {
		t.Fatal("settled successful native release unnecessarily retained an owner")
	}
}
