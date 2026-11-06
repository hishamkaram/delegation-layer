package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPrivateMaintenanceRemovesExactSupervisorRow(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	rootID := strings.Repeat("a", 32)
	taskID := strings.Repeat("b", 32)
	numericTaskID := int64(42)
	status := statusTestPayload(t, map[string]any{
		"42": statusTestJob(numericTaskID, "delegate:"+rootID+":"+taskID, map[string]any{"Done": map[string]any{
			"enqueued_at": statusTestTime,
			"start":       statusTestTime,
			"end":         statusTestTime,
			"result":      "Success",
		}}),
	}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if err := os.WriteFile(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-status.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-record-kill")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	client.options.SuccessfulCleanupAuthorizer = allowSuccessfulCleanupForTest
	startPrivateMaintenancePass(t, client, func() {}).requireCompleted(t)
	data, err := os.ReadFile(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-kill-task-id"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "42" {
		t.Fatalf("removed task id=%q, want 42", got)
	}
}

func TestPrivateMaintenanceKeepsExactRunningSupervisorRow(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	rootID := strings.Repeat("a", 32)
	taskID := strings.Repeat("b", 32)
	numericTaskID := int64(42)
	status := statusTestPayload(t, map[string]any{
		"42": statusTestJob(numericTaskID, "delegate:"+rootID+":"+taskID, map[string]any{"Running": map[string]any{
			"enqueued_at": statusTestTime,
			"start":       statusTestTime,
		}}),
	}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 1}})
	if err := os.WriteFile(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-status.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-record-kill")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	client.options.SuccessfulCleanupAuthorizer = allowSuccessfulCleanupForTest
	startPrivateMaintenancePass(t, client, func() {}).requireCompleted(t)
	if _, err := os.Stat(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-kill-task-id")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("running task was removed or removal marker was unavailable: %v", err)
	}
}

func TestCreatePrivateConfigPublishesCompleteFile(t *testing.T) {
	base := filepath.Join(canonicalTemp(t), "supervisor")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "pueue.yml")
	want := "private-config\n"
	if err := createPrivateConfig(configPath, base, want); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("published private config=%q, want %q", got, want)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pueue.yml-") {
			t.Fatalf("staged private config was left behind: %s", entry.Name())
		}
	}
}

func TestBindPrivateBootstrapsAndReusesSupervisor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	first := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if got := first.Binding().ObservedVersion; got != "pueue 99.7.3" {
		t.Fatalf("runtime version was not preserved: %q", got)
	}
	assertPrivateDaemonIdentityMatchesBinding(t, fixture.stateRoot, first.Binding())
	identityAtStart := readPrivateSupervisorFile(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "daemon-identity-at-start"))
	if string(identityAtStart) != "absent\n" {
		t.Fatalf("launch provenance was published before daemon readiness: %q", identityAtStart)
	}
	assertPrivateConfigRootedInState(t, fixture.stateRoot)
	if err := WithBootstrapLock(t.Context(), fixture.stateRoot, func() error {
		_, configureErr := first.SetPrivateDefaultParallelTasksWithBootstrapLock(t.Context(), fixture.stateRoot, 0)
		return configureErr
	}); err != nil {
		t.Fatalf("private admission did not configure unlimited ordinary parallelism: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "parallel-unlimited")); err != nil {
		t.Fatalf("private bootstrap did not configure unlimited ordinary parallelism: %v", err)
	}

	second := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if second.Binding().ConfigPath != first.Binding().ConfigPath || second.Binding().Endpoint != first.Binding().Endpoint {
		t.Fatalf("private binding changed across reuse: first=%+v second=%+v", first.Binding(), second.Binding())
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestRecoverPrivateRestartsStoppedSupervisor(t *testing.T) {
	base := canonicalTemp(t)
	stateRoot := filepath.Join(base, "state")
	writePrivateStateRootRecord(t, stateRoot)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)

	options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout}
	first, err := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, stateRoot)
	saved := first.Binding()
	saved.ResolutionCwd = filepath.Join(base, "removed-before-recovery")
	recovery, err := AttemptPrivateRecovery(context.Background(), stateRoot, saved, clientPath, daemonPath, options)
	recovered := recovery.Client()
	assertPrivateRecoverySucceeded(t, recovery, err)
	if recovered.Binding() != saved {
		t.Fatalf("recovery changed the saved supervisor binding: saved=%+v recovered=%+v", saved, recovered.Binding())
	}
	command, err := recovered.prepareCommand("status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Dir(recovered.RuntimeBinding().ConfigPath); command.Dir != want {
		t.Fatalf("recovered supervisor command directory=%q, want config directory %q", command.Dir, want)
	}
	log, err := os.ReadFile(filepath.Join(stateRoot, privateSupervisorDirectory, "daemon-started"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "started\n"); got != 2 {
		t.Fatalf("private daemon was restarted %d times, want 2: %q", got, log)
	}
}

