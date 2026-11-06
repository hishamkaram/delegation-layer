package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestVerifyPrivateMigrationEndpointFailsClosedWithoutLogicalPredecessor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout}
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
	options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout}
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
	options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout}
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
	first := bindPrivateSetupForTest(t, stateRoot, clientPath, oldDaemonPath, options)
	ownPrivateMigrationTestClient(t, first)
	options = first.options
	current := preparePrivateCandidateForTest(t, stateRoot, clientPath, currentDaemonPath, options)
	pending := privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(first.Binding()),
		Successor:     privateDaemonIdentityForBinding(current.Binding()),
	}
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.WaitUnavailable(context.Background()); err != nil {
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
	assertRecoveredPrivateMigrationPredecessor(t, stateRoot, first.Binding())
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
	legacy := bindPrivateSetupForTest(t, stateRoot, clientPath, legacyDaemonPath, options)
	ownPrivateMigrationTestClient(t, legacy)
	options = legacy.options
	successor := preparePrivateCandidateForTest(t, stateRoot, clientPath, successorDaemonPath, options)
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

	if err = startDaemon(context.Background(), stateRoot, successor.runtimeBinding.DaemonExecutable, successor.runtimeBinding.ConfigPath, basePath, options); err != nil {
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
	assertRecoveredPrivateMigrationPredecessor(t, stateRoot, legacy.Binding())
	assertPrivateDaemonStartCount(t, stateRoot, 2)
	if _, err = os.Stat(privateMigrationRecordPath(basePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed successor launch migration journal remained: %v", err)
	}
}

func TestPendingPrivateMigrationKeepsAcceptedTargetAfterSecondInstallationUpgrade(t *testing.T) {
	root, bindings := newPrivateSuccessorTestState(t)
	base := filepath.Join(root, privateSupervisorDirectory)
	legacy, first, second := bindings["A"], bindings["B"], bindings["C"]
	pending := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(legacy), Successor: privateDaemonIdentityForBinding(first)}
	if err := publishPrivateMigrationState(base, pending); err != nil {
		t.Fatal(err)
	}
	before := snapshotPrivateSuccessorTestState(t, root)
	client := &Client{binding: second, runtimeBinding: second}
	err := client.resumeValidatedPreparedPrivateMigration(t.Context(), root, base, legacy, true, pending.Legacy, true, privateDaemonIdentityForBinding(second), privateDaemonIdentityForBinding(second), pending)
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("new installation silently selected instead of accepted target: %v", err)
	}
	if after := snapshotPrivateSuccessorTestState(t, root); !reflect.DeepEqual(before, after) {
		t.Fatal("new installation changed accepted journal")
	}
}

func TestPendingPrivateMigrationSelectsInstalledTargetBeforeShutdown(t *testing.T) {
	// The production store owns durable root and generation records. Bootstrap
	// EX serializes migration; its generation guard owns every publication.
	store := repairEditorTestStore(t)
	stateRoot := store.Root
	base := filepath.Dir(stateRoot)
	legacyClient, legacyDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "predecessor"))
	preparedInstallation := filepath.Join(base, "prepared")
	preparedClient, preparedDaemon := installPrivateSupervisorPair(t, preparedInstallation)
	selectedClient, selectedDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "selected"))
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	waitOwner, _ := ownPrivateRecoveryDaemonWaits(t, stateRoot)
	options.DaemonWaitOwner = waitOwner
	legacy := bindPrivateSetupForTest(t, stateRoot, legacyClient, legacyDaemon, options)
	ownPrivateMigrationTestClient(t, legacy)
	options = legacy.options
	prepared := preparePrivateCandidateForTest(t, stateRoot, preparedClient, preparedDaemon, options)
	pending := privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(legacy.Binding()),
		Successor:     privateDaemonIdentityForBinding(prepared.Binding()),
	}
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := WithBootstrapLockContext(t.Context(), stateRoot, func(ctx context.Context) error {
		return publishPrivateMigrationStateContext(ctx, privateBase, pending, options.PrivateGenerationGuard)
	}); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPath(t, preparedInstallation)
	selected := preparePrivateCandidateForTest(t, stateRoot, selectedClient, selectedDaemon, options)
	assertPreparedPrivateMigrationBeforeShutdown(t, privateBase, pending)
	if err := WithBootstrapLockContext(t.Context(), stateRoot, func(ctx context.Context) error {
		return selected.MigratePrivateSupervisorWithLock(ctx, stateRoot)
	}); err != nil {
		t.Fatalf("ordinary migration rejected the current installed target before predecessor shutdown: %v", err)
	}
	assertPrivateDaemonIdentityMatchesBinding(t, stateRoot, selected.Binding())
	assertRecoveredPrivateMigrationPredecessor(t, stateRoot, legacy.Binding())
	assertPrivateDaemonStartCount(t, stateRoot, 2)
}

