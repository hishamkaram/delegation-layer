package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRecoverPrivateRestoresMissingLogicalBindingForStoppedSupervisor(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{ObservationTimeout: privateTestObservationTimeout}
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	saved := first.Binding()
	if err = os.Remove(privateSupervisorBindingPath(stateRoot)); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, stateRoot)

	recovered, err := RecoverPrivate(context.Background(), stateRoot, saved, clientPath, daemonPath, options)
	if err != nil {
		t.Fatalf("stopped supervisor with missing logical binding was not recoverable: %v", err)
	}
	if recovered.Binding() != saved {
		t.Fatalf("recovery changed the saved supervisor binding: saved=%+v recovered=%+v", saved, recovered.Binding())
	}
	persisted, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !task.SameSupervisorIdentity(persisted, saved) {
		t.Fatalf("saved logical binding was not restored: found=%t binding=%+v want=%+v", found, persisted, saved)
	}
}

func TestRecoverSavedStartsAndGracefullyStopsRecordedLegacySupervisor(t *testing.T) {
	fake := newFakeSupervisor(t, "normal")
	readyPath := filepath.Join(filepath.Dir(fake.configPath), "legacy-ready")
	shutdownPath := filepath.Join(filepath.Dir(fake.configPath), "legacy-shutdown")
	fake.environment = append(fake.environment, "FAKE_READY="+readyPath, "FAKE_SHUTDOWN_MARKER="+shutdownPath)
	fake.client.options.Environment = fake.environment
	daemonPath := filepath.Join(filepath.Dir(fake.configPath), "pueued")
	writeExecutable(t, daemonPath, `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c) config=$2; shift 2 ;;
    *) shift ;;
  esac
done
touch "$(dirname "$config")/legacy-ready"
`)
	digest, err := hashExecutable(daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	saved := fake.client.Binding()
	saved.DaemonExecutable = daemonPath
	saved.DaemonSHA256 = digest
	client, started, err := RecoverSaved(context.Background(), saved, Options{ObservationTimeout: privateTestObservationTimeout, Environment: fake.environment})
	if err != nil {
		t.Fatalf("legacy recovery failed: %v", err)
	}
	if !started || client == nil {
		t.Fatalf("legacy recovery did not report its started daemon: client=%v started=%t", client != nil, started)
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatalf("legacy shutdown failed: %v", err)
	}
	if err := client.WaitUnavailable(context.Background()); err != nil {
		t.Fatalf("legacy endpoint remained available after shutdown: %v", err)
	}
}

func TestRecoverSavedRejectsPartialDaemonIdentity(t *testing.T) {
	fake := newFakeSupervisor(t, "normal")
	saved := fake.client.Binding()
	saved.DaemonExecutable = fake.executable
	if _, _, err := RecoverSaved(context.Background(), saved, Options{Environment: fake.environment}); !errors.Is(err, ErrBinding) {
		t.Fatalf("partial daemon identity was accepted: %v", err)
	}
}

func TestRecoverSavedStopsAlreadyReachableLegacySupervisor(t *testing.T) {
	fake := newFakeSupervisor(t, "normal")
	shutdownPath := filepath.Join(filepath.Dir(fake.configPath), "legacy-shutdown")
	fake.environment = append(fake.environment, "FAKE_SHUTDOWN_MARKER="+shutdownPath)
	saved := fake.client.Binding()
	client, started, err := RecoverSaved(context.Background(), saved, Options{ObservationTimeout: privateTestObservationTimeout, Environment: fake.environment})
	if err != nil {
		t.Fatalf("reachable legacy recovery failed: %v", err)
	}
	if started {
		t.Fatal("reachable legacy supervisor was started a second time")
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatalf("reachable legacy shutdown failed: %v", err)
	}
	if err := client.WaitUnavailable(context.Background()); err != nil {
		t.Fatalf("reachable legacy endpoint remained available after shutdown: %v", err)
	}
}

func TestRecoverPrivateUsesCurrentPairAfterSavedInstallRemoved(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldInstall := filepath.Join(base, "old")
	currentInstall := filepath.Join(base, "current")
	oldClientPath := filepath.Join(oldInstall, "pueue")
	oldDaemonPath := filepath.Join(oldInstall, "pueued")
	currentClientPath := filepath.Join(currentInstall, "pueue")
	currentDaemonPath := filepath.Join(currentInstall, "pueued")
	makePrivateTestDirectory(t, oldInstall)
	makePrivateTestDirectory(t, currentInstall)
	writeExecutable(t, oldClientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentClientPath, privateClientFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture)

	first := bindPrivateForTest(t, stateRoot, oldClientPath, oldDaemonPath)
	identityBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	daemonIdentityPath := privateDaemonIdentityPath(identityBase)
	supervisorIdentityPath := privateSupervisorBindingPath(stateRoot)
	oldDaemonIdentity := readPrivateSupervisorFile(t, daemonIdentityPath)
	oldSupervisorIdentity := readPrivateSupervisorFile(t, supervisorIdentityPath)
	appendPrivateSupervisorFile(t, currentDaemonPath, "\n# upgraded daemon pair\n")
	removePrivateTestPath(t, oldInstall)
	assertPrivateTestPathMissing(t, first.Binding().ClientExecutable)

	recovered := recoverPrivateForTest(t, stateRoot, first.Binding(), currentClientPath, currentDaemonPath)
	if recovered.Binding() != first.Binding() {
		t.Fatalf("recovery changed the durable task supervisor identity: first=%+v recovered=%+v", first.Binding(), recovered.Binding())
	}
	if recovered.runtimeBinding.ClientExecutable != currentClientPath || recovered.runtimeBinding.DaemonExecutable != currentDaemonPath {
		t.Fatalf("recovered client did not use the current executable pair: %+v", recovered.runtimeBinding)
	}
	if err := recovered.Ready(context.Background()); err != nil {
		t.Fatalf("recovered client could not use the current pair to inspect the live endpoint: %v", err)
	}
	if identityAfterAdoption := readPrivateSupervisorFile(t, daemonIdentityPath); !bytes.Equal(identityAfterAdoption, oldDaemonIdentity) {
		t.Fatal("recovery rewrote daemon launch provenance while adopting the existing endpoint")
	}
	if identityAfterAdoption := readPrivateSupervisorFile(t, supervisorIdentityPath); !bytes.Equal(identityAfterAdoption, oldSupervisorIdentity) {
		t.Fatal("recovery rewrote the completed supervisor generation while adopting the existing endpoint")
	}

	removePrivateTestPath(t, filepath.Join(stateRoot, privateSupervisorDirectory, "ready"))
	if _, err := RecoverPrivate(context.Background(), stateRoot, first.Binding(), currentClientPath, currentDaemonPath, Options{ObservationTimeout: DefaultObservationTimeout}); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("stopped active generation accepted a replacement pair: %v", err)
	}
	if identityAfterRefusal := readPrivateSupervisorFile(t, daemonIdentityPath); !bytes.Equal(identityAfterRefusal, oldDaemonIdentity) {
		t.Fatal("refused recovery changed daemon launch provenance")
	}
	if identityAfterRefusal := readPrivateSupervisorFile(t, supervisorIdentityPath); !bytes.Equal(identityAfterRefusal, oldSupervisorIdentity) {
		t.Fatal("refused recovery changed the completed supervisor generation")
	}
	assertPrivateDaemonStartCount(t, stateRoot, 1)
}