func TestRecoverPrivatePredecessorAdoptsLiveCompletedSuccessor(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, true)
	historyBefore := readPrivateSupervisorFile(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))
	identityBefore := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot))

	recovery, err := AttemptPrivateRecovery(context.Background(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
	recovered := recovery.Client()
	assertPrivateRecoverySucceeded(t, recovery, err)
	fixture.setCleanupClient(recovered)
	if recovered.Binding() != fixture.saved {
		t.Fatalf("recovery changed durable predecessor binding: got=%+v want=%+v", recovered.Binding(), fixture.saved)
	}
	if recovered.RuntimeBinding() != fixture.successor {
		t.Fatalf("recovery did not use completed successor at runtime: got=%+v want=%+v", recovered.RuntimeBinding(), fixture.successor)
	}
	if identity := readPrivateDaemonIdentity(t, fixture.stateRoot); identity.Executable != fixture.successor.DaemonExecutable || identity.SHA256 != fixture.successor.DaemonSHA256 {
		t.Fatalf("recovery changed completed daemon identity: got=%+v successor=%+v", identity, fixture.successor)
	}
	if got := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot)); !bytes.Equal(got, identityBefore) {
		t.Fatal("live predecessor recovery republished the completed supervisor binding")
	}
	if got := readPrivateSupervisorFile(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, historyBefore) {
		t.Fatal("live predecessor recovery changed migration history")
	}
	if _, err := os.Stat(privateMigrationRecordPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live predecessor recovery created a migration journal: %v", err)
	}
	if err := recovered.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recovered.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPrivatePredecessorAdoptsLiveSuccessorWithCompatiblePair(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, true)
	compatibleClient, compatibleDaemon := installPrivateSupervisorPair(t, filepath.Join(fixture.base, "compatible"))
	removePrivateTestPath(t, filepath.Join(fixture.base, "completed"))
	identityBefore := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))
	supervisorBefore := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot))

	recovery, err := AttemptPrivateRecovery(context.Background(), fixture.stateRoot, fixture.saved, compatibleClient, compatibleDaemon, fixture.options)
	recovered := recovery.Client()
	assertPrivateRecoverySucceeded(t, recovery, err)
	fixture.setCleanupClient(recovered)
	if recovered.Binding() != fixture.saved {
		t.Fatalf("recovery changed durable predecessor binding: got=%+v want=%+v", recovered.Binding(), fixture.saved)
	}
	if !task.SameSupervisorIdentity(recovered.RuntimeBinding(), fixture.successor) || recovered.RuntimeBinding().ClientExecutable != compatibleClient || recovered.RuntimeBinding().DaemonExecutable != compatibleDaemon {
		t.Fatalf("recovery did not retain completed successor endpoint with compatible runtime pair: got=%+v successor=%+v", recovered.RuntimeBinding(), fixture.successor)
	}
	if got := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, identityBefore) {
		t.Fatal("compatible live recovery changed daemon identity")
	}
	if got := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot)); !bytes.Equal(got, supervisorBefore) {
		t.Fatal("compatible live recovery changed supervisor identity")
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	if err := recovered.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recovered.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPrivateRestartsExactCompletedSuccessorForPredecessor(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, false)
	historyBefore := readPrivateSupervisorFile(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))
	identityBefore := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))
	supervisorBefore := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot))

	recovery, err := AttemptPrivateRecovery(context.Background(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
	recovered := recovery.Client()
	assertPrivateRecoverySucceeded(t, recovery, err)
	fixture.setCleanupClient(recovered)
	if recovered.Binding() != fixture.saved {
		t.Fatalf("recovery changed durable predecessor binding: got=%+v want=%+v", recovered.Binding(), fixture.saved)
	}
	if recovered.RuntimeBinding() != fixture.successor {
		t.Fatalf("recovery did not restart exact completed successor: got=%+v want=%+v", recovered.RuntimeBinding(), fixture.successor)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateDaemonIdentityMatchesBinding(t, fixture.stateRoot, fixture.successor)
	if got := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, identityBefore) {
		t.Fatal("exact successor restart changed the completed daemon identity")
	}
	if got := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot)); !bytes.Equal(got, supervisorBefore) {
		t.Fatal("exact successor restart changed the completed supervisor binding")
	}
	if got := readPrivateSupervisorFile(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, historyBefore) {
		t.Fatal("exact successor restart changed migration history")
	}
	if _, err := os.Stat(privateMigrationRecordPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact successor restart created a migration journal: %v", err)
	}
	if err := recovered.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recovered.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPrivateRejectsUnrelatedPredecessorWithoutStartingSuccessor(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, false)
	unrelated := fixture.saved
	unrelated.ConfigDigest = task.ComputeSHA256([]byte("unrelated config"))
	unrelated.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("unrelated resolved config"))
	unrelated.Endpoint = "unix:" + filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "unrelated.sock")
	identityBefore := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))
	supervisorBefore := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot))
	historyBefore := readPrivateSupervisorFile(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))

	recovery, err := AttemptPrivateRecovery(context.Background(), fixture.stateRoot, unrelated, fixture.clientPath, fixture.daemonPath, fixture.options)
	assertPrivateRecoveryHasNoResult(t, recovery)
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unrelated saved predecessor was accepted: result=%+v err=%v", recovery, err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	if got := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, identityBefore) {
		t.Fatal("unrelated recovery changed daemon identity")
	}
	if got := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot)); !bytes.Equal(got, supervisorBefore) {
		t.Fatal("unrelated recovery changed supervisor identity")
	}
	if got := readPrivateSupervisorFile(t, privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, historyBefore) {
		t.Fatal("unrelated recovery changed migration history")
	}
}

