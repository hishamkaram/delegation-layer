package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
	"golang.org/x/sys/unix"
)

func newRevocableShutdownFixture(t *testing.T) (privateSupervisorTestFixture, *Client) {
	t.Helper()
	fixture := newPrivateSupervisorTestFixture(t)
	config := shutdownScopeConfigForTest(t, fixture.stateRoot)
	var client *Client
	var bindErr error
	t.Cleanup(func() {
		if closeErr := client.CloseAfterPending(bindErr); closeErr != nil {
			t.Error(closeErr)
		}
	})
	client, bindErr = Bind(t.Context(), fixture.clientPath, config, Options{WaitForPendingClose: true})
	if bindErr != nil {
		t.Fatal(bindErr)
	}
	client.options.ControlOwnerExecutable = os.Args[0]
	return fixture, client
}

// This setup guard may commit exactly the one canonical config in this
// owned fixture root. It grants no daemon Start or native placement proof.
func shutdownScopeConfigForTest(t *testing.T, root string) string {
	t.Helper()
	resolution, err := CurrentResolutionContext()
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	calls := 0
	guard := generationGuardFunc(func(got context.Context, gotRoot string, commit func() error) error {
		if got != ctx || gotRoot != root {
			return ErrConfiguration
		}
		calls++
		return allowPrivateGenerationForTest{}.GuardPrivateGenerationCommit(got, gotRoot, commit)
	})
	config, _, err := ensurePrivateConfigContext(ctx, root, resolution, guard)
	if err != nil || calls != 1 {
		t.Fatalf("owned config setup: calls=%d err=%v", calls, err)
	}
	return config
}

type retainedShutdownScope struct {
	context context.Context
	files   []*os.File
}

// The transport Pending retains the original real bootstrap descriptor after
// callback return. Its completion and every duplicated native FD are owned by
// this fixture; reacquiring EX joins their native lock release before removal.
func retainedShutdownContext(t *testing.T, fixture privateSupervisorTestFixture, client *Client, beforeReturn func(context.Context)) retainedShutdownScope {
	t.Helper()
	pending := transport.NewPending(nil)
	complete := sync.OnceFunc(func() { transport.CompletePending(pending, CommandResult{}) })
	var retained retainedShutdownScope
	var original *bootstrapLock
	t.Cleanup(func() {
		closeErr := closeControlLeaseFiles(retained.files)
		complete()
		ctx, cancel := context.WithTimeout(context.Background(), controlOwnerTestStartupTimeout)
		defer cancel()
		lock, acquireErr := acquireExistingSupervisorLock(ctx, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "bootstrap.lock"), unix.LOCK_EX)
		if releaseErr := errors.Join(closeErr, acquireErr, closeBootstrapLock(lock)); releaseErr != nil {
			t.Error(releaseErr)
		}
		if original != nil {
			original.mu.Lock()
			closed := original.closed
			original.mu.Unlock()
			if !closed {
				t.Error("retained bootstrap owner did not close its native descriptor")
			}
		}
	})
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(locked context.Context) error {
		scoped, scopeErr := client.privateShutdownContext(locked)
		if scopeErr != nil {
			return scopeErr
		}
		retained.context = scoped
		bootstrap, tagged := locked.Value(privateBootstrapEXKey{}).(*privateBootstrapEX)
		if !tagged || bootstrap == nil {
			return ErrBinding
		}
		original = bootstrap.lock
		duplicates, duplicateErr := duplicateControlLeaseFiles(scoped)
		retained.files = duplicates
		if duplicateErr != nil {
			return duplicateErr
		}
		if beforeReturn != nil {
			beforeReturn(scoped)
		}
		return &InFlightError{Pending: pending}
	})
	if pendingFrom(err) != pending {
		t.Fatalf("fixture lost its actual retained bootstrap owner: %v", err)
	}
	probe, probeErr := acquireExistingSupervisorLock(t.Context(), filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "bootstrap.lock"), unix.LOCK_EX|unix.LOCK_NB)
	if !errors.Is(probeErr, errSupervisorLockBusy) {
		t.Fatal(errors.Join(errors.New("fixture bootstrap FD is not still locked"), probeErr, closeBootstrapLock(probe)))
	}
	return retained
}

