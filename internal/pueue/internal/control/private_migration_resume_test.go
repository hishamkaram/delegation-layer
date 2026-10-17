package control

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPendingPrivateMigrationResumesPersistedPID(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := privateMigrationTestOptions(fixture.stateRoot, 10*time.Second)
	legacy := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	nextPath := filepath.Join(filepath.Dir(fixture.daemonPath), "proof-successor")
	writeExecutable(t, nextPath, privateDaemonFixture+"\n# successor\n")
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, nextPath, options)
	var err error
	base := filepath.Dir(current.Binding().ConfigPath)
	pending := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(legacy.Binding()), Successor: privateDaemonIdentityForBinding(current.Binding())}
	if err = publishPrivateMigrationRecord(base, pending.Legacy, pending.Successor); err != nil {
		t.Fatal(err)
	}
	saved := pending
	saved.LegacyPID, _, err = legacy.privateDaemonPID()
	if err != nil {
		t.Fatal(err)
	}
	if err = replacePrivateMigrationRecord(base, pending, saved); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), PrivateControlTimeout(options))
	defer cancel()
	err = WithBootstrapLock(ctx, fixture.stateRoot, func() error { return current.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot) })
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, fixture.stateRoot, nextPath)
	if _, found, err := loadPrivateMigrationRecord(base); err != nil || found {
		t.Fatalf("journal not cleared: found=%v error=%v", found, err)
	}
}

func TestPendingPrivateMigrationAfterDispatchedShutdownWaitsForRecordedPID(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	shutdownMarker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "shutdown-marker")
	options := privateMigrationTestOptions(fixture.stateRoot, 10*time.Second)
	options.Environment = append(os.Environ(), "PRIVATE_SHUTDOWN_MARKER="+shutdownMarker)
	legacy := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	nextPath := filepath.Join(filepath.Dir(fixture.daemonPath), "proof-successor")
	writeExecutable(t, nextPath, privateDaemonFixture+"\n# successor\n")
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, nextPath, options)
	base := filepath.Dir(current.Binding().ConfigPath)
	pending := privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(legacy.Binding()),
		Successor:     privateDaemonIdentityForBinding(current.Binding()),
	}
	if err := publishPrivateMigrationRecord(base, pending.Legacy, pending.Successor); err != nil {
		t.Fatal(err)
	}
	dispatched := pending
	var err error
	process, present, err := legacy.privateDaemonProcessIdentity()
	if err != nil || !present {
		t.Fatal(err)
	}
	dispatched.LegacyPID = process.PID
	dispatched.LegacyProcessStart = process.StartToken
	dispatched.LegacyShutdownRequested = true
	dispatched.LegacyShutdownDispatched = true
	if err = replacePrivateMigrationRecord(base, pending, dispatched); err != nil {
		t.Fatal(err)
	}
	if recorded, found, loadErr := loadPrivateMigrationRecord(base); loadErr != nil || !found || !recorded.LegacyShutdownDispatched {
		t.Fatalf("shutdown phase was not durable before dispatch: record=%+v found=%t err=%v", recorded, found, loadErr)
	}
	if err = legacy.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPath(t, shutdownMarker)

	ctx, cancel := context.WithTimeout(context.Background(), PrivateControlTimeout(options))
	defer cancel()
	err = WithBootstrapLock(ctx, fixture.stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot)
	})
	if err != nil {
		t.Fatalf("resume after dispatched shutdown failed: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, fixture.stateRoot, nextPath)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateTestPathMissing(t, shutdownMarker)
	if _, found, err := loadPrivateMigrationRecord(base); err != nil || found {
		t.Fatalf("journal not cleared after dispatched shutdown: found=%v error=%v", found, err)
	}
}

func TestPrivateMigrationRetriesKnownPreStartShutdownFailure(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	var removed atomic.Bool
	var shutdownStarts atomic.Int32
	options := privateMigrationRetryOptions(t, fixture, &removed, &shutdownStarts)
	legacy, current, nextPath := setupPrivateMigrationRetry(t, fixture, options)
	t.Cleanup(func() {
		cleanupPrivateMigrationRetry(t, current)
	})

	if err := runPrivateMigrationRetryAttempt(t, fixture.stateRoot, current, options); err == nil {
		t.Fatal("migration succeeded after a pre-Start shutdown failure")
	}
	assertPrivateMigrationRetryPending(t, fixture.stateRoot, &shutdownStarts)
	restorePrivateMigrationRetryPredecessor(t, fixture, legacy)

	if err := runPrivateMigrationRetryAttempt(t, fixture.stateRoot, current, options); err != nil {
		t.Fatalf("retryable shutdown request did not recover: %v", err)
	}
	assertPrivateMigrationRetryCompleted(t, fixture.stateRoot, nextPath, &shutdownStarts)
}

func privateMigrationRetryOptions(t *testing.T, fixture privateSupervisorTestFixture, removed *atomic.Bool, shutdownStarts *atomic.Int32) Options {
	t.Helper()
	options := privateMigrationTestOptions(fixture.stateRoot, privateTestObservationTimeout)
	options.Observer = privateMigrationRetryObserver(t, fixture, removed, shutdownStarts)
	return options
}

func privateMigrationRetryObserver(t *testing.T, fixture privateSupervisorTestFixture, removed *atomic.Bool, shutdownStarts *atomic.Int32) func(CommandEvent) {
	t.Helper()
	return func(event CommandEvent) {
		observePrivateMigrationRetryEvent(t, fixture, removed, shutdownStarts, event)
	}
}

func observePrivateMigrationRetryEvent(t *testing.T, fixture privateSupervisorTestFixture, removed *atomic.Bool, shutdownStarts *atomic.Int32, event CommandEvent) {
	t.Helper()
	if len(event.Argv) == 0 || event.Argv[len(event.Argv)-1] != "shutdown" {
		return
	}
	if event.Stage == "entry" && removed.CompareAndSwap(false, true) {
		if err := os.Remove(fixture.clientPath); err != nil {
			t.Errorf("remove client before shutdown Start: %v", err)
		}
	}
	if event.Stage == "started" {
		shutdownStarts.Add(1)
	}
}

func setupPrivateMigrationRetry(t *testing.T, fixture privateSupervisorTestFixture, options Options) (*Client, *Client, string) {
	t.Helper()
	legacy := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	nextPath := filepath.Join(filepath.Dir(fixture.daemonPath), "retry-successor")
	writeExecutable(t, nextPath, privateDaemonFixture+"\n# retry successor\n")
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, nextPath, options)
	return legacy, current, nextPath
}

