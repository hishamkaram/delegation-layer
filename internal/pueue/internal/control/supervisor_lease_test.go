package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
)

func TestWithSupervisorLeaseCallbacksOverlap(t *testing.T) {
	stateRoot := supervisorLeaseTestRoot(t)
	first := supervisorLeaseTestHold(t, stateRoot, WithSupervisorLease)
	// Hold waits for callback entry before returning. The second callback must
	// enter while the first is still waiting on its release channel.
	second := supervisorLeaseTestHold(t, stateRoot, WithSupervisorLease)
	first.finish(t)
	second.finish(t)
}

func TestWithSupervisorLeaseBlocksTransitionUntilBothCallbacksReturn(t *testing.T) {
	stateRoot := supervisorLeaseTestRoot(t)
	first := supervisorLeaseTestHold(t, stateRoot, WithSupervisorLease)
	second := supervisorLeaseTestHold(t, stateRoot, WithSupervisorLease)

	supervisorLeaseTestBlocked(t, stateRoot, WithBootstrapLock)
	first.finish(t)
	supervisorLeaseTestBlocked(t, stateRoot, WithBootstrapLock)
	second.finish(t)
	supervisorLeaseTestEnters(t, stateRoot, WithBootstrapLock)
}

func TestWithSupervisorLeaseWaitsForExclusiveTransition(t *testing.T) {
	stateRoot := supervisorLeaseTestRoot(t)
	transition := supervisorLeaseTestHold(t, stateRoot, WithBootstrapLock)
	supervisorLeaseTestBlocked(t, stateRoot, WithSupervisorLease)
	transition.finish(t)
	supervisorLeaseTestEnters(t, stateRoot, WithSupervisorLease)
}

func TestTryWithPrivateTransitionDoesNotQueueFollowers(t *testing.T) {
	stateRoot := filepath.Join(canonicalTemp(t), "state")
	owner := supervisorLeaseTestHold(t, stateRoot, withPrivateTransitionForTest)
	called := false
	acquired, err := TryWithPrivateTransition(t.Context(), stateRoot, func() error {
		called = true
		return nil
	})
	if err != nil || acquired || called {
		t.Fatalf("busy nonblocking transition = acquired=%t called=%t err=%v, want false false nil", acquired, called, err)
	}
	owner.finish(t)
	acquired, err = TryWithPrivateTransition(t.Context(), stateRoot, func() error {
		called = true
		return nil
	})
	if err != nil || !acquired || !called {
		t.Fatalf("available nonblocking transition = acquired=%t called=%t err=%v, want true true nil", acquired, called, err)
	}
}

func withPrivateTransitionForTest(ctx context.Context, stateRoot string, callback func() error) error {
	acquired, err := TryWithPrivateTransition(ctx, stateRoot, callback)
	if err != nil || acquired {
		return err
	}
	return errSupervisorLockBusy
}

func TestWithSupervisorLeaseRetainsLockUntilPendingCompletion(t *testing.T) {
	stateRoot := supervisorLeaseTestRoot(t)
	pending := transport.NewPending(nil)
	complete := sync.OnceFunc(func() { transport.CompletePending(pending, CommandResult{}) })
	t.Cleanup(func() {
		complete()
		// Join the retained lock's eventual release even after a failed assertion.
		// The test context is canceled before cleanup callbacks run.
		if err := WithBootstrapLock(context.WithoutCancel(t.Context()), stateRoot, func() error { return nil }); err != nil {
			t.Errorf("release retained supervisor lease: %v", err)
		}
	})
	observationErr := errors.New("caller stopped observing")
	err := WithSupervisorLease(t.Context(), stateRoot, func() error {
		return errors.Join(observationErr, &InFlightError{Pending: pending})
	})
	if !errors.Is(err, observationErr) || !errors.Is(err, ErrInFlight) || pendingFrom(err) != pending {
		t.Fatalf("lease lost its pending command: %v", err)
	}
	select {
	case <-pending.Done():
		t.Fatal("returning from the lease completed its pending command")
	default:
	}

	// Retention keeps a shared lease, so independent controls remain possible.
	supervisorLeaseTestEnters(t, stateRoot, WithSupervisorLease)
	supervisorLeaseTestBlocked(t, stateRoot, WithBootstrapLock)
	complete()
	supervisorLeaseTestEnters(t, stateRoot, WithBootstrapLock)
}

func TestWithSupervisorLeaseDoesNotProvisionMissingState(t *testing.T) {
	stateRoot := filepath.Join(canonicalTemp(t), "state")
	err := WithSupervisorLease(t.Context(), stateRoot, func() error {
		t.Fatal("shared lease entered without a published lifecycle boundary")
		return nil
	})
	if !errors.Is(err, ErrPrivateSupervisorNotInitialized) {
		t.Fatalf("shared lease error = %v, want uninitialized supervisor", err)
	}
	if _, statErr := os.Stat(stateRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("shared lease provisioned missing state root: %v", statErr)
	}
}