func assertPreparedPrivateMigrationBeforeShutdown(t *testing.T, base string, want privateMigrationRecord) {
	t.Helper()
	pending, found, err := loadPrivateMigrationRecord(base)
	if err != nil || !found || pending != want {
		t.Fatalf("prepared proposal changed before migration: found=%t pending=%+v err=%v", found, pending, err)
	}
	if pending.LegacyShutdownRequested || pending.LegacyShutdownDispatched || pending.LegacyStopped {
		t.Fatal("prepared proposal already crossed predecessor shutdown")
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
	current := preparePrivateCandidateForTest(t, stateRoot, clientPath, currentDaemonPath, options)
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

	legacyIdentity := privateDaemonIdentityForBinding(legacy.Binding())
	successorIdentity := privateDaemonIdentityForBinding(current.Binding())
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
	legacyBinding := validBindingForTest()
	legacyBinding.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	legacyBinding.ClientSHA256 = ""
	legacyBinding.ConfigPath = PrivateConfigPath(stateRoot)
	legacyBinding.DaemonExecutable = legacyPath
	legacy := privateDaemonIdentityForBinding(legacyBinding)
	successor := legacy
	successor.Executable = successorPath
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
	options.DaemonWaitOwner, _ = ownPrivateRecoveryDaemonWaits(t, stateRoot)
	legacy := bindPrivateSetupForTest(t, stateRoot, clientPath, oldDaemonPath, options)
	ownPrivateMigrationTestClient(t, legacy)
	current := preparePrivateCandidateForTest(t, stateRoot, clientPath, currentDaemonPath, legacy.options)
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	status := statusTestPayload(t, map[string]any{
		"7": statusTestJob(7, "delegate:active", map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}}),
	}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if writeErr := os.WriteFile(filepath.Join(privateBase, "test-status.json"), status, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	err := WithBootstrapLockContext(t.Context(), stateRoot, func(ctx context.Context) error {
		return current.MigratePrivateSupervisorWithLock(ctx, stateRoot)
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

func publishPrivateMigrationRecord(base string, legacy, successor privateDaemonIdentity) error {
	record := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: legacy, Successor: successor}
	return publishPrivateMigrationState(base, record)
}

func TestPreparedPrivateMigrationRetargetabilityRequiresReversibleProvenance(t *testing.T) {
	root, bindings := newPrivateSuccessorTestState(t)
	pending := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(bindings["A"]), Successor: privateDaemonIdentityForBinding(bindings["B"])}
	for _, name := range []string{"prepared", "requested", "dispatched", "stopped", "pid", "process-start", "missing-legacy-logical", "missing-successor-logical"} {
		t.Run(name, func(t *testing.T) {
			candidate := pending
			switch name {
			case "prepared":
			case "requested":
				candidate.LegacyShutdownRequested = true
			case "dispatched":
				candidate.LegacyShutdownDispatched = true
			case "stopped":
				candidate.LegacyStopped = true
			case "pid":
				candidate.LegacyPID = 123
			case "process-start":
				candidate.LegacyProcessStart = "prior-process"
			case "missing-legacy-logical":
				candidate.Legacy = privateDaemonIdentity{ExecutableIdentity: pending.Legacy.ExecutableIdentity, Executable: pending.Legacy.Executable}
			case "missing-successor-logical":
				candidate.Successor = privateDaemonIdentity{ExecutableIdentity: pending.Successor.ExecutableIdentity, Executable: pending.Successor.Executable}
			}
			if got := privateMigrationRetargetable(candidate); got != (name == "prepared") {
				t.Fatalf("retargetable=%t for %s", got, name)
			}
		})
	}
	if err := publishPrivateMigrationState(filepath.Join(root, privateSupervisorDirectory), pending); err != nil {
		t.Fatal(err)
	}
	if err := WithBootstrapLockContext(t.Context(), root, func(context.Context) error {
		retargetable, err := PrivateMigrationRetargetableWithLock(root)
		if err == nil && !retargetable {
			t.Fatal("public migration decision refused the reversible journal")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedPrivateMigrationSuccessorReplacementIsCompareAndSwap(t *testing.T) {
	root, bindings := newPrivateSuccessorTestState(t)
	base := filepath.Join(root, privateSupervisorDirectory)
	pending := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(bindings["A"]), Successor: privateDaemonIdentityForBinding(bindings["B"])}
	if err := publishPrivateMigrationState(base, pending); err != nil {
		t.Fatal(err)
	}
	replacement := pending
	replacement.Successor = privateDaemonIdentityForBinding(bindings["C"])
	if err := replacePrivateMigrationRecord(base, pending, replacement); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("phase writer changed accepted identities: %v", err)
	}
	if err := replacePreparedPrivateMigrationSuccessor(base, pending, pending.Legacy); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("self-edge was accepted: %v", err)
	}
	if err := replacePreparedPrivateMigrationSuccessor(base, pending, replacement.Successor); err != nil {
		t.Fatal(err)
	}
	assertPreparedPrivateMigrationBeforeShutdown(t, base, replacement)
	if err := replacePreparedPrivateMigrationSuccessor(base, pending, pending.Successor); !errors.Is(err, ErrBinding) {
		t.Fatalf("lost compare-and-swap replaced a different journal: %v", err)
	}
	assertPreparedPrivateMigrationBeforeShutdown(t, base, replacement)
}

func TestPrivateSupervisorMigrationPendingClassifiesPersistedFaults(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		payload string
		cause   error
	}{
		{name: "malformed JSON", payload: "}", cause: ErrBinding},
		{name: "incomplete record", payload: `{"schema_version":1}`, cause: ErrBinding},
		{name: "unsupported schema", cause: task.ErrUnsupportedSchema},
		{name: "invalid identity", cause: ErrBinding},
		{name: "unreadable journal", cause: ErrConfiguration},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := materializePrivateMigrationReaderFault(t, fixture.name, fixture.payload)
			pending, err := PrivateSupervisorMigrationPending(root)
			var recovery *PrivateRecoveryError
			if pending || !errors.As(err, &recovery) || recovery.ReasonCode != "state_unreadable" {
				t.Fatalf("persisted fault lost its public reason: pending=%t err=%v", pending, err)
			}
			if !errors.Is(err, fixture.cause) || !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
				t.Fatalf("persisted fault lost its original cause: %v", err)
			}
			if fixture.name == "malformed JSON" {
				var syntax *json.SyntaxError
				if !errors.As(err, &syntax) {
					t.Fatalf("malformed journal lost its JSON decode cause: %v", err)
				}
			}
		})
	}
}