func TestPrivateBootstrapScopeRevokesRetainedTag(t *testing.T) {
	for _, derived := range []string{"saved", "with cancellation", "without cancellation"} {
		t.Run(derived, func(t *testing.T) {
			fixture, client := newRevocableShutdownFixture(t)
			retained := retainedShutdownContext(t, fixture, client, nil)
			saved := retained.context
			if derived == "with cancellation" {
				var cancel context.CancelFunc
				saved, cancel = context.WithCancel(saved)
				t.Cleanup(cancel)
			}
			if derived == "without cancellation" {
				saved = context.WithoutCancel(saved)
			}
			if saved.Err() != nil {
				t.Fatalf("revocation must not cancel the legacy bootstrap context: %v", saved.Err())
			}
			if _, err := client.privateShutdownContext(saved); !errors.Is(err, ErrBinding) {
				t.Errorf("saved bootstrap tag admitted managed shutdown after callback: %v", err)
			}
			if _, err := privateShutdownForwarding(saved, retained.files); !errors.Is(err, ErrBinding) {
				t.Errorf("saved bootstrap tag forwarded retained FD after callback: %v", err)
			}
		})
	}
}

func shutdownScopeMarker(t *testing.T, client *Client) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "shutdown-entered")
	client.options.Environment = append(os.Environ(), "PRIVATE_SHUTDOWN_MARKER="+marker)
	return marker
}

func requireShutdownMarkerAbsent(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("revoked admission entered the finite shutdown CLI: %v", err)
	}
}

func TestManagedShutdownRejectsRevokedRetainedBootstrap(t *testing.T) {
	fixture, client := newRevocableShutdownFixture(t)
	marker := shutdownScopeMarker(t, client)
	retained := retainedShutdownContext(t, fixture, client, nil)
	dispatched, err := client.ShutdownDispatched(context.WithoutCancel(retained.context))
	closeErr := client.CloseAfterPending(err)
	if !errors.Is(err, ErrBinding) || dispatched || closeErr != nil {
		t.Fatalf("revoked managed shutdown: dispatched=%t err=%v close=%v", dispatched, err, closeErr)
	}
	requireShutdownMarkerAbsent(t, marker)
}

