package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPrivateCleanupSharesStableGenerationLease(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = allowSuccessfulCleanupForTest
	held := supervisorLeaseTestHold(t, fixture.stateRoot, WithSupervisorLease)
	startPrivateMaintenancePass(t, client, func() {}).requireCompleted(t)
	assertCleanupTestRemoval(t, fixture.stateRoot, true)
	held.finish(t)
}

func TestPrivateCleanupWaitsForQueueIdentity(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = allowSuccessfulCleanupForTest
	scope := newTestCallScope(t, client.options)
	released, release := scope.barrier()
	entered := make(chan struct{})
	lease := startQueueIdentityLease(scope, client, func(context.Context) error {
		close(entered)
		<-released
		return nil
	})
	awaitEntry(scope, entered, lease, "hold queue identity")
	pass := startPrivateMaintenancePass(t, client, release)
	select {
	case <-pass.done:
		t.Fatalf("cleanup crossed an active queue identity lease: %v", pass.err)
	case <-time.After(100 * time.Millisecond):
	}
	assertCleanupTestRemoval(t, fixture.stateRoot, false)
	release()
	awaitLockCall(scope, lease, "release queue identity")
	pass.requireCompleted(t)
	assertCleanupTestRemoval(t, fixture.stateRoot, true)
}

func TestPrivateCleanupDefersToExclusiveGenerationTransition(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	client.options.SuccessfulCleanupAuthorizer = allowSuccessfulCleanupForTest
	held := supervisorLeaseTestHold(t, fixture.stateRoot, WithBootstrapLock)
	var queries atomic.Int32
	client.options.Observer = func(event CommandEvent) {
		if event.Stage == "entry" {
			queries.Add(1)
		}
	}
	// The pass wins its maintenance election; the held generation lease still prevents observation.
	electionSkipped, err := startPrivateMaintenancePass(t, client, func() {}).result(t)
	if electionSkipped || err != nil || queries.Load() != 0 {
		t.Fatalf("cleanup crossed a generation transition: election-skipped=%t queries=%d err=%v", electionSkipped, queries.Load(), err)
	}
	assertCleanupTestRemoval(t, fixture.stateRoot, false)
	held.finish(t)
	startPrivateMaintenancePass(t, client, func() {}).requireCompleted(t)
	assertCleanupTestRemoval(t, fixture.stateRoot, true)
}

func TestPrivateCleanupPendingQueryRetainsLease(t *testing.T) {
	scenario := newPrivateTerminalTriggerScenario(t, "cleanup-query", true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pass := startPrivateMaintenancePassContext(t, ctx, scenario.client, func() { scenario.releaseStatus(t) })
	waitForPrivateFixtureMarker(t, scenario.startedPath)
	cancel()
	_, err := pass.result(t)
	pending := pendingHandles(err)
	if len(pending) == 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup query lost pending ownership: %v", err)
	}
	supervisorLeaseTestBlocked(t, scenario.fixture.stateRoot, WithBootstrapLock)
	assertCleanupTestRemoval(t, scenario.fixture.stateRoot, false)
	scenario.releaseStatus(t)
	for _, handle := range pending {
		waitPending(t, handle)
	}
	if closeErr := scenario.client.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	supervisorLeaseTestEnters(t, scenario.fixture.stateRoot, WithBootstrapLock)
	if scenario.statusCommands.Load() != 1 || scenario.removeCommands.Load() != 0 {
		t.Fatalf("pending cleanup commands: status=%d remove=%d", scenario.statusCommands.Load(), scenario.removeCommands.Load())
	}
}

func TestPrivateCleanupRejectsUnsafeGenerationBeforeObservation(t *testing.T) {
	for _, kind := range []string{"unprotected lock", "unreadable binding"} {
		t.Run(kind, func(t *testing.T) {
			fixture, client := newMaintenanceProbeFixture(t)
			client.options.SuccessfulCleanupAuthorizer = allowSuccessfulCleanupForTest
			path := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "bootstrap.lock")
			want := ErrConfiguration
			if kind == "unprotected lock" {
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				path = privateSupervisorBindingPath(fixture.stateRoot)
				writePrivateTestFile(t, path, "{")
				want = ErrBinding
			}
			var queries atomic.Int32
			client.options.Observer = func(event CommandEvent) {
				if event.Stage == "entry" {
					queries.Add(1)
				}
			}
			_, err := startPrivateMaintenancePass(t, client, func() {}).result(t)
			if !errors.Is(err, want) || queries.Load() != 0 {
				t.Fatalf("unsafe generation reached cleanup: queries=%d err=%v, want %v", queries.Load(), err, want)
			}
			assertCleanupTestRemoval(t, fixture.stateRoot, false)
			assertCleanupGenerationFixtureUnchanged(t, path, kind)
		})
	}
}

func assertCleanupGenerationFixtureUnchanged(t *testing.T, path, kind string) {
	t.Helper()
	if kind == "unprotected lock" {
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("refusal changed unsafe lock: %v", statErr)
		}
	} else if got := string(readPrivateSupervisorFile(t, path)); got != "{" {
		t.Fatalf("refusal changed published binding: %q", got)
	}
}

// Generation refusal must remain an error even when the queue is empty.
func TestPrivateCleanupRejectsUnreadableGenerationWithNoRows(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	writeCleanupTestRows(t, fixture.stateRoot, nil)
	client.options.SuccessfulCleanupAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) (PrivateQueueCleanupPlan, error) {
		t.Error("unreadable generation reached authorization")
		return PrivateQueueCleanupPlan{}, ErrCleanupAuthorization
	}
	writePrivateTestFile(t, privateSupervisorBindingPath(fixture.stateRoot), "{")
	_, err := startPrivateMaintenancePass(t, client, func() {}).result(t)
	if !errors.Is(err, ErrBinding) {
		t.Fatalf("empty queue hid generation refusal: %v", err)
	}
	assertCleanupTestRemoval(t, fixture.stateRoot, false)
}