func cleanupPrivateMigrationRetry(t *testing.T, current *Client) {
	t.Helper()
	if err := current.Shutdown(context.Background()); err != nil {
		t.Logf("cleanup successor: %v", err)
	}
}

func runPrivateMigrationRetryAttempt(t *testing.T, stateRoot string, current *Client, options Options) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), PrivateControlTimeout(options))
	err := WithBootstrapLock(ctx, stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(ctx, stateRoot)
	})
	cancel()
	return err
}

func assertPrivateMigrationRetryPending(t *testing.T, stateRoot string, shutdownStarts *atomic.Int32) {
	t.Helper()
	pending, found, loadErr := loadPrivateMigrationRecord(filepath.Join(stateRoot, privateSupervisorDirectory))
	if loadErr != nil || !found {
		t.Fatalf("retryable shutdown request was not retained: record=%+v found=%t err=%v", pending, found, loadErr)
	}
	if !pending.LegacyShutdownRequested || pending.LegacyShutdownDispatched || pending.LegacyStopped || pending.LegacyProcessStart == "" {
		t.Fatalf("pre-Start failure advanced the shutdown phase: %+v", pending)
	}
	if got := shutdownStarts.Load(); got != 0 {
		t.Fatalf("failed shutdown reported %d successful starts, want zero", got)
	}
}

func restorePrivateMigrationRetryPredecessor(t *testing.T, fixture privateSupervisorTestFixture, legacy *Client) {
	t.Helper()
	writeExecutable(t, fixture.clientPath, privateClientFixture)
	reachable, reachableErr := legacy.Reachable(context.Background())
	if reachableErr != nil || !reachable {
		t.Fatalf("predecessor was not preserved after pre-Start failure: reachable=%t err=%v", reachable, reachableErr)
	}
}

func assertPrivateMigrationRetryCompleted(t *testing.T, stateRoot, nextPath string, shutdownStarts *atomic.Int32) {
	t.Helper()
	if got := shutdownStarts.Load(); got != 1 {
		t.Fatalf("recovery started %d shutdown commands, want one", got)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, nextPath)
	if _, found, err := loadPrivateMigrationRecord(filepath.Join(stateRoot, privateSupervisorDirectory)); err != nil || found {
		t.Fatalf("completed retry left migration journal: found=%t err=%v", found, err)
	}
}