func TestRecoverPrivateHealthyGenerationSharesSupervisorLease(t *testing.T) {
	fixture, original := supervisorLeaseTestBind(t)
	saved := original.Binding()
	lease := supervisorLeaseTestHold(t, fixture.stateRoot, WithSupervisorLease)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	recovered, err := RecoverPrivate(ctx, fixture.stateRoot, saved, fixture.clientPath, fixture.daemonPath, Options{ObservationTimeout: 10 * time.Second})
	supervisorLeaseTestSettleClient(t, recovered, err)
	if err != nil {
		t.Fatalf("healthy saved generation required exclusive recovery: %v", err)
	}
	if recovered.Binding() != saved {
		t.Fatalf("healthy recovery changed saved generation: got=%+v want=%+v", recovered.Binding(), saved)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	lease.finish(t)
}

func TestRecoverPrivateRestartWaitsForSupervisorLease(t *testing.T) {
	fixture, original := supervisorLeaseTestBind(t)
	saved := original.Binding()
	lease := supervisorLeaseTestHold(t, fixture.stateRoot, WithSupervisorLease)
	readyPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready")
	removePrivateTestPath(t, readyPath)
	removePrivateTestPID(t, fixture.stateRoot)

	faultContext, cancelFault := context.WithTimeout(t.Context(), 50*time.Millisecond)
	recovered, err := RecoverPrivate(faultContext, fixture.stateRoot, saved, fixture.clientPath, fixture.daemonPath, Options{ObservationTimeout: 10 * time.Second})
	cancelFault()
	supervisorLeaseTestSettleClient(t, recovered, err)
	if recovered != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stopped generation bypassed exclusive recovery: client=%v err=%v", recovered, err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	assertPrivateTestPathMissing(t, readyPath)
	assertPrivateTestPathMissing(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.pid"))

	lease.finish(t)
	recoveryContext, cancelRecovery := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelRecovery()
	recovered, err = RecoverPrivate(recoveryContext, fixture.stateRoot, saved, fixture.clientPath, fixture.daemonPath, Options{ObservationTimeout: 10 * time.Second})
	supervisorLeaseTestSettleClient(t, recovered, err)
	if err != nil {
		t.Fatalf("stopped generation did not recover after lease release: %v", err)
	}
	if recovered.Binding() != saved {
		t.Fatalf("restart changed saved generation: got=%+v want=%+v", recovered.Binding(), saved)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}

func supervisorLeaseTestBind(t *testing.T) (privateSupervisorTestFixture, *Client) {
	t.Helper()
	fixture := newPrivateSupervisorTestFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, err := BindPrivate(ctx, fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: 10 * time.Second})
	supervisorLeaseTestSettleClient(t, client, err)
	if err != nil {
		t.Fatalf("bind supervisor fixture before lease fault: %v", err)
	}
	return fixture, client
}

func supervisorLeaseTestRoot(t *testing.T) string {
	t.Helper()
	stateRoot := filepath.Join(canonicalTemp(t), "state")
	if err := WithBootstrapLock(t.Context(), stateRoot, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	return stateRoot
}

func supervisorLeaseTestSettleClient(t *testing.T, client *Client, err error) {
	t.Helper()
	// The fault call keeps its short deadline. Only fixture teardown waits for
	// any commands still owned by the returned error before releasing files.
	for _, pending := range pendingHandles(err) {
		waitPending(t, pending)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("close supervisor fixture client: %v", closeErr)
		}
	})
}

type supervisorLeaseTestCallback struct {
	entered chan struct{}
	done    chan struct{}
	release func()
	err     error
}

func supervisorLeaseTestHold(t *testing.T, stateRoot string, acquire func(context.Context, string, func() error) error) *supervisorLeaseTestCallback {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	release := make(chan struct{})
	call := &supervisorLeaseTestCallback{
		entered: make(chan struct{}),
		done:    make(chan struct{}),
		release: sync.OnceFunc(func() { close(release) }),
	}
	t.Cleanup(func() {
		call.release()
		cancel()
		<-call.done
		if call.err != nil && !errors.Is(call.err, context.Canceled) {
			t.Errorf("held supervisor callback failed: %v", call.err)
		}
	})
	go func() {
		call.err = acquire(ctx, stateRoot, func() error {
			close(call.entered)
			<-release
			return nil
		})
		close(call.done)
	}()
	select {
	case <-call.entered:
	case <-call.done:
		t.Fatalf("supervisor lock returned before callback entry: %v", call.err)
	case <-ctx.Done():
		t.Fatalf("supervisor callback did not enter within its setup bound: %v", ctx.Err())
	}
	return call
}

func (call *supervisorLeaseTestCallback) finish(t *testing.T) {
	t.Helper()
	call.release()
	<-call.done
	if call.err != nil {
		t.Fatalf("supervisor callback failed: %v", call.err)
	}
}

func supervisorLeaseTestBlocked(t *testing.T, stateRoot string, acquire func(context.Context, string, func() error) error) {
	t.Helper()
	// A deadline is used only to observe deliberate lock contention. Callback
	// entry and release in the successful paths are coordinated by channels.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	entered := false
	err := acquire(ctx, stateRoot, func() error {
		entered = true
		return nil
	})
	if entered || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("conflicting supervisor lock entered=%v error=%v, want deadline without entry", entered, err)
	}
}

func supervisorLeaseTestEnters(t *testing.T, stateRoot string, acquire func(context.Context, string, func() error) error) {
	t.Helper()
	entered := false
	err := acquire(t.Context(), stateRoot, func() error {
		entered = true
		return nil
	})
	if err != nil || !entered {
		t.Fatalf("available supervisor lock entered=%v error=%v", entered, err)
	}
}