func TestRecoveredPrivateClientCanStopTaskWithOriginalSupervisorReference(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldClientPath, oldDaemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "old"))
	currentClientPath, currentDaemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "current"))
	privateRoot := filepath.Join(stateRoot, privateSupervisorDirectory)
	statusPath := filepath.Join(privateRoot, "test-status.json")
	killMarker := filepath.Join(privateRoot, "test-kill-task-id")
	// This test verifies recovery preserves the durable stop authority; it does
	// not exercise a short observation timeout. Use the production default so
	// a full -race run cannot turn scheduler contention into a false failure.
	options := Options{ObservationTimeout: DefaultObservationTimeout}
	first, err := BindPrivate(context.Background(), oldClientPath, oldDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(privateRoot, "test-record-kill"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := newTaskFixture(t, first.Binding())
	submission := createSubmission(t, fixture, first.Binding())
	writePrivateRunningTaskStatus(t, statusPath, submission, 7)
	observation, err := first.Reconcile(context.Background(), identityFromRecord(submission))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.taskDir.RecordSupervisorReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
	permit := createStopPermit(t, fixture)
	defer func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()

	removePrivateTestPath(t, filepath.Join(base, "old"))
	recovered := recoverPrivateForTest(t, stateRoot, first.Binding(), currentClientPath, currentDaemonPath)
	if recovered.Binding() != first.Binding() {
		t.Fatalf("recovery changed the queued task's supervisor reference: first=%+v recovered=%+v", first.Binding(), recovered.Binding())
	}
	result, err := recovered.Stop(context.Background(), permit)
	if err != nil {
		t.Fatalf("recovered private client could not stop the task: %+v %v", result, err)
	}
	if !result.Matched || !result.Attempted || result.Action != "kill" || result.Acknowledged == nil || !*result.Acknowledged {
		t.Fatalf("recovered private client did not complete the authorized stop: %+v", result)
	}
	if got := string(readPrivateSupervisorFile(t, killMarker)); got != "7\n" {
		t.Fatalf("recovered client stopped task %q, want task 7", got)
	}
}

func TestRecoverPrivateBoundsHungBootstrapAndRetainsLock(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, stateRoot)

	startedMarker := filepath.Join(base, "status-started")
	releaseMarker := filepath.Join(base, "status-release")
	options := Options{
		// Leave enough time for RecoverPrivate's version preflight to finish
		// before the deliberately blocked status command consumes the budget.
		ObservationTimeout: time.Second,
		Environment: append(
			os.Environ(),
			"PRIVATE_STATUS_STARTED="+startedMarker,
			"PRIVATE_STATUS_RELEASE="+releaseMarker,
		),
	}
	defer releasePrivateStatusOnCleanup(t, releaseMarker)
	started := time.Now()
	_, err = RecoverPrivate(context.Background(), stateRoot, first.Binding(), clientPath, daemonPath, options)
	if err == nil {
		t.Fatal("hung private bootstrap unexpectedly recovered")
	}
	maxBootstrapDuration := time.Duration(privateBootstrapObservationPhases)*options.ObservationTimeout + 500*time.Millisecond
	if elapsed := time.Since(started); elapsed > maxBootstrapDuration {
		t.Fatalf("private recovery exceeded its bounded budget: elapsed=%s limit=%s", elapsed, maxBootstrapDuration)
	}
	var inFlight *InFlightError
	if !errors.As(err, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("private recovery lost its pending ownership: %v", err)
	}
	if !strings.Contains("\x00"+strings.Join(inFlight.Pending.Args(), "\x00")+"\x00", "\x00status\x00") {
		t.Fatalf("private recovery timed out before the blocked status probe: %q", inFlight.Pending.Args())
	}
	waitForPrivateFixtureMarker(t, startedMarker)

	lockContext, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	deferredLock, lockErr := acquireBootstrapLock(lockContext, filepath.Join(stateRoot, privateSupervisorDirectory, "bootstrap.lock"))
	cancel()
	if deferredLock != nil {
		if closeErr := closeBootstrapLock(deferredLock); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if !errors.Is(lockErr, context.DeadlineExceeded) {
		t.Fatalf("bootstrap lock was released while readiness was still running: %v", lockErr)
	}
	releasePrivateStatus(t, releaseMarker)
	if !awaitPendingNaturally(inFlight.Pending, 10*time.Second) {
		t.Fatal("hung readiness process remained in flight")
	}
	lock, lockErr := acquireBootstrapLock(context.Background(), filepath.Join(stateRoot, privateSupervisorDirectory, "bootstrap.lock"))
	if lockErr != nil {
		t.Fatal(lockErr)
	}
	if err = closeBootstrapLock(lock); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPrivateRestartsStoppedSupervisorAfterExecutableDrift(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	first := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	wantDaemonIdentity := readPrivateDaemonIdentity(t, fixture.stateRoot)
	wantSupervisorIdentity := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot))
	removePrivateTestPath(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready"))
	appendPrivateSupervisorFile(t, fixture.daemonPath, "\n")
	if _, err := RecoverPrivate(context.Background(), fixture.stateRoot, first.Binding(), fixture.clientPath, fixture.daemonPath, Options{ObservationTimeout: DefaultObservationTimeout}); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("stopped active generation accepted executable drift: %v", err)
	}

	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	if identity := readPrivateDaemonIdentity(t, fixture.stateRoot); identity != wantDaemonIdentity {
		t.Fatalf("rejected recovery changed daemon launch provenance: got=%+v want=%+v", identity, wantDaemonIdentity)
	}
	if identity := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot)); !bytes.Equal(identity, wantSupervisorIdentity) {
		t.Fatal("rejected recovery changed the completed supervisor generation")
	}
}