func materializePrivateMigrationReaderFault(t *testing.T, name, data string) string {
	t.Helper()
	root, bindings := newPrivateSuccessorTestState(t)
	path := privateMigrationRecordPath(filepath.Join(root, privateSupervisorDirectory))
	if name == "unreadable journal" {
		makePrivateTestDirectory(t, path)
		return root
	}
	payload := []byte(data)
	if name == "unsupported schema" || name == "invalid identity" {
		record := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(bindings["A"]), Successor: privateDaemonIdentityForBinding(bindings["B"])}
		if name == "unsupported schema" {
			record.SchemaVersion++
		} else {
			record.Legacy.Executable = "relative-daemon"
		}
		var err error
		payload, err = json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
	}
	writePrivateSuccessorTestBytes(t, path, payload)
	return root
}

func TestPrivateSupervisorMigrationPendingPreservesAbsenceAndInvalidRoot(t *testing.T) {
	root, _ := newPrivateSuccessorTestState(t)
	if pending, err := PrivateSupervisorMigrationPending(root); err != nil || pending {
		t.Fatalf("missing journal became a recovery fault: pending=%t err=%v", pending, err)
	}
	pending, err := PrivateSupervisorMigrationPending("relative-root")
	var recovery *PrivateRecoveryError
	if pending || !errors.Is(err, ErrConfiguration) || errors.Is(err, ErrPrivateSupervisorMigrationRequired) || errors.As(err, &recovery) {
		t.Fatalf("invalid root changed its configuration contract: pending=%t err=%v", pending, err)
	}
}