func TestRecoverPrivateStartsCompletedSuccessorInsteadOfStagedPair(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, false)
	stagedClient, stagedDaemon := installPrivateSupervisorPair(t, filepath.Join(fixture.base, "staged"))
	recovery, err := AttemptPrivateRecovery(context.Background(), fixture.stateRoot, fixture.saved, stagedClient, stagedDaemon, fixture.options)
	recovered := recovery.Client()
	assertPrivateRecoverySucceeded(t, recovery, err)
	fixture.setCleanupClient(recovered)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateDaemonIdentityMatchesBinding(t, fixture.stateRoot, fixture.successor)
	if err := recovered.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recovered.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPrivateRestoresExactLegacyGenerationWhenCompletedBindingMissing(t *testing.T) {
	fixture := newCompletedPrivateLegacyTestFixture(t)
	if err := os.Remove(privateSupervisorBindingPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPID(t, fixture.stateRoot)
	recovery, err := AttemptPrivateRecovery(context.Background(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
	recovered := recovery.Client()
	assertPrivateRecoverySucceeded(t, recovery, err)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateDaemonIdentityMatchesBinding(t, fixture.stateRoot, fixture.saved)
	persisted, found, err := loadPrivateSupervisorBinding(fixture.stateRoot)
	if err != nil || !found || persisted != fixture.saved {
		t.Fatalf("legacy recovery did not republish exact saved binding: found=%t binding=%+v err=%v", found, persisted, err)
	}
	if err := recovered.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recovered.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverPrivateRejectsDifferentPairWhenCompletedBindingMissing(t *testing.T) {
	fixture := newCompletedPrivateLegacyTestFixture(t)
	if err := os.Remove(privateSupervisorBindingPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	differentClient, differentDaemon := installPrivateSupervisorPair(t, filepath.Join(fixture.base, "different"))
	different := fixture.saved
	different.ClientExecutable = canonicalPrivateTestExecutable(t, differentClient)
	different.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	different.ClientSHA256, different.DaemonSHA256 = "", ""
	different.DaemonExecutable = canonicalPrivateTestExecutable(t, differentDaemon)
	different.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	different.ClientSHA256, different.DaemonSHA256 = "", ""
	identityBefore := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))

	recovery, err := AttemptPrivateRecovery(context.Background(), fixture.stateRoot, different, differentClient, differentDaemon, fixture.options)
	assertPrivateRecoveryHasNoResult(t, recovery)
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("different saved generation was accepted without a completed binding: result=%+v err=%v", recovery, err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	if got := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, identityBefore) {
		t.Fatal("rejected legacy recovery changed daemon identity")
	}
}

func TestAttemptPrivateRecoveryReportsMissingPairFact(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, false)
	compatibleClient, compatibleDaemon := installPrivateSupervisorPair(t, filepath.Join(fixture.base, "compatible"))
	removePrivateTestPath(t, filepath.Join(fixture.base, "completed"))

	recovery, err := AttemptPrivateRecovery(context.Background(), fixture.stateRoot, fixture.saved, compatibleClient, compatibleDaemon, fixture.options)
	if err != nil || !recovery.PairUnavailable() || recovery.Client() != nil {
		t.Fatalf("missing completed pair = recovery %+v err %v, want PairUnavailable", recovery, err)
	}
	if !errors.Is(recovery.Cause(), ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("missing-pair cause lost its identity: %v", recovery.Cause())
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestCompletedPrivateRecoveryKeepsOperationalFaultsJoinedWithMissingFile(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, false)
	for _, test := range []struct {
		name  string
		cause error
	}{
		{name: "options", cause: ErrConfiguration},
		{name: "config", cause: ErrBinding},
		{name: "permission", cause: os.ErrPermission},
		{name: "verification", cause: errors.New("published pair verification failed")},
		{name: "close", cause: errors.New("published client close failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			// This is the existing preparation result port, not a filesystem
			// lookup oracle: a joined ENOENT never erases an operational fault.
			fault := errors.Join(test.cause, os.ErrNotExist)
			recovery, err := recoverUnpreparedPublishedPrivate(t.Context(), unpreparedPublishedPrivate{
				stateRoot: fixture.stateRoot, saved: fixture.saved, published: fixture.successor,
				clientExecutable: fixture.clientPath, daemonExecutable: fixture.daemonPath,
				options: fixture.options, prepared: true, pairErr: os.ErrNotExist, prepareErr: fault,
			})
			if !errors.Is(err, test.cause) || !errors.Is(err, os.ErrNotExist) || recovery.PairUnavailable() || recovery.Client() != nil {
				t.Fatalf("operational fault was converted to pair availability: recovery=%+v err=%v", recovery, err)
			}
		})
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestAttemptPrivateRecoveryRefusesNonExecutablePresentPair(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(map[bool]string{true: "live", false: "stopped"}[live], func(t *testing.T) {
			fixture := newCompletedPrivateSuccessorTestFixture(t, live)
			compatibleClient, compatibleDaemon := installPrivateSupervisorPair(t, filepath.Join(fixture.base, "compatible"))
			t.Cleanup(func() {
				if err := os.Chmod(fixture.daemonPath, 0o700); err != nil {
					t.Error(err)
				}
			})
			if err := os.Chmod(fixture.daemonPath, 0o600); err != nil {
				t.Fatal(err)
			}
			before := snapshotSavedProbeTestState(t, fixture.stateRoot)
			recovery, err := AttemptPrivateRecovery(t.Context(), fixture.stateRoot, fixture.saved, compatibleClient, compatibleDaemon, fixture.options)
			closeSavedProbeTestClient(t, recovery.Client(), err)
			if recovery.Client() != nil || recovery.PairUnavailable() || !errors.Is(err, ErrBinding) || !errors.Is(err, ErrConfiguration) {
				t.Fatalf("invalid installed metadata did not remain an operational fault: recovery=%+v err=%v", recovery, err)
			}
			assertSavedProbeTestUnchanged(t, fixture.stateRoot, before)
			starts := 1
			if live {
				starts = 2
			}
			assertPrivateDaemonStartCount(t, fixture.stateRoot, starts)
		})
	}
}

// TestCompletedMigrationPreparationFaultCannotFallThrough keeps an unreadable
// identity publication of the completed generation as the operational
// result: the live compatible endpoint is not used to hide it and nothing
// starts.
func TestCompletedMigrationPreparationFaultCannotFallThrough(t *testing.T) {
	fixture := newCompletedPrivateSuccessorTestFixture(t, true)
	compatibleClient, compatibleDaemon := installPrivateSupervisorPair(t, filepath.Join(fixture.base, "compatible"))
	makePrivateTestDirectory(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, privateIdentityPublicationRecordName))

	recovery, err := AttemptPrivateRecovery(context.Background(), fixture.stateRoot, fixture.saved, compatibleClient, compatibleDaemon, fixture.options)
	if err == nil || recovery.PairUnavailable() || recovery.Client() != nil {
		t.Fatalf("preparation fault = recovery %+v err %v, want operational failure", recovery, err)
	}
	if !errors.Is(err, ErrBinding) {
		t.Fatalf("preparation fault lost its identity: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}

func newCompletedPrivateSuccessorTestFixture(t *testing.T, live bool) completedPrivateSuccessorTestFixture {
	t.Helper()
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldClient, oldDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "predecessor"))
	currentClient, currentDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "completed"))
	options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout}
	legacy := bindPrivateSetupForTest(t, stateRoot, oldClient, oldDaemon, options)
	options = legacy.options
	saved := legacy.Binding()
	if err := legacy.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := legacy.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	configPath := saved.ConfigPath
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	oldSocket := strconv.Quote(filepath.Join(stateRoot, privateSupervisorDirectory, "run", "pueue.sock"))
	newSocket := strconv.Quote(filepath.Join(stateRoot, privateSupervisorDirectory, "run", "completed.sock"))
	changed := strings.Replace(string(data), "unix_socket_path: "+oldSocket, "unix_socket_path: "+newSocket, 1)
	if changed == string(data) {
		t.Fatal("completed successor fixture did not change its endpoint")
	}
	if writeErr := os.WriteFile(configPath, []byte(changed), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	successorClient, err := Bind(context.Background(), currentClient, configPath, options)
	if err != nil {
		t.Fatal(err)
	}
	setCleanupClient := ownCompletedSuccessorClient(t, successorClient, live)
	if err := attachPrivateDaemon(successorClient, currentDaemon); err != nil {
		t.Fatal(err)
	}
	successor := successorClient.Binding()
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := publishPrivateDaemonIdentity(privateBase, successor); err != nil {
		t.Fatal(err)
	}
	if err := appendPrivateMigrationHistory(privateBase, privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(saved),
		Successor:     privateDaemonIdentityForBinding(successor),
		LegacyPID:     101,
		LegacyStopped: true,
	}); err != nil {
		t.Fatal(err)
	}
	if live {
		if err := successorClient.StartPrivate(context.Background(), stateRoot); err != nil {
			t.Fatal(err)
		}
	}
	return completedPrivateSuccessorTestFixture{
		base: base, stateRoot: stateRoot, clientPath: currentClient, daemonPath: currentDaemon,
		saved: saved, successor: successor, options: options, setCleanupClient: setCleanupClient,
	}
}

// Register before Start so the fixture's real daemon Wait joins before client
// teardown. A compatible recovery client may take over marker cleanup when
// the test removes the original installation; both clients still close.
func ownCompletedSuccessorClient(t *testing.T, original *Client, live bool) func(*Client) {
	t.Helper()
	cleanupClient := original
	t.Cleanup(func() {
		if live {
			if err := cleanupClient.Shutdown(context.Background()); err != nil {
				t.Errorf("stop completed successor fixture: %v", err)
			}
			if err := cleanupClient.WaitUnavailable(context.Background()); err != nil {
				t.Errorf("wait for completed successor fixture: %v", err)
			}
		}
		if cleanupClient != original {
			if err := cleanupClient.Close(); err != nil {
				t.Error(err)
			}
		}
		if err := original.Close(); err != nil {
			t.Error(err)
		}
	})
	return func(client *Client) { cleanupClient = client }
}

func newCompletedPrivateLegacyTestFixture(t *testing.T) completedPrivateLegacyTestFixture {
	t.Helper()
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath, daemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "legacy"))
	options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout}
	client := bindPrivateSetupForTest(t, stateRoot, clientPath, daemonPath, options)
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.WaitUnavailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	return completedPrivateLegacyTestFixture{base: base, stateRoot: stateRoot, clientPath: clientPath, daemonPath: daemonPath, saved: client.Binding(), options: options}
}

