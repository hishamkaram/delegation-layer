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

func TestPrivateStatusFixtureRetainsRootUntilPositiveJoin(t *testing.T) {
	pending := transport.NewPending(nil, nil)
	complete := sync.OnceFunc(func() { transport.CompletePending(pending, CommandResult{}) })
	var fixture *privateStatusFixture
	var lock *bootstrapLock
	var owner *BootstrapCleanupError
	var cleanupErr error
	// The outer test retains the exact handles if an assertion or counterexample
	// leaves the inner fixture held, and only removes its root after positive join.
	t.Cleanup(func() {
		complete()
		if owner != nil && !waitUntil(owner.Done(), time.Now().Add(10*time.Second)) {
			t.Error("bootstrap fixture cleanup did not join")
			return
		}
		if closeErr := closeBootstrapLock(lock); closeErr != nil {
			t.Error(closeErr)
			return
		}
		if fixture == nil {
			return
		}
		if err := fixture.finish(time.Now().Add(10 * time.Second)); err != nil {
			t.Error(err)
			return
		}
		if err := os.RemoveAll(fixture.base); err != nil {
			t.Error(err)
		}
	})
	t.Run("expired cleanup", func(t *testing.T) {
		fixture = &privateStatusFixture{retained: true}
		fixture.base = canonicalTempWithRetention(t, &fixture.retained)
		fixture.releasePath = filepath.Join(fixture.base, "status-release")
		t.Cleanup(func() { cleanupErr = fixture.finish(time.Now().Add(-time.Second)) })
		var err error
		lock, err = acquireBootstrapLock(context.Background(), filepath.Join(fixture.base, "bootstrap.lock"))
		if err != nil {
			t.Fatal(err)
		}
		resultErr := error(&InFlightError{Pending: pending})
		finishPrivateBootstrap(&lock, &resultErr)
		fixture.capture(nil, resultErr)
		if !errors.As(resultErr, &owner) {
			t.Fatal("bootstrap cleanup owner was not retained")
		}
	})
	if !errors.Is(cleanupErr, context.DeadlineExceeded) {
		t.Fatalf("held fixture cleanup = %v, want an unproven join", cleanupErr)
	}
	assertPrivateStatusFixtureHeld(t, fixture, owner)
	complete()
	if err := fixture.finish(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if fixture.retained || owner.CleanupPending() {
		t.Fatal("completed fixture did not permit directory cleanup")
	}
}

func assertPrivateStatusFixtureHeld(t *testing.T, fixture *privateStatusFixture, owner *BootstrapCleanupError) {
	t.Helper()
	if !fixture.retained || !owner.CleanupPending() {
		t.Fatal("unfinished command or bootstrap custody was discarded")
	}
	if _, err := os.Stat(fixture.releasePath); err != nil {
		t.Fatalf("release marker disappeared before positive join: %v", err)
	}
	if _, err := os.Stat(fixture.base); err != nil {
		t.Fatalf("unfinished fixture root was removed: %v", err)
	}
}

func TestPrivateStatusFixtureRemovesRootAfterJoinedCommandFailure(t *testing.T) {
	pending := transport.NewPending(nil, nil)
	commandErr := errors.New("released status exited 1")
	complete := sync.OnceFunc(func() { transport.CompletePending(pending, CommandResult{Err: commandErr, ExitCode: 1}) })
	var fixture *privateStatusFixture
	var owner *BootstrapCleanupError
	t.Cleanup(func() {
		complete()
		if owner != nil && !waitUntil(owner.Done(), time.Now().Add(10*time.Second)) {
			t.Error("bootstrap fixture cleanup did not join")
		}
	})
	t.Run("joined command failure", func(t *testing.T) {
		fixture = newPrivateStatusFixture(t)
		lock, err := acquireBootstrapLock(context.Background(), filepath.Join(fixture.base, "bootstrap.lock"))
		if err != nil {
			t.Fatal(err)
		}
		resultErr := error(&InFlightError{Pending: pending})
		finishPrivateBootstrap(&lock, &resultErr)
		fixture.capture(nil, resultErr)
		complete()
		if !errors.As(resultErr, &owner) {
			t.Fatal("bootstrap cleanup owner was not retained")
		}
	})
	if !errors.Is(fixture.lateDiagnostics, commandErr) {
		t.Fatalf("late command diagnostics = %v, want original command error", fixture.lateDiagnostics)
	}
	if fixture.retained || owner.CleanupPending() {
		t.Fatal("joined command failure was confused with native closure")
	}
	if _, err := os.Stat(fixture.base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("positively joined fixture root remains: %v", err)
	}
}

func TestPrivateStatusFixtureRetainsAllPathsUntilDaemonWaitJoins(t *testing.T) {
	done := make(chan struct{})
	complete := sync.OnceFunc(func() { close(done) })
	var fixture *privateStatusFixture
	var supervisor privateSupervisorTestFixture
	var cleanupErr error
	t.Cleanup(func() {
		complete()
		if fixture == nil {
			return
		}
		if err := fixture.finish(time.Now().Add(10 * time.Second)); err != nil {
			t.Error(err)
			return
		}
		if err := os.RemoveAll(fixture.base); err != nil {
			t.Error(err)
		}
	})
	t.Run("unfinished daemon wait", func(t *testing.T) {
		fixture = &privateStatusFixture{retained: true}
		fixture.base = canonicalTempWithRetention(t, &fixture.retained)
		fixture.releasePath = filepath.Join(fixture.base, "status-release")
		supervisor = newPrivateSupervisorTestFixtureAtBase(t, fixture.base)
		if err := fixture.RegisterDaemonWait(supervisor.stateRoot, 0, done); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cleanupErr = fixture.finish(time.Now().Add(-time.Second)) })
	})
	if !errors.Is(cleanupErr, context.DeadlineExceeded) || !fixture.retained {
		t.Fatalf("unfinished daemon cleanup=%v retained=%t", cleanupErr, fixture.retained)
	}
	for _, path := range []string{fixture.releasePath, filepath.Join(supervisor.stateRoot, "root.json"), supervisor.clientPath, supervisor.daemonPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("owned fixture path disappeared before daemon Wait joined: %s: %v", path, err)
		}
	}
	complete()
	if err := fixture.finish(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if fixture.retained {
		t.Fatal("joined daemon Wait did not permit directory cleanup")
	}
}