func TestPrivateBootstrapScopeSurvivesNestedLockAndCancellation(t *testing.T) {
	fixture, client := newRevocableShutdownFixture(t)
	marker := shutdownScopeMarker(t, client)
	innerPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "shutdown-inner.lock")
	if err := os.WriteFile(innerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := WithBootstrapLockContext(context.Background(), fixture.stateRoot, func(locked context.Context) error {
		if innerErr := withPublishedPrivateLockContext(locked, fixture.stateRoot, "shutdown-inner.lock", unix.LOCK_SH, func(context.Context) error { return nil }); innerErr != nil {
			return innerErr
		}
		canceled, cancel := context.WithCancel(locked)
		cancel()
		// Existing recovery cleanup removes observation cancellation while
		// staying INSIDE the actual bootstrap callback, without a new grant.
		dispatched, shutdownErr := client.ShutdownDispatched(context.WithoutCancel(canceled))
		if !dispatched && shutdownErr == nil {
			return errors.New("active managed shutdown did not dispatch")
		}
		return shutdownErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, markerErr := os.Stat(marker); markerErr != nil {
		t.Fatalf("active bootstrap did not reach its finite command: %v", markerErr)
	}
}

func TestPrivateBootstrapZeroScopeCannotConsumeAdmission(t *testing.T) {
	var zero privateBootstrapEX
	called := false
	err := zero.consume(func() error { called = true; return nil })
	if !errors.Is(err, ErrBinding) || called {
		t.Fatalf("zero scope granted admission: err=%v predicate=%t", err, called)
	}
}

func TestPrivateBootstrapConsumePreservesPrivateAdmissionFault(t *testing.T) {
	fixture, client := newRevocableShutdownFixture(t)
	cause := errors.New("capture-owned admission refused")
	calls := 0
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(locked context.Context) error {
		scoped, scopeErr := client.privateShutdownContext(locked)
		if scopeErr != nil {
			return scopeErr
		}
		consume := privateShutdownConsume(scoped, func() error { calls++; return cause })
		if consume == nil {
			return errors.New("managed scope has no final consume")
		}
		return consume()
	})
	if !errors.Is(err, cause) || calls != 1 {
		t.Fatalf("private admission lost its fault: calls=%d err=%v", calls, err)
	}
}

func TestManagedShutdownFinalConsumeRejectsRevocationAfterDup(t *testing.T) {
	fixture, client := newRevocableShutdownFixture(t)
	marker := shutdownScopeMarker(t, client)
	interposer := &admissionReleaseDuringDup{original: consumeTestDecoy(t), armed: true}
	entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	var result CommandResult
	var commandErr error
	retainedShutdownContext(t, fixture, client, func(scoped context.Context) {
		consume := privateShutdownConsume(scoped, nil)
		if consume == nil {
			t.Fatal("managed scope has no final consume")
		}
		started := false
		t.Cleanup(func() {
			release() // Always release the fixture before joining command work.
			if started {
				<-done
				if closeErr := client.CloseAfterPending(commandErr); closeErr != nil {
					t.Error(closeErr)
				}
			}
		})
		started = true
		go func() {
			result, commandErr = client.command(WithControlLease(scoped, interposer), func() error {
				// Canonical descriptor/forwarding preparation has finished. This
				// gate is OUTSIDE ScopeMu, just before its production consume.
				close(entered)
				<-resume
				return consume()
			}, "shutdown")
			close(done)
		}()
		select {
		case <-entered:
			if len(interposer.duplicates) != 1 {
				t.Fatal("final consume preceded native FD duplication")
			}
		case <-done:
			t.Fatalf("command never reached final consume: %v", commandErr)
		}
	})
	// Actual callback end revoked the tag while its original FD remains held.
	release()
	<-done
	if closeErr := client.CloseAfterPending(commandErr); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(commandErr, ErrBinding) || result.Started {
		t.Errorf("post-dup revocation admitted command: %+v err=%v", result, commandErr)
	}
	requireShutdownMarkerAbsent(t, marker)
	assertConsumeDuplicatesClosed(t, interposer.duplicates)
}

func TestLegacyUnscopedShutdownKeepsExistingBehavior(t *testing.T) {
	_, canonical := newRevocableShutdownFixture(t)
	legacy, err := NewClient(canonical.Binding(), canonical.options)
	var commandErr error
	t.Cleanup(func() {
		if closeErr := legacy.CloseAfterPending(errors.Join(err, commandErr)); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	marker := shutdownScopeMarker(t, legacy)
	dispatched, shutdownErr := legacy.ShutdownDispatched(t.Context())
	commandErr = shutdownErr
	if shutdownErr != nil || !dispatched {
		t.Fatalf("existing unscoped legacy shutdown changed: dispatched=%t err=%v", dispatched, shutdownErr)
	}
	if _, markerErr := os.Stat(marker); markerErr != nil {
		t.Fatal(markerErr)
	}
}

func TestStopRecoveredWithinBootstrapPreservesCleanupAuthority(t *testing.T) {
	fixture, client := newRevocableShutdownFixture(t)
	marker := shutdownScopeMarker(t, client)
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(locked context.Context) error {
		canceled, cancel := context.WithCancel(locked)
		cancel()
		return client.StopRecovered(canceled, privateTestObservationTimeout)
	})
	if err != nil {
		t.Fatalf("existing in-callback recovered cleanup lost authority: %v", err)
	}
	if _, markerErr := os.Stat(marker); markerErr != nil {
		t.Fatalf("recovered cleanup did not reach its finite command: %v", markerErr)
	}
}