func TestPublishedPrivatePairSelectionKeepsOperationalFaults(t *testing.T) {
	for _, fault := range []string{"historical-copy", "configuration", "other-member-stat"} {
		t.Run(fault, func(t *testing.T) {
			fixture := newGenerationGuardFixture(t)
			binding := fixture.client.Binding()
			options := fixture.client.options
			switch fault {
			case "historical-copy":
				removePrivateTestPath(t, binding.ClientExecutable)
				options.RuntimeClientExecutable = filepath.Join(fixture.base, "pueue-pair-"+strings.Repeat("a", 64), "pueue")
			case "configuration":
				removePrivateTestPath(t, binding.DaemonExecutable)
				options.ObservationTimeout = -1
			case "other-member-stat":
				removePrivateTestPath(t, binding.ClientExecutable)
				removePrivateTestPath(t, binding.DaemonExecutable)
				if err := os.Symlink(binding.DaemonExecutable, binding.DaemonExecutable); err != nil {
					t.Fatal(err)
				}
			}
			client, pairErr, err := preparePublishedPrivatePair(t.Context(), binding, options)
			if client != nil || pairErr != nil || err == nil {
				t.Fatalf("operational %s became unavailable pair: client=%t pairErr=%v err=%v", fault, client != nil, pairErr, err)
			}
			if fault == "configuration" && !errors.Is(err, ErrConfiguration) {
				t.Fatalf("configuration cause was lost: %v", err)
			}
		})
	}
}
