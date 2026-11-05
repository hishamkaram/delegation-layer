package control

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestBootstrapCleanupOwnerRetainsNativeLockAndPendingGraph(t *testing.T) {
	fixture := newBootstrapCleanupFixture(t)
	first, second := fixture.pending(), fixture.pending()
	cause := errors.New("original operation")
	resultErr := errors.Join(cause, fmt.Errorf("nested: %w", &InFlightError{Pending: first}), &InFlightError{Pending: second}, &InFlightError{Pending: first})
	finishPrivateBootstrap(&fixture.lock, &resultErr)
	if !errors.As(resultErr, &fixture.owner) || fixture.owner == nil || fixture.lock != nil {
		t.Fatalf("actual lock cleanup owner is unreachable: %v", resultErr)
	}
	if !errors.Is(resultErr, cause) || len(pendingHandles(resultErr)) != 2 {
		t.Fatalf("original pending graph was changed: %v", resultErr)
	}
	if !errors.Is(fixture.owner.Close(), ErrInFlight) {
		t.Fatal("cleanup Close released or waited for unfinished work")
	}
	if !fixture.owner.CleanupPending() {
		t.Fatal("unfinished native custody was hidden")
	}
	fixture.assertLocked(t)
	transport.CompletePending(first, CommandResult{})
	fixture.assertLocked(t)
	select {
	case <-fixture.owner.Done():
		t.Fatal("cleanup completed before every command")
	default:
	}
	lateErr := errors.New("late command failure")
	transport.CompletePending(second, CommandResult{Err: lateErr})
	awaitBootstrapCleanup(t, fixture.owner)
	if fixture.owner.CleanupPending() || !errors.Is(fixture.owner.Err(), lateErr) || !errors.Is(resultErr, lateErr) {
		t.Fatalf("late result error is unreachable: %v", resultErr)
	}
	fixture.assertReleased(t)
}

func TestBootstrapCleanupPreservesPureInFlightProjection(t *testing.T) {
	fixture := newBootstrapCleanupFixture(t)
	pending := fixture.pending()
	resultErr := error(&InFlightError{Pending: pending})
	finishPrivateBootstrap(&fixture.lock, &resultErr)
	if !errors.As(resultErr, &fixture.owner) {
		t.Fatal("cleanup owner missing")
	}
	isInFlight := func(err error) bool { return errors.Is(err, ErrInFlight) }
	if !task.AllErrorCauses(resultErr, isInFlight) {
		t.Fatalf("clean owner changed pending classification: %v", resultErr)
	}
	transport.CompletePending(pending, CommandResult{})
	awaitBootstrapCleanup(t, fixture.owner)
	if fixture.owner.Err() != nil || !task.AllErrorCauses(resultErr, isInFlight) {
		t.Fatalf("successful cleanup invented a fault: %v", resultErr)
	}
	fixture.assertReleased(t)
}

func TestBootstrapCleanupLateNestedPendingRetainsNativeLock(t *testing.T) {
	fixture := newBootstrapCleanupFixture(t)
	first, child := fixture.pending(), fixture.pending()
	resultErr := error(&InFlightError{Pending: first})
	finishPrivateBootstrap(&fixture.lock, &resultErr)
	if !errors.As(resultErr, &fixture.owner) {
		t.Fatal("cleanup owner missing")
	}
	transport.CompletePending(first, CommandResult{Err: errors.Join(&InFlightError{Pending: child}, &InFlightError{Pending: first})})
	fixture.assertLocked(t)
	childErr := errors.New("nested completion fault")
	transport.CompletePending(child, CommandResult{Err: childErr})
	awaitBootstrapCleanup(t, fixture.owner)
	if !errors.Is(resultErr, childErr) {
		t.Fatalf("late nested result was lost: %v", resultErr)
	}
	fixture.assertReleased(t)
}

func TestBootstrapCleanupSettledResultRetainsUnfinishedChild(t *testing.T) {
	fixture := newBootstrapCleanupFixture(t)
	first, child := fixture.pending(), fixture.pending()
	transport.CompletePending(first, CommandResult{Err: &InFlightError{Pending: child}})
	resultErr := error(&InFlightError{Pending: first})
	finishPrivateBootstrap(&fixture.lock, &resultErr)
	if !errors.As(resultErr, &fixture.owner) {
		t.Fatal("settled parent lost unfinished child cleanup")
	}
	fixture.assertLocked(t)
	transport.CompletePending(child, CommandResult{})
	awaitBootstrapCleanup(t, fixture.owner)
	fixture.assertReleased(t)
}

func TestBootstrapCleanupReleaseFaultIsOwnedAndReported(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "closed-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	lock := &bootstrapLock{file: file}
	pending := transport.NewPending(nil)
	reported := make(chan error, 1)
	owner := retainBootstrapLock(&lock, []*Pending{pending}, func(err error) { reported <- err })
	transport.CompletePending(pending, CommandResult{})
	awaitBootstrapCleanup(t, owner)
	if !errors.Is(owner.Err(), os.ErrClosed) || !owner.CleanupPending() {
		t.Fatalf("failed native close lost its actual cleanup owner: %v", owner.Err())
	}
	select {
	case err := <-reported:
		if !errors.Is(err, os.ErrClosed) {
			t.Fatalf("legacy report lost native close fault: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("legacy release report was dropped")
	}
	if !errors.Is(owner.Close(), os.ErrClosed) {
		t.Fatal("retry discarded unresolved close fault")
	}
}

func TestBootstrapCleanupRepeatedCloseKeepsHistoryAfterNativeRelease(t *testing.T) {
	fixture := newBootstrapCleanupFixture(t)
	pending := fixture.pending()
	resultErr := error(&InFlightError{Pending: pending})
	finishPrivateBootstrap(&fixture.lock, &resultErr)
	if !errors.As(resultErr, &fixture.owner) {
		t.Fatal("cleanup owner missing")
	}
	commandErr := errors.New("completed command failed")
	transport.CompletePending(pending, CommandResult{Err: commandErr})
	awaitBootstrapCleanup(t, fixture.owner)
	for range 2 {
		if !errors.Is(fixture.owner.Close(), commandErr) || fixture.owner.CleanupPending() {
			t.Fatalf("historical fault confused native custody: %v", fixture.owner.Err())
		}
		if !errors.Is(fixture.owner.Err(), commandErr) || !errors.Is(resultErr, commandErr) {
			t.Fatal("cleanup retry erased historical command failure")
		}
		fixture.assertReleased(t)
	}
}
