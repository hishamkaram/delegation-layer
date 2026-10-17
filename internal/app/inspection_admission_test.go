package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestInspectionDeadlineIsOperationalDispatchFailure(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	briefPath := filepath.Join(request.CanonicalCwd, "brief.md")
	writeAppTestFile(t, briefPath, []byte("inspection brief"))
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"dispatch", "--json", "--root", store.Root,
		"--provider", request.Provider, "--brief", briefPath,
		"--cwd", request.CanonicalCwd, "--id", request.TaskID,
	}, &stdout, &stderr, Dependencies{
		PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
			return commonprovider.ProfileCandidate{}, errors.Join(errors.New("inspection preparation"), inspection.ErrAdmissionExpired)
		},
	})
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("deadline exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.TaskID != request.TaskID || response.Admission != "unknown" || response.Outcome != nil {
		t.Fatalf("deadline invented terminal authority: %+v", response)
	}
	assertAppInspectionNoOrdinaryTasks(t, store)
}

func TestEnsureInspectionGroupReportsExternalMigrationUnsupported(t *testing.T) {
	store := newBareInspectionStore(t)
	legacy, _, _ := newUncertainGroupSupervisor(t, store.RootID)
	current, _, _ := newUncertainGroupSupervisor(t, store.RootID)
	group, err := inspection.OpenGroup(store, legacy.Binding())
	if err != nil {
		t.Fatal(err)
	}
	if err = group.Close(); err != nil {
		t.Fatal(err)
	}
	err = supervisorcore.EnsureInspectionGroupWithMigration(store, current, time.Second, true)
	if !errors.Is(err, inspection.ErrGroupMigrationUnsupported) {
		t.Fatalf("external supervisor mismatch error=%v, want unsupported migration", err)
	}
}

func TestEnsureInspectionGroupUsesPublishedSuccessorGeneration(t *testing.T) {
	store := newBareInspectionStore(t)
	legacy, _, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	current, currentLog, currentGroupDone := newUncertainGroupSupervisorWithConfigAndTimeout(
		t,
		store.RootID,
		pueue.PrivateConfigPath(store.Root),
		15*time.Second,
	)
	legacyGroup := inspection.GroupRequestRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		Supervisor:    legacy.Binding(),
		Name:          (pueue.InspectionIdentity{RootID: store.RootID}).Group(),
		ParallelTasks: 1,
	}
	group, err := inspection.OpenGroup(store, legacy.Binding())
	if err != nil {
		t.Fatal(err)
	}
	if err = group.Close(); err != nil {
		t.Fatal(err)
	}
	successor := legacyGroup
	successor.Supervisor = current.Binding()
	publishAppTestGroupMigration(t, store, legacyGroup, successor)
	statusPath := filepath.Join(filepath.Dir(currentLog), "status.json")
	status := []byte(`{"tasks":{},"groups":{"delegation-inspection-` + store.RootID + `":{"status":"Running","parallel_tasks":1}}}`)
	if err = os.WriteFile(statusPath, status, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = supervisorcore.EnsureInspectionGroupWithMigration(store, current, 15*time.Second, false); err != nil {
		t.Fatalf("published successor generation was not reopened: %v", err)
	}
	if _, err = os.Stat(currentGroupDone); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published successor generation was recreated: %v", err)
	}
}

func TestMigrateInspectionGroupRecoversAfterPublishedSuccessorCrash(t *testing.T) {
	scenario := newHistoricalInspectionMigrationScenario(t)
	injectHistoricalInspectionMigrationCrash(t, scenario)
	assertHistoricalInspectionMigrationCrashState(t, scenario)
	completeHistoricalInspectionMigration(t, scenario)
}

type historicalInspectionMigrationScenario struct {
	fixture        *historicalSupervisorFixture
	legacyDir      string
	configPath     string
	config         []byte
	changedConfig  []byte
	legacyGroup    inspection.GroupRequestRecord
	successorGroup inspection.GroupRequestRecord
}

