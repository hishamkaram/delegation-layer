package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRecoverPrivateDoesNotRestartDispatchedPredecessor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	shutdownMarker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "shutdown-marker")
	options := privateMigrationTestOptions(fixture.stateRoot, 10*time.Second)
	options.Environment = append(os.Environ(), "PRIVATE_SHUTDOWN_MARKER="+shutdownMarker)
	legacy := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	nextPath := filepath.Join(filepath.Dir(fixture.daemonPath), "proof-successor")
	writeExecutable(t, nextPath, privateDaemonFixture+"\n# successor\n")
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, nextPath, options)
	t.Cleanup(func() {
		if err := current.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
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
	if err = legacy.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPath(t, shutdownMarker)

	if _, err = RecoverPrivate(context.Background(), fixture.stateRoot, legacy.Binding(), fixture.clientPath, fixture.daemonPath, options); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("dispatched predecessor was recovered: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	assertPrivateTestPathMissing(t, shutdownMarker)
	stopped, found, err := loadPrivateMigrationRecord(base)
	if err != nil || !found || !stopped.LegacyStopped {
		t.Fatalf("recovery did not record predecessor absence: record=%+v found=%t error=%v", stopped, found, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), PrivateControlTimeout(options))
	defer cancel()
	if err = WithBootstrapLock(ctx, fixture.stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot)
	}); err != nil {
		t.Fatalf("successor was not launched after predecessor absence: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, fixture.stateRoot, nextPath)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateTestPathMissing(t, shutdownMarker)
}

func TestPendingPrivateMigrationResumesPublishedSuccessor(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(fmt.Sprintf("later-installation=%v", upgrade), func(t *testing.T) {
			testResumePublishedPrivateSuccessor(t, upgrade)
		})
	}
}

func testResumePublishedPrivateSuccessor(t *testing.T, upgrade bool) {
	t.Helper()
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
	if err = legacy.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = legacy.WaitUnavailableWithPID(context.Background(), saved.LegacyPID); err != nil {
		t.Fatal(err)
	}
	stopped := saved
	stopped.LegacyStopped = true
	if err = replacePrivateMigrationRecord(base, saved, stopped); err != nil {
		t.Fatal(err)
	}
	if err = current.RestartPrivate(context.Background(), fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	if upgrade {
		latestPath := filepath.Join(filepath.Dir(fixture.daemonPath), "latest-successor")
		writeExecutable(t, latestPath, privateDaemonFixture+"\n# latest successor\n")
		current, err = BindPrivate(context.Background(), fixture.clientPath, latestPath, fixture.stateRoot, options)
		if err != nil {
			t.Fatal(err)
		}
		nextPath = latestPath
	}
	ctx, cancel := context.WithTimeout(context.Background(), PrivateControlTimeout(options))
	defer cancel()
	err = WithBootstrapLock(ctx, fixture.stateRoot, func() error { return current.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot) })
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, fixture.stateRoot, nextPath)
	if upgrade {
		assertRecordedPrivateSuccessorCompleted(t, base, stopped)
	}
	if _, found, err := loadPrivateMigrationRecord(base); err != nil || found {
		t.Fatalf("journal not cleared: found=%v error=%v", found, err)
	}
}

func assertRecordedPrivateSuccessorCompleted(t *testing.T, base string, stopped privateMigrationRecord) {
	t.Helper()
	history, found, err := loadPrivateMigrationHistory(base)
	if err != nil || !found || len(history.Migrations) != 2 {
		t.Fatalf("migration history: %+v found=%v err=%v", history, found, err)
	}
	if history.Migrations[0] != stopped {
		t.Fatal("recorded successor was retargeted after shutdown")
	}
}

func TestPrivateMaintenanceRetainsPendingCommandLock(t *testing.T) {
	for _, command := range []string{"status", "remove"} {
		t.Run(command, func(t *testing.T) {
			fixture := newPrivateSupervisorTestFixture(t)
			client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
			row := statusTestJob(0, "finished", statusStreamTestDoneState("Success"))
			data := statusTestPayload(t, map[string]any{"0": row}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
			if err := os.WriteFile(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-status.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			base := canonicalTemp(t)
			started, release := filepath.Join(base, "started"), filepath.Join(base, "release")
			defer releasePrivateStatusOnCleanup(t, release)
			client.options.ObservationTimeout = time.Second
			prefix := "PRIVATE_" + strings.ToUpper(command)
			client.options.Environment = append(os.Environ(), prefix+"_STARTED="+started, prefix+"_RELEASE="+release)
			client.options.SuccessfulCleanupAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) (PrivateQueueCleanupPlan, error) {
				return PrivateQueueCleanupPlan{RemoveIDs: []int64{0}}, nil
			}
			cancel, err := configurePrivateTestParallelismAndMaintain(context.Background(), client)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			waitForPrivateFixtureMarker(t, started)
			supervisorLeaseTestBlocked(t, fixture.stateRoot, WithBootstrapLock)
			releasePrivateStatus(t, release)
			supervisorLeaseTestEnters(t, fixture.stateRoot, WithBootstrapLock)
		})
	}
}

func configurePrivateTestParallelismAndMaintain(ctx context.Context, client *Client) (context.CancelFunc, error) {
	controlContext, cancel, err := client.privateControlContext(ctx)
	if err != nil {
		return func() {}, err
	}
	err = client.withPrivateSupervisorLease(controlContext, func(context.Context) error {
		return client.setPrivateDefaultParallelTasksLocked(controlContext, 0)
	})
	if err != nil {
		cancel()
		return func() {}, err
	}
	client.MaintainPrivateQueue(controlContext)
	return cancel, nil
}