func TestRestartPrivateRepinsIdentityAfterGracefulShutdown(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")

	if _, err := BindPrivate(context.Background(), clientPath, oldDaemonPath, stateRoot, Options{ObservationTimeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	legacyIdentity := readPrivateDaemonIdentity(t, stateRoot)
	current, err := BindPrivate(context.Background(), clientPath, currentDaemonPath, stateRoot, Options{ObservationTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got := readPrivateDaemonIdentity(t, stateRoot); got != legacyIdentity {
		t.Fatalf("reachable upgrade rewrote identity before migration: got=%+v legacy=%+v", got, legacyIdentity)
	}
	if err := current.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := current.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := current.RestartPrivate(context.Background(), stateRoot); err != nil {
		t.Fatal(err)
	}
	want := privateDaemonIdentityForBinding(current.Binding())
	if got := readPrivateDaemonIdentity(t, stateRoot); got != want {
		t.Fatalf("migration did not atomically repin current identity: got=%+v want=%+v", got, want)
	}
}

func TestPrivateDaemonIdentityDriftIsReadOnly(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	makePrivateTestDirectory(t, privateBase)
	seedPrivateDaemonIdentity(t, privateBase, oldDaemonPath)

	drift, err := PrivateDaemonIdentityDrift(stateRoot, currentDaemonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !drift {
		t.Fatal("changed daemon launch provenance was not detected")
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, oldDaemonPath)

	drift, err = PrivateDaemonIdentityDrift(stateRoot, oldDaemonPath)
	if err != nil {
		t.Fatal(err)
	}
	if drift {
		t.Fatal("matching daemon launch provenance was reported as drift")
	}
}

func TestIsPrivateConfigRejectsExistingUnownedSupervisorConfig(t *testing.T) {
	stateRoot := filepath.Join(canonicalTemp(t), "state")
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := os.MkdirAll(privateBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PrivateConfigPath(stateRoot), []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsPrivateConfig(stateRoot, PrivateConfigPath(stateRoot)) {
		t.Fatal("existing supervisor config without a Delegate state root was treated as private")
	}
}

func TestPrivateSupervisorLogicalDriftIsDetected(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if drift, err := PrivateSupervisorLogicalDrift(fixture.stateRoot, client.Binding()); err != nil || drift {
		t.Fatalf("matching private supervisor was reported as logically drifted: drift=%v err=%v", drift, err)
	}
	drifted := client.Binding()
	drifted.ConfigDigest = strings.Repeat("a", 64)
	if drift, err := PrivateSupervisorLogicalDrift(fixture.stateRoot, drifted); err != nil || !drift {
		t.Fatalf("logical private supervisor drift was not detected: drift=%v err=%v", drift, err)
	}
}

func TestRefreshPrivateSupervisorBindingUsesPublishedIdentity(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	saved, err := BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	currentDaemon := filepath.Join(canonicalTemp(t), "current-pueued")
	writeExecutable(t, currentDaemon, privateDaemonFixture+"\n# current\n")
	seedPrivateDaemonIdentity(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory), currentDaemon)

	refreshed, err := RefreshPrivateSupervisorBinding(fixture.stateRoot, saved.Binding())
	if err != nil {
		t.Fatal(err)
	}
	if !task.SameSupervisorIdentity(refreshed, saved.Binding()) {
		t.Fatalf("refresh changed logical supervisor identity: got=%+v want=%+v", refreshed, saved.Binding())
	}
	if refreshed.DaemonExecutable != canonicalPrivateTestExecutable(t, currentDaemon) {
		t.Fatalf("refresh kept stale daemon executable: %q", refreshed.DaemonExecutable)
	}
}

func TestMigratePrivateSupervisorWithLockRepinsDriftedIdentity(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	if _, err := BindPrivate(context.Background(), clientPath, oldDaemonPath, stateRoot, options); err != nil {
		t.Fatal(err)
	}
	legacyIdentity := readPrivateDaemonIdentity(t, stateRoot)
	current, err := BindPrivate(context.Background(), clientPath, currentDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if got := readPrivateDaemonIdentity(t, stateRoot); got != legacyIdentity {
		t.Fatalf("reachable upgrade rewrote identity before migration: got=%+v legacy=%+v", got, legacyIdentity)
	}
	if err := WithBootstrapLock(context.Background(), stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	}); err != nil {
		t.Fatal(err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, currentDaemonPath)
	if _, err := os.Stat(privateRecoveryRecordPath(stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed private migration retained recovery record: %v", err)
	}
	assertPrivateDaemonStartCount(t, stateRoot, 2)
}

func TestPreparePrivateForMigrationDoesNotProbeChangedEndpoint(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	bindPrivateSetupForTest(t, stateRoot, clientPath, daemonPath, Options{ObservationTimeout: privateTestObservationTimeout})
	statusStarted := filepath.Join(base, "status-started")
	releaseStatus := filepath.Join(base, "release-status")
	environment := append(
		os.Environ(),
		"PRIVATE_STATUS_STARTED="+statusStarted,
		"PRIVATE_STATUS_RELEASE="+releaseStatus,
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := WithBootstrapLock(ctx, stateRoot, func() error {
		current, err := PreparePrivateForMigrationWithLock(ctx, clientPath, daemonPath, stateRoot, Options{
			ObservationTimeout: privateTestObservationTimeout,
			Environment:        environment,
		})
		if err != nil {
			return err
		}
		return current.Close()
	}); err != nil {
		t.Fatalf("migration preparation probed an unavailable successor endpoint: %v", err)
	}
	if _, err := os.Stat(statusStarted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("migration preparation launched a successor status probe: %v", err)
	}
}