func newHistoricalInspectionMigrationScenario(t *testing.T) *historicalInspectionMigrationScenario {
	t.Helper()
	fixture := newHistoricalSupervisorFixturePaths(t)
	_, legacyDir := prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = newHistoricalSupervisorOptions()
	prepareHistoricalSupervisorTask(t, fixture, filepath.Join(filepath.Dir(fixture.root), "workspace"))

	openHistoricalInspectionGroup(t, fixture.store, fixture.predecessor)
	legacyGroup, migrated, err := inspection.ReadGroupHead(t.Context(), fixture.store)
	if err != nil || migrated {
		t.Fatalf("fixture did not retain predecessor group head: migrated=%t err=%v", migrated, err)
	}
	configPath := pueue.PrivateConfigPath(fixture.root)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	privateBase := filepath.Dir(configPath)
	oldSocket := strconv.Quote(filepath.Join(privateBase, "run", "pueue.sock"))
	newSocket := strconv.Quote(filepath.Join(privateBase, "run", "successor.sock"))
	changedConfig := strings.Replace(string(config), "unix_socket_path: "+oldSocket, "unix_socket_path: "+newSocket, 1)
	if changedConfig == string(config) {
		t.Fatal("private supervisor fixture did not contain the predecessor socket")
	}
	if err = os.WriteFile(configPath, []byte(changedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	var successor *pueue.Client
	err = pueue.WithBootstrapLock(t.Context(), fixture.root, func() error {
		var prepareErr error
		successor, prepareErr = pueue.PreparePrivateForMigrationWithLock(t.Context(), fixture.clientB, fixture.daemonB, fixture.root, fixture.options)
		return prepareErr
	})
	if err != nil {
		t.Fatalf("prepare successor supervisor: %v", err)
	}
	fixture.current = successor
	fixture.successor = successor.Binding()
	successorGroup := legacyGroup
	successorGroup.Supervisor = fixture.successor
	return &historicalInspectionMigrationScenario{
		fixture:        fixture,
		legacyDir:      legacyDir,
		configPath:     configPath,
		config:         config,
		changedConfig:  []byte(changedConfig),
		legacyGroup:    legacyGroup,
		successorGroup: successorGroup,
	}
}

func injectHistoricalInspectionMigrationCrash(t *testing.T, scenario *historicalInspectionMigrationScenario) {
	t.Helper()
	fixture := scenario.fixture
	crash := errors.New("simulated process exit before group completion")
	err := pueue.WithBootstrapLock(t.Context(), fixture.root, func() error {
		if prepareErr := inspection.PrepareGroupMigration(t.Context(), fixture.store, scenario.legacyGroup, scenario.successorGroup); prepareErr != nil {
			return prepareErr
		}
		if publishErr := pueue.PublishPrivateSupervisorBindingIfAbsent(fixture.root, fixture.predecessor); publishErr != nil {
			return publishErr
		}
		if migrateErr := fixture.current.MigratePrivateSupervisorWithLock(t.Context(), fixture.root); migrateErr != nil {
			return migrateErr
		}
		// Recreate the exact crash window after successor publication and
		// migration-journal cleanup but before recovery-alias retirement.
		if writeErr := os.WriteFile(scenario.configPath, scenario.config, 0o600); writeErr != nil {
			return writeErr
		}
		recoveryErr := fixture.predecessorClient.PersistPrivateRecoveryBinding(fixture.root)
		restoreErr := os.WriteFile(scenario.configPath, scenario.changedConfig, 0o600)
		if recoveryErr != nil {
			return recoveryErr
		}
		if restoreErr != nil {
			return restoreErr
		}
		return crash
	})
	if !errors.Is(err, crash) {
		t.Fatalf("crash injection did not reach the successor-published boundary: %v", err)
	}
	if complete, completeErr := inspection.GroupMigrationComplete(t.Context(), fixture.store, scenario.legacyGroup, scenario.successorGroup); completeErr != nil || complete {
		t.Fatalf("injected crash unexpectedly completed group migration: complete=%t err=%v", complete, completeErr)
	}
}

func assertHistoricalInspectionMigrationCrashState(t *testing.T, scenario *historicalInspectionMigrationScenario) {
	t.Helper()
	fixture := scenario.fixture
	var published, complete bool
	err := pueue.WithBootstrapLock(t.Context(), fixture.root, func() error {
		var checkErr error
		published, checkErr = pueue.PrivateSupervisorMigrationEdgePublishedWithLock(t.Context(), fixture.root, fixture.predecessor, fixture.successor)
		if checkErr != nil {
			return checkErr
		}
		complete, checkErr = pueue.PrivateSupervisorMigrationEdgeCompleteWithLock(t.Context(), fixture.root, fixture.predecessor, fixture.successor)
		return checkErr
	})
	if err != nil || !published || complete {
		t.Fatalf("crash fixture does not preserve published A-to-B edge with pending cleanup: published=%t complete=%t err=%v", published, complete, err)
	}
	persistedLegacy, err := inspection.ReadGroupRequest(t.Context(), fixture.store)
	if err != nil || !reflect.DeepEqual(persistedLegacy, scenario.legacyGroup) {
		t.Fatalf("private migration changed the immutable group source: persisted=%+v expected=%+v err=%v", persistedLegacy, scenario.legacyGroup, err)
	}
}

func completeHistoricalInspectionMigration(t *testing.T, scenario *historicalInspectionMigrationScenario) {
	t.Helper()
	fixture := scenario.fixture
	if err := os.RemoveAll(scenario.legacyDir); err != nil {
		t.Fatal(err)
	}
	resetHistoricalSupervisorCommandLog(t, fixture)

	err := pueue.WithBootstrapLock(t.Context(), fixture.root, func() error {
		return supervisorcore.MigrateInspectionGroup(t.Context(), fixture.store, fixture.current, pueue.DefaultObservationTimeout, scenario.legacyGroup)
	})
	if err != nil {
		t.Fatalf("retry did not complete the exact published migration: %v", err)
	}
	assertHistoricalInspectionMigrationComplete(t, scenario)
	if err := pueue.WithBootstrapLock(t.Context(), fixture.root, func() error {
		return supervisorcore.MigrateInspectionGroup(t.Context(), fixture.store, fixture.current, pueue.DefaultObservationTimeout, scenario.legacyGroup)
	}); err != nil {
		t.Fatalf("replaying group completion was not idempotent: %v", err)
	}
}

func assertHistoricalInspectionMigrationComplete(t *testing.T, scenario *historicalInspectionMigrationScenario) {
	t.Helper()
	fixture := scenario.fixture
	if complete, completeErr := inspection.GroupMigrationComplete(t.Context(), fixture.store, scenario.legacyGroup, scenario.successorGroup); completeErr != nil || !complete {
		t.Fatalf("retry did not publish group completion: complete=%t err=%v", complete, completeErr)
	}
	head, migrated, err := inspection.ReadGroupHead(t.Context(), fixture.store)
	if err != nil || !migrated || !reflect.DeepEqual(head, scenario.successorGroup) {
		t.Fatalf("group head did not advance to exact successor: head=%+v migrated=%t err=%v", head, migrated, err)
	}
	if present, presentErr := pueue.PrivateRecoveryBindingPresent(fixture.root); presentErr != nil || present {
		t.Fatalf("retry did not retire the predecessor recovery alias: present=%t err=%v", present, presentErr)
	}
	if complete, completeErr := pueue.PrivateSupervisorMigrationComplete(fixture.root, fixture.successor); completeErr != nil || !complete {
		t.Fatalf("retry did not finish private successor cleanup: complete=%t err=%v", complete, completeErr)
	}
}

func TestEnsureInspectionGroupRetiresCompletedRecoveryAlias(t *testing.T) {
	fixture := newHistoricalSupervisorFixture(t, false)
	prepareHistoricalInspectionRecoveryAlias(t, fixture)
	assertHistoricalInspectionRecoveryAliasPending(t, fixture)
	reconcileHistoricalInspectionGroup(t, fixture)
	assertHistoricalInspectionRecoveryAliasRetired(t, fixture)
}

func prepareHistoricalInspectionRecoveryAlias(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	store := fixture.store
	inspectionPredecessor := historicalInspectionPredecessor(fixture)
	legacyGroup := historicalInspectionGroupRequest(store.RootID, inspectionPredecessor)
	openHistoricalInspectionGroup(t, store, inspectionPredecessor)
	successor := legacyGroup
	successor.Supervisor = fixture.successor
	publishAppTestGroupMigration(t, store, legacyGroup, successor)
	if err := pueue.WithBootstrapLock(t.Context(), store.Root, func() error {
		return fixture.predecessorClient.PersistPrivateRecoveryBinding(store.Root)
	}); err != nil {
		t.Fatal(err)
	}
	status := []byte(`{"tasks":{},"groups":{"delegation-inspection-` + store.RootID + `":{"status":"Running","parallel_tasks":1}}}`)
	if err := os.WriteFile(fixture.statusPath, status, 0o600); err != nil {
		t.Fatal(err)
	}
}

func historicalInspectionPredecessor(fixture *historicalSupervisorFixture) task.SupervisorRef {
	inspectionPredecessor := fixture.predecessor
	inspectionPredecessor.Endpoint = "unix:" + filepath.Join(fixture.store.Root, ".supervisor", "run", "inspection-predecessor.sock")
	return inspectionPredecessor
}

func historicalInspectionGroupRequest(rootID string, supervisor task.SupervisorRef) inspection.GroupRequestRecord {
	return inspection.GroupRequestRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        rootID,
		Supervisor:    supervisor,
		Name:          (pueue.InspectionIdentity{RootID: rootID}).Group(),
		ParallelTasks: 1,
	}
}

func openHistoricalInspectionGroup(t *testing.T, store *taskdir.Store, supervisor task.SupervisorRef) {
	t.Helper()
	group, err := inspection.OpenGroup(store, supervisor)
	if err != nil {
		t.Fatal(err)
	}
	if err = group.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertHistoricalInspectionRecoveryAliasPending(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	store := fixture.store
	if complete, completeErr := pueue.PrivateSupervisorMigrationComplete(store.Root, fixture.successor); completeErr != nil || complete {
		t.Fatalf("successor unexpectedly complete before predecessor alias retirement: complete=%t err=%v", complete, completeErr)
	}
	if recovered, found, readErr := pueue.ReadPrivateRecoveryBinding(store.Root); readErr != nil || !found || recovered != fixture.predecessor {
		t.Fatalf("recovery alias does not preserve predecessor A: found=%t binding=%+v err=%v", found, recovered, readErr)
	}
	if present, presentErr := pueue.PrivateRecoveryBindingPresent(store.Root); presentErr != nil || !present {
		t.Fatalf("completed migration recovery alias was not created: present=%v err=%v", present, presentErr)
	}
}

func reconcileHistoricalInspectionGroup(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	// This is successful reconciliation, not a deadline fault. Use the same
	// bounded admission allowance as the production inspection path.
	if err := supervisorcore.EnsureInspectionGroupWithMigration(fixture.store, fixture.current, inspection.AdmissionTimeout, true); err != nil {
		t.Fatalf("normal inspection bootstrap did not reconcile the completed migration: %v", err)
	}
}

func assertHistoricalInspectionRecoveryAliasRetired(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	store := fixture.store
	if present, presentErr := pueue.PrivateRecoveryBindingPresent(store.Root); presentErr != nil || present {
		t.Fatalf("completed migration recovery alias remained: present=%v err=%v", present, presentErr)
	}
	if complete, completeErr := pueue.PrivateSupervisorMigrationComplete(store.Root, fixture.successor); completeErr != nil || !complete {
		t.Fatalf("successor did not become complete after predecessor alias retirement: complete=%t err=%v", complete, completeErr)
	}
}

func TestMigrateInspectionGroupRetiresOlderRecoveryAlias(t *testing.T) {
	store := newBareInspectionStore(t)
	legacySupervisor, logPath, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	privateBase := filepath.Join(store.Root, ".supervisor")
	shared := map[string]any{
		"host":                    "127.0.0.1",
		"port":                    "6924",
		"pueue_directory":         filepath.Join(privateBase, "data"),
		"runtime_directory":       filepath.Join(privateBase, "run"),
		"use_unix_socket":         true,
		"unix_socket_path":        filepath.Join(privateBase, "run", "pueue.sock"),
		"unix_socket_permissions": 448,
		"alias_file":              filepath.Join(privateBase, "aliases.yml"),
		"pid_path":                filepath.Join(privateBase, "run", "pueue.pid"),
		"shared_secret_path":      filepath.Join(privateBase, "data", "shared_secret"),
		"daemon_cert":             filepath.Join(privateBase, "data", "certs", "daemon.cert"),
		"daemon_key":              filepath.Join(privateBase, "data", "certs", "daemon.key"),
	}
	config, err := json.Marshal(map[string]any{
		"shared": shared,
		"daemon": map[string]any{"shell_command": []string{"/bin/sh", "-c", "{{ pueue_command_string }}"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pueue.PrivateConfigPath(store.Root), config, 0o600); err != nil {
		t.Fatal(err)
	}
	environment := append(
		os.Environ(),
		"FAKE_LOG="+logPath,
		"FAKE_STATUS="+filepath.Join(filepath.Dir(logPath), "status.json"),
		"FAKE_AFTER="+filepath.Join(filepath.Dir(logPath), "status-after.json"),
		"FAKE_DONE="+filepath.Join(filepath.Dir(logPath), "group-done"),
	)
	legacySupervisor, err = pueue.Bind(context.Background(), legacySupervisor.Binding().ClientExecutable, pueue.PrivateConfigPath(store.Root), pueue.Options{Environment: environment, ObservationTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	legacy := inspection.GroupRequestRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		Supervisor:    legacySupervisor.Binding(),
		Name:          (pueue.InspectionIdentity{RootID: store.RootID}).Group(),
		ParallelTasks: 1,
	}
	group, err := inspection.OpenGroup(store, legacy.Supervisor)
	if err != nil {
		t.Fatal(err)
	}
	if err = group.Close(); err != nil {
		t.Fatal(err)
	}
	successor := legacy
	successor.Supervisor.Endpoint += ".successor"
	publishAppTestGroupMigration(t, store, legacy, successor)
	if err = pueue.WithBootstrapLock(t.Context(), store.Root, func() error {
		if persistErr := legacySupervisor.PersistPrivateRecoveryBinding(store.Root); persistErr != nil {
			return persistErr
		}
		return supervisorcore.ReconcileStaleInspectionRecovery(t.Context(), store, successor)
	}); err != nil {
		t.Fatalf("stale published predecessor alias was not reconciled: %v", err)
	}
	if present, presentErr := pueue.PrivateRecoveryBindingPresent(store.Root); presentErr != nil || present {
		t.Fatalf("stale predecessor recovery alias remained: present=%v err=%v", present, presentErr)
	}
}

func TestRejectExternalInspectionMigrationRejectsExternalSuccessor(t *testing.T) {
	store := newBareInspectionStore(t)
	privateSupervisor, _, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	group, err := inspection.OpenGroup(store, privateSupervisor.Binding())
	if err != nil {
		t.Fatal(err)
	}
	if err = group.Close(); err != nil {
		t.Fatal(err)
	}
	externalSupervisor, _, _ := newUncertainGroupSupervisor(t, store.RootID)
	err = supervisorcore.RejectExternalInspectionMigration(context.Background(), store, externalSupervisor)
	if !errors.Is(err, inspection.ErrGroupMigrationUnsupported) {
		t.Fatalf("external successor was accepted for a private inspection group: %v", err)
	}
}

func TestPrivateAdmissionRejectsLogicalMigrationBeforeStartingSuccessor(t *testing.T) {
	store := newBareInspectionStore(t)
	current, _, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	legacy := current.Binding()
	legacy.Endpoint = "unix:/legacy-supervisor.sock"
	group, err := inspection.OpenGroup(store, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err = group.Close(); err != nil {
		t.Fatal(err)
	}
	err = rejectPrivateAdmissionMigration(context.Background(), store, current, true)
	if !errors.Is(err, inspection.ErrGroupMigrationRequired) {
		t.Fatalf("private logical drift was not blocked before startup: %v", err)
	}
}

func TestPrivateAdmissionRejectsUnreachableNoJournalSupervisorWithIdentity(t *testing.T) {
	store := newBareInspectionStore(t)
	current, logPath, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	if err := os.Remove(filepath.Join(filepath.Dir(logPath), "status.json")); err != nil {
		t.Fatal(err)
	}
	digest, err := commonprovider.FingerprintExecutable("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := json.Marshal(map[string]string{"executable": "/usr/bin/true", "sha256": digest})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, ".supervisor", "daemon.identity.json"), identity, 0o600); err != nil {
		t.Fatal(err)
	}
	err = rejectPrivateAdmissionMigration(context.Background(), store, current, true)
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unreachable no-journal supervisor was allowed to start: %v", err)
	}
}

func TestPrivateAdmissionRejectsUnreachableNoJournalSupervisorWithoutIdentity(t *testing.T) {
	store := newBareInspectionStore(t)
	current, logPath, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	if err := os.Remove(filepath.Join(filepath.Dir(logPath), "status.json")); err != nil {
		t.Fatal(err)
	}
	err := rejectPrivateAdmissionMigration(context.Background(), store, current, true)
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unreachable pre-existing supervisor without identity was allowed to start: %v", err)
	}
}

func TestPrivateAdmissionAllowsFreshUnreachableSupervisorWithoutJournal(t *testing.T) {
	store := newBareInspectionStore(t)
	current, logPath, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	if err := os.Remove(filepath.Join(filepath.Dir(logPath), "status.json")); err != nil {
		t.Fatal(err)
	}
	if err := rejectPrivateAdmissionMigration(context.Background(), store, current, false); err != nil {
		t.Fatalf("fresh supervisor was incorrectly blocked before its first start: %v", err)
	}
}

func TestPrivateAdmissionAllowsIncompleteConfigOnlyBootstrap(t *testing.T) {
	store := newBareInspectionStore(t)
	current, logPath, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	if err := os.Remove(filepath.Join(filepath.Dir(logPath), "status.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Root, ".supervisor", "bootstrap.pending"), []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectPrivateAdmissionMigration(context.Background(), store, current, true); err != nil {
		t.Fatalf("incomplete private bootstrap was treated as an existing predecessor: %v", err)
	}
}
