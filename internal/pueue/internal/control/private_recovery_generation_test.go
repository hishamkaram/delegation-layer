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

func TestRemoveCompletedTaskUsesExactSupervisorRow(t *testing.T) {
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
	identity := Identity{
		RootID:        rootID,
		TaskID:        taskID,
		SpecSHA256:    strings.Repeat("c", 64),
		MetaSHA256:    strings.Repeat("d", 64),
		NumericTaskID: &numericTaskID,
	}
	if err := client.RemoveCompletedTask(context.Background(), identity); err != nil {
		t.Fatalf("completed task cleanup failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-kill-task-id"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "42" {
		t.Fatalf("removed task id=%q, want 42", got)
	}
}

func TestRemoveCompletedTaskDefersExactRunningSupervisorRow(t *testing.T) {
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
	identity := Identity{
		RootID:        rootID,
		TaskID:        taskID,
		SpecSHA256:    strings.Repeat("c", 64),
		MetaSHA256:    strings.Repeat("d", 64),
		NumericTaskID: &numericTaskID,
	}
	if err := client.RemoveCompletedTask(context.Background(), identity); !errors.Is(err, ErrCleanupDeferred) {
		t.Fatalf("running completed-task cleanup error=%v, want ErrCleanupDeferred", err)
	}
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

	options := Options{ObservationTimeout: privateTestObservationTimeout}
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
	recovered, err := RecoverPrivate(context.Background(), stateRoot, saved, clientPath, daemonPath, options)
	if err != nil {
		t.Fatal(err)
	}
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

	recovered, err := RecoverPrivate(context.Background(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
	if err != nil {
		t.Fatalf("predecessor recovery against live successor failed: %v", err)
	}
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

	recovered, err := RecoverPrivate(context.Background(), fixture.stateRoot, fixture.saved, compatibleClient, compatibleDaemon, fixture.options)
	if err != nil {
		t.Fatalf("predecessor recovery did not adopt live successor through compatible pair: %v", err)
	}
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

	recovered, err := RecoverPrivate(context.Background(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
	if err != nil {
		t.Fatalf("predecessor recovery did not restart completed successor: %v", err)
	}
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

	if _, err := RecoverPrivate(context.Background(), fixture.stateRoot, unrelated, fixture.clientPath, fixture.daemonPath, fixture.options); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unrelated saved predecessor was accepted: %v", err)
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
	recovered, err := RecoverPrivate(context.Background(), fixture.stateRoot, fixture.saved, stagedClient, stagedDaemon, fixture.options)
	if err != nil {
		t.Fatalf("recovery with staged successor inventory failed: %v", err)
	}
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
	recovered, err := RecoverPrivate(context.Background(), fixture.stateRoot, fixture.saved, fixture.clientPath, fixture.daemonPath, fixture.options)
	if err != nil {
		t.Fatalf("exact legacy generation was not restored: %v", err)
	}
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
	different.ClientSHA256 = hashPrivateTestExecutable(t, differentClient)
	different.DaemonExecutable = canonicalPrivateTestExecutable(t, differentDaemon)
	different.DaemonSHA256 = hashPrivateTestExecutable(t, differentDaemon)
	identityBefore := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)))

	if _, err := RecoverPrivate(context.Background(), fixture.stateRoot, different, differentClient, differentDaemon, fixture.options); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("different saved generation was accepted without a completed binding: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	if got := readPrivateSupervisorFile(t, privateDaemonIdentityPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))); !bytes.Equal(got, identityBefore) {
		t.Fatal("rejected legacy recovery changed daemon identity")
	}
}

func newCompletedPrivateSuccessorTestFixture(t *testing.T, live bool) completedPrivateSuccessorTestFixture {
	t.Helper()
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldClient, oldDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "predecessor"))
	currentClient, currentDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "completed"))
	options := Options{ObservationTimeout: privateTestObservationTimeout}
	legacy := bindPrivateSetupForTest(t, stateRoot, oldClient, oldDaemon, options)
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
		t.Cleanup(func() {
			if cleanupErr := successorClient.Shutdown(context.Background()); cleanupErr != nil {
				t.Logf("stop completed successor fixture: %v", cleanupErr)
			}
			if waitErr := successorClient.WaitUnavailable(context.Background()); waitErr != nil {
				t.Logf("wait for completed successor fixture: %v", waitErr)
			}
		})
	}
	return completedPrivateSuccessorTestFixture{
		base: base, stateRoot: stateRoot, clientPath: currentClient, daemonPath: currentDaemon,
		saved: saved, successor: successor, options: options,
	}
}

func newCompletedPrivateLegacyTestFixture(t *testing.T) completedPrivateLegacyTestFixture {
	t.Helper()
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath, daemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "legacy"))
	options := Options{ObservationTimeout: privateTestObservationTimeout}
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
