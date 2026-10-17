package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestVerifyPrivateMigrationEndpointFailsClosedWithoutLogicalPredecessor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := Options{ObservationTimeout: privateTestObservationTimeout}
	bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	if err := os.Remove(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, fixture.stateRoot)
	if err := os.Remove(privateSupervisorBindingPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	currentDaemon := filepath.Join(filepath.Dir(fixture.stateRoot), "current-pueued")
	writeExecutable(t, currentDaemon, privateDaemonFixture+"\n# current bundle\n")

	var current *Client
	err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		var prepareErr error
		current, prepareErr = PreparePrivateForMigrationWithLock(context.Background(), fixture.clientPath, currentDaemon, fixture.stateRoot, options)
		if prepareErr != nil {
			return prepareErr
		}
		defer func() {
			if closeErr := current.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
		return current.VerifyPrivateMigrationEndpoint(context.Background(), fixture.stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unreachable endpoint with persisted identity was accepted: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestVerifyPrivateMigrationEndpointAllowsStoppedMatchingSupervisor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := Options{ObservationTimeout: privateTestObservationTimeout}
	bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	if err := os.Remove(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, fixture.stateRoot)

	var current *Client
	err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		var prepareErr error
		current, prepareErr = PreparePrivateForMigrationWithLock(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
		if prepareErr != nil {
			return prepareErr
		}
		defer func() {
			if closeErr := current.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
		if err := current.VerifyPrivateMigrationEndpoint(context.Background(), fixture.stateRoot); err != nil {
			return err
		}
		return current.StartPrivate(context.Background(), fixture.stateRoot)
	})
	if err != nil {
		t.Fatalf("stopped supervisor with matching identity was not restartable: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}

func TestVerifyPrivateMigrationEndpointFailsClosedWithoutIdentity(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := Options{ObservationTimeout: privateTestObservationTimeout}
	if _, err := BindPrivate(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, fixture.stateRoot)
	if err := os.Remove(privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); err != nil {
		t.Fatal(err)
	}

	var current *Client
	err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		var prepareErr error
		current, prepareErr = PreparePrivateForMigrationWithLock(context.Background(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
		if prepareErr != nil {
			return prepareErr
		}
		defer func() {
			if closeErr := current.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
		return current.VerifyPrivateMigrationEndpoint(context.Background(), fixture.stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unreachable endpoint without persisted identity was accepted: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestPendingPrivateMigrationJournalResumesAfterShutdown(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	first, err := BindPrivate(context.Background(), clientPath, oldDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	current, err := BindPrivate(context.Background(), clientPath, currentDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	pending := privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(first.Binding()),
		Successor:     privateDaemonIdentityForBinding(current.Binding()),
	}
	if err := current.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := current.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateMigrationRecord(filepath.Dir(current.Binding().ConfigPath), pending.Legacy, pending.Successor); err != nil {
		t.Fatal(err)
	}

	if err := WithBootstrapLock(context.Background(), stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	}); err != nil {
		t.Fatal(err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, currentDaemonPath)
	if _, err := os.Stat(privateMigrationRecordPath(filepath.Dir(current.Binding().ConfigPath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed pending migration journal remained: %v", err)
	}
	assertPrivateDaemonStartCount(t, stateRoot, 3)
}

func TestPendingPrivateMigrationCompletesAfterSuccessorLaunchBeforeIdentityPublication(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	legacyDaemonPath := filepath.Join(base, "legacy-pueued")
	successorDaemonPath := filepath.Join(base, "successor-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, legacyDaemonPath, privateDaemonFixture)
	writeExecutable(t, successorDaemonPath, privateDaemonFixture+"\n# successor\n")
	options := privateMigrationTestOptions(stateRoot, DefaultObservationTimeout)
	legacy, err := BindPrivate(context.Background(), clientPath, legacyDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := BindPrivate(context.Background(), clientPath, successorDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	pid, present, err := legacy.privateDaemonPID()
	if err != nil || !present {
		t.Fatalf("predecessor PID: %v, present=%v", err, present)
	}
	if err = legacy.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = legacy.WaitUnavailableWithPID(context.Background(), pid); err != nil {
		t.Fatal(err)
	}
	pending := privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(legacy.Binding()),
		Successor:     privateDaemonIdentityForBinding(successor.Binding()),
		LegacyPID:     pid,
		LegacyStopped: true,
	}
	data, err := marshalPrivateMigrationRecord(pending)
	if err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err = publishPrivateRecord(basePath, privateMigrationRecordPath(basePath), data); err != nil {
		t.Fatal(err)
	}

	if err = startDaemon(successor.runtimeBinding.DaemonExecutable, successor.runtimeBinding.DaemonSHA256, successor.runtimeBinding.ConfigPath, basePath, options); err != nil {
		t.Fatal(err)
	}
	if err = waitReady(context.Background(), successor, options.ObservationTimeout); err != nil {
		t.Fatal(err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, legacyDaemonPath)

	if err = WithBootstrapLock(context.Background(), stateRoot, func() error {
		return successor.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	}); err != nil {
		t.Fatalf("successor launch was not reconciled from the migration phase: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, successorDaemonPath)
	assertPrivateDaemonStartCount(t, stateRoot, 2)
	if _, err = os.Stat(privateMigrationRecordPath(basePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed successor launch migration journal remained: %v", err)
	}
}

func TestPendingPrivateMigrationRetargetsAfterSecondInstallationUpgrade(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	legacyDaemonPath := filepath.Join(base, "legacy-pueued")
	firstDaemonPath := filepath.Join(base, "first-pueued")
	secondDaemonPath := filepath.Join(base, "second-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, legacyDaemonPath, privateDaemonFixture)
	writeExecutable(t, firstDaemonPath, privateDaemonFixture+"\n# first installation\n")
	writeExecutable(t, secondDaemonPath, privateDaemonFixture+"\n# second installation\n")
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	legacy, err := BindPrivate(context.Background(), clientPath, legacyDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	first, err := BindPrivate(context.Background(), clientPath, firstDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	legacyIdentity := privateDaemonIdentityForBinding(legacy.Binding())
	firstIdentity := privateDaemonIdentityForBinding(first.Binding())
	if err = publishPrivateMigrationRecord(filepath.Dir(first.Binding().ConfigPath), legacyIdentity, firstIdentity); err != nil {
		t.Fatal(err)
	}

	second, err := BindPrivate(context.Background(), clientPath, secondDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := WithBootstrapLock(context.Background(), stateRoot, func() error {
		return second.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	}); err != nil {
		t.Fatalf("pending migration did not retarget to the second installation: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, stateRoot, secondDaemonPath)
	if _, err := os.Stat(privateMigrationRecordPath(filepath.Dir(second.Binding().ConfigPath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retargeted migration journal remained: %v", err)
	}
}

func TestPrivateSupervisorGenerationMatchesPublishedMigrationHistory(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	current := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	base := current.Binding()
	first := base
	first.ConfigDigest = task.ComputeSHA256([]byte("first-config"))
	first.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("first-resolved"))
	first.Endpoint = "unix:" + filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "first.sock")
	second := first
	second.ConfigDigest = task.ComputeSHA256([]byte("second-config"))
	second.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("second-resolved"))
	second.Endpoint = "unix:" + filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "second.sock")
	third := second
	third.ConfigDigest = task.ComputeSHA256([]byte("third-config"))
	third.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("third-resolved"))
	third.Endpoint = "unix:" + filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "third.sock")
	basePath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	for _, migration := range []privateMigrationRecord{
		{
			SchemaVersion: task.SchemaVersion,
			Legacy:        privateDaemonIdentityForBinding(first),
			Successor:     privateDaemonIdentityForBinding(second),
			LegacyPID:     101,
			LegacyStopped: true,
		},
		{
			SchemaVersion: task.SchemaVersion,
			Legacy:        privateDaemonIdentityForBinding(second),
			Successor:     privateDaemonIdentityForBinding(third),
			LegacyPID:     102,
			LegacyStopped: true,
		},
	} {
		if err := appendPrivateMigrationHistory(basePath, migration); err != nil {
			t.Fatal(err)
		}
	}

	for name, binding := range map[string]task.SupervisorRef{
		"first predecessor":  first,
		"second predecessor": second,
		"latest successor":   third,
	} {
		t.Run(name, func(t *testing.T) {
			matched, err := PrivateSupervisorGenerationMatches(fixture.stateRoot, binding)
			if err != nil {
				t.Fatal(err)
			}
			if !matched {
				t.Fatalf("published migration history did not match %+v", binding)
			}
		})
	}
	unrelated := third
	unrelated.Endpoint = "unix:" + filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "unrelated.sock")
	matched, err := PrivateSupervisorGenerationMatches(fixture.stateRoot, unrelated)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("unpublished supervisor generation matched migration history")
	}
}

func TestMigratePrivateKeepsRecoveryAliasUntilSuccessorReady(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, "#!/bin/sh\nexit 0\n")
	options := privateMigrationTestOptions(stateRoot, 100*time.Millisecond)
	legacy := bindPrivateSetupForTest(t, stateRoot, clientPath, oldDaemonPath, options)
	current := bindPrivateSetupForTest(t, stateRoot, clientPath, currentDaemonPath, options)
	if err := legacy.PersistPrivateRecoveryBinding(stateRoot); err != nil {
		t.Fatal(err)
	}
	if err := current.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := current.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateDaemonIdentity(filepath.Dir(current.Binding().ConfigPath), current.Binding()); err != nil {
		t.Fatal(err)
	}

	tryMigration := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return WithBootstrapLock(ctx, stateRoot, func() error {
			return current.MigratePrivateSupervisorWithLock(ctx, stateRoot)
		})
	}
	if err := tryMigration(); err == nil {
		t.Fatal("migration accepted an unavailable successor")
	}
	if _, err := os.Stat(privateRecoveryRecordPath(stateRoot)); err != nil {
		t.Fatalf("recovery alias was removed before successor readiness: %v", err)
	}

	legacyIdentity := privateDaemonIdentity{Executable: legacy.Binding().DaemonExecutable, SHA256: legacy.Binding().DaemonSHA256}
	successorIdentity := privateDaemonIdentity{Executable: current.Binding().DaemonExecutable, SHA256: current.Binding().DaemonSHA256}
	if err := publishPrivateMigrationRecord(filepath.Dir(current.Binding().ConfigPath), legacyIdentity, successorIdentity); err != nil {
		t.Fatal(err)
	}
	if err := tryMigration(); err == nil {
		t.Fatal("pending migration accepted an unavailable successor")
	}
	if _, err := os.Stat(privateRecoveryRecordPath(stateRoot)); err != nil {
		t.Fatalf("recovery alias was removed while pending successor was unavailable: %v", err)
	}
}

func TestPrivateSupervisorMigrationPendingReadsValidatedJournal(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	if err := ensurePrivateBase(filepath.Join(stateRoot, privateSupervisorDirectory)); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(base, "legacy-pueued")
	successorPath := filepath.Join(base, "successor-pueued")
	writeExecutable(t, legacyPath, privateDaemonFixture)
	writeExecutable(t, successorPath, privateDaemonFixture+"\n# successor\n")
	legacy := privateDaemonIdentity{Executable: legacyPath, SHA256: hashPrivateTestExecutable(t, legacyPath)}
	successor := privateDaemonIdentity{Executable: successorPath, SHA256: hashPrivateTestExecutable(t, successorPath)}
	if err := publishPrivateMigrationRecord(filepath.Join(stateRoot, privateSupervisorDirectory), legacy, successor); err != nil {
		t.Fatal(err)
	}
	pending, err := PrivateSupervisorMigrationPending(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("validated private migration journal was not reported as pending")
	}
	if err := os.WriteFile(privateMigrationRecordPath(filepath.Join(stateRoot, privateSupervisorDirectory)), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrivateSupervisorMigrationPending(stateRoot); !errors.Is(err, ErrBinding) {
		t.Fatalf("malformed private migration journal was accepted: %v", err)
	}
}

func TestMigratePrivateSupervisorWithLockRefusesActiveWork(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")
	options := privateMigrationTestOptions(stateRoot, DefaultObservationTimeout)
	if _, err := BindPrivate(context.Background(), clientPath, oldDaemonPath, stateRoot, options); err != nil {
		t.Fatal(err)
	}
	current, err := BindPrivate(context.Background(), clientPath, currentDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	status := statusTestPayload(t, map[string]any{
		"7": statusTestJob(7, "delegate:active", map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}}),
	}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if writeErr := os.WriteFile(filepath.Join(privateBase, "test-status.json"), status, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	err = WithBootstrapLock(context.Background(), stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("active private work was not preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(privateBase, "ready")); err != nil {
		t.Fatalf("active legacy supervisor was stopped: %v", err)
	}
	if _, err := os.Stat(privateRecoveryRecordPath(stateRoot)); err != nil {
		t.Fatalf("active migration did not retain recovery record: %v", err)
	}
	if got := readPrivateDaemonIdentity(t, stateRoot); got.Executable != legacyDaemonExecutable(t, oldDaemonPath) {
		t.Fatalf("active migration replaced legacy identity: %+v", got)
	}
}
