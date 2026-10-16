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

func TestRunAdmissionInspectionReplaysRecordedFactsAfterDeadline(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, _, _ := newUncertainGroupSupervisor(t, store.RootID)
	createdAt := time.Now().UTC().Add(-inspection.AdmissionTimeout - time.Second)
	operation := newInspectionPollTestOperationAt(t, store, bound, createdAt)
	completeInspectionPollTestOperation(t, operation)
	if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	rewriteInspectionPollTestTerminationTime(t, store, operation, createdAt.Add(4*time.Millisecond))
	if err := operation.RecordWorkerSuccessContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}

	facts, deadline, err := runAdmissionInspection(operation, nil, "", "")
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("expired replay discarded validated admission facts: facts=%s deadline=%v err=%v", facts, deadline, err)
	}
	if !operation.Expired(time.Now()) {
		t.Fatal("replay fixture was not expired")
	}
}

func TestRunAdmissionInspectionReplaysHistoricalRecordedFactsAfterDeadline(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, _, _ := newUncertainGroupSupervisor(t, store.RootID)
	createdAt := time.Now().UTC().Add(-inspection.AdmissionTimeout - time.Second)
	operation := newInspectionPollTestOperationAt(t, store, bound, createdAt)
	completeInspectionPollTestOperation(t, operation)
	if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordWorkerSuccessContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}

	facts, deadline, err := runAdmissionInspection(operation, nil, "", "")
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("historical replay discarded validated admission facts: facts=%s deadline=%v err=%v", facts, deadline, err)
	}
	if !operation.Expired(time.Now()) {
		t.Fatal("historical replay fixture was not expired")
	}
}

func TestRunAdmissionInspectionInitialReadUsesOperationDeadline(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, _, _ := newUncertainGroupSupervisor(t, store.RootID)
	createdAt := time.Now().UTC().Add(-inspection.AdmissionTimeout + 2*time.Second)
	operation := newInspectionPollTestOperationAt(t, store, bound, createdAt)

	control, err := store.OpenInspection(operation.Request().TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- control.WithLock(context.Background(), func(_ *taskdir.ControlTransaction) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	result := make(chan error, 1)
	go func() {
		_, _, runErr := runAdmissionInspection(operation, nil, "", "")
		result <- runErr
	}()
	select {
	case runErr := <-result:
		if !errors.Is(runErr, inspection.ErrAdmissionExpired) {
			t.Fatalf("initial evidence read ignored deadline error: %v", runErr)
		}
	case <-time.After(3 * time.Second):
		close(release)
		<-lockDone
		t.Fatal("initial evidence read outlived the operation deadline")
	}
	close(release)
	if err = <-lockDone; err != nil {
		t.Fatal(err)
	}
}

func TestValidateLegacyEndedInspectionLifetimeAcceptsHistoricalSuccess(t *testing.T) {
	_, operation, job := newCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	if err := operation.RecordWorkerSuccessContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}

	if err := validateLegacyEndedInspectionLifetime(t.Context(), operation, operation.Request().TaskID, job); err != nil {
		t.Fatalf("historical ended inspection was rejected during migration: %v", err)
	}
}

func TestInspectionPollingStopsWhenSupervisorCommandIsInFlight(t *testing.T) {
	pending := &pueue.Pending{}
	if !shouldStopInspectionPolling(&pueue.InFlightError{Pending: pending}) {
		t.Fatal("in-flight supervisor command remained pollable")
	}
	if shouldStopInspectionPolling(&pueue.InFlightError{}) {
		t.Fatal("in-flight error without a retained command stopped polling")
	}
	if shouldStopInspectionPolling(pueue.ErrUnknown) {
		t.Fatal("ordinary observation uncertainty stopped polling")
	}
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
	err = ensureInspectionGroupWithMigration(store, current, time.Second, true)
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
	if err = ensureInspectionGroupWithMigration(store, current, 15*time.Second, false); err != nil {
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
		return migrateInspectionGroup(t.Context(), fixture.store, fixture.current, pueue.DefaultObservationTimeout, scenario.legacyGroup)
	})
	if err != nil {
		t.Fatalf("retry did not complete the exact published migration: %v", err)
	}
	assertHistoricalInspectionMigrationComplete(t, scenario)
	if err := pueue.WithBootstrapLock(t.Context(), fixture.root, func() error {
		return migrateInspectionGroup(t.Context(), fixture.store, fixture.current, pueue.DefaultObservationTimeout, scenario.legacyGroup)
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
	if err := ensureInspectionGroupWithMigration(fixture.store, fixture.current, inspection.AdmissionTimeout, true); err != nil {
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
		return reconcileStaleInspectionRecovery(t.Context(), store, successor)
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
	err = rejectExternalInspectionMigration(context.Background(), store, externalSupervisor)
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

func TestSupervisorOptionsForCandidateKeepOrdinaryTimeout(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := commonprovider.FingerprintExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	candidate := commonprovider.ProfileCandidate{Inspection: &commonprovider.InspectionDefinition{
		Revision: commonprovider.RuntimeInspectionRevision, Executable: executable,
		ExecutableSHA256: digest, Arguments: []string{"--version"}, Directory: workspace,
		Environment: []string{}, OutputLimit: commonprovider.MaxInspectionOutput,
		Project: func([]byte) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	}}
	options, err := supervisorOptionsForCandidate(pueue.Options{ObservationTimeout: time.Millisecond}, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if options.ObservationTimeout != time.Millisecond {
		t.Fatalf("ordinary observation timeout=%s, want %s", options.ObservationTimeout, time.Millisecond)
	}
	longer, err := supervisorOptionsForCandidate(pueue.Options{ObservationTimeout: time.Minute}, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if longer.ObservationTimeout != time.Minute {
		t.Fatalf("explicit longer observation timeout=%s was reduced", longer.ObservationTimeout)
	}
	if _, err = supervisorOptionsForCandidate(pueue.Options{ObservationTimeout: -time.Second}, candidate); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("negative observation timeout was accepted: %v", err)
	}
}

func TestRuntimeInspectionBindingKeepsCapabilityContractAcrossIdentityRefresh(t *testing.T) {
	base := inspection.Binding{DefinitionRevision: commonprovider.RuntimeInspectionRevision, CapabilitySHA256: strings.Repeat("a", 64), Environment: []string{"LANG=C"}, EnvironmentRecorded: true}
	changed := base
	changed.CapabilitySHA256 = strings.Repeat("b", 64)
	if sameInspectionBindingBase(base, changed) {
		t.Fatal("runtime capability contract drift was accepted")
	}
	identityRefresh := base
	identityRefresh.DefinitionSHA256 = strings.Repeat("c", 64)
	if !sameInspectionBindingBase(base, identityRefresh) {
		t.Fatal("runtime executable identity refresh was rejected")
	}
	legacy := base
	legacy.CapabilitySHA256 = ""
	if !sameInspectionBindingBase(base, legacy) {
		t.Fatal("legacy runtime binding was not left for a fresh capability probe")
	}
	legacyChanged := legacy
	legacyChanged.DefinitionSHA256 = strings.Repeat("d", 64)
	if sameInspectionBindingBase(base, legacyChanged) {
		t.Fatal("markerless runtime binding relaxed executable identity")
	}
	legacyNonEmpty := base
	legacyNonEmpty.EnvironmentRecorded = false
	if !sameInspectionBindingBase(base, legacyNonEmpty) {
		t.Fatal("legacy non-empty environment binding was not normalized as recorded")
	}
	explicitEmpty := base
	explicitEmpty.Environment = []string{}
	explicitEmpty.EnvironmentRecorded = true
	legacyEmpty := explicitEmpty
	legacyEmpty.EnvironmentRecorded = false
	if sameInspectionBindingBase(explicitEmpty, legacyEmpty) {
		t.Fatal("inherited and explicit-empty environment bindings were treated as equivalent")
	}
	mixed := inspection.Binding{
		DefinitionRevision: commonprovider.RuntimeInspectionRevision,
		DefinitionSHA256:   strings.Repeat("a", 64),
		HelperExecutable:   "/usr/bin/security",
		HelperSHA256:       strings.Repeat("c", 64),
		Environment:        []string{"LANG=C"},
	}
	mixedChanged := mixed
	mixedChanged.DefinitionSHA256 = strings.Repeat("b", 64)
	mixedChanged.HelperExecutable = "/usr/bin/other-helper"
	if sameInspectionBindingBase(mixed, mixedChanged) {
		t.Fatal("mixed native/runtime binding relaxed its native contract")
	}
}

func TestAdmissionRejectsCompletedRuntimeProofAfterIdentityDrift(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	supervisor, _ := newBlockingInspectionSupervisor(t, store.RootID)
	oldBinding := inspection.Binding{
		DefinitionRevision: commonprovider.RuntimeInspectionRevision,
		DefinitionSHA256:   strings.Repeat("a", 64),
		CapabilitySHA256:   strings.Repeat("b", 64),
		HelperExecutable:   "/usr/bin/true",
		HelperSHA256:       strings.Repeat("c", 64),
		WorkerExecutable:   "/usr/bin/true",
		WorkerSHA256:       strings.Repeat("d", 64),
		Environment:        []string{"LANG=C"},
		Supervisor:         supervisor.Binding(),
	}
	openedAt := time.Now().UTC()
	operation, err := inspection.OpenOperation(store, request, oldBinding, openedAt)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := operation.ClaimSubmissionContext(context.Background(), openedAt.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = submission.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	start, err := operation.ClaimStart(openedAt.Add(2 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = operation.Complete(inspection.ResultEligible, []byte(`{"eligible":true}`), openedAt.Add(3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(17); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordWorkerSuccess(17); err != nil {
		t.Fatal(err)
	}
	currentBinding := oldBinding
	currentBinding.DefinitionSHA256 = strings.Repeat("e", 64)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = rejectCompletedRuntimeIdentityDrift(canceled, operation, oldBinding, currentBinding); !errors.Is(err, context.Canceled) {
		t.Fatalf("runtime drift check discarded caller cancellation: %v", err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	replayed, err := openAdmissionInspectionOperation(store, request, currentBinding, time.Now().UTC())
	if replayed != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("completed runtime proof crossed identity drift: operation=%v err=%v", replayed, err)
	}
}

func TestAdmissionReplayReprobesIncompleteMarkerlessRuntimeAfterRefresh(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	supervisor, _ := newBlockingInspectionSupervisor(t, store.RootID)
	oldBinding := inspection.Binding{
		DefinitionRevision: commonprovider.RuntimeInspectionRevision,
		DefinitionSHA256:   strings.Repeat("a", 64),
		HelperExecutable:   "/usr/bin/provider-old",
		HelperSHA256:       strings.Repeat("b", 64),
		WorkerExecutable:   "/usr/bin/true",
		WorkerSHA256:       strings.Repeat("c", 64),
		Environment:        []string{"LANG=C"},
		Supervisor:         supervisor.Binding(),
	}
	operation, err := inspection.OpenOperation(store, request, oldBinding, time.Unix(100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	currentBinding := oldBinding
	currentBinding.DefinitionSHA256 = strings.Repeat("d", 64)
	currentBinding.CapabilitySHA256 = strings.Repeat("e", 64)
	currentBinding.HelperExecutable = "/usr/bin/provider-new"
	currentBinding.HelperSHA256 = strings.Repeat("f", 64)
	currentBinding.RunnerOwnership = task.RunnerOwnershipManaged
	currentBinding.EnvironmentRecorded = true
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	allowed, replayErr := allowIncompleteMarkerlessRuntimeReplay(canceled, operation, oldBinding, currentBinding)
	if allowed || !errors.Is(replayErr, context.Canceled) {
		t.Fatalf("markerless replay discarded caller cancellation: allowed=%v err=%v", allowed, replayErr)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	replayed, err := openAdmissionInspectionOperation(store, request, currentBinding, time.Unix(101, 0).UTC())
	if err != nil {
		t.Fatalf("incomplete markerless runtime journal was not reopened for re-probe: %v", err)
	}
	defer func() {
		if closeErr := replayed.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if got := replayed.Request().Binding; !reflect.DeepEqual(got, oldBinding) {
		t.Fatalf("replay mutated immutable markerless binding: got=%+v want=%+v", got, oldBinding)
	}
}

func TestEnsureInspectionGroupRetainsUncertainCreate(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, logPath, donePath := newUncertainGroupSupervisor(t, store.RootID)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{}, 1)
	client := newObservedInspectionTestClient(t, bound, logPath, 10*time.Second, func(event pueue.CommandEvent) {
		if event.Stage == "started" && strings.Contains("\x00"+strings.Join(event.Argv, "\x00")+"\x00", "\x00group\x00") {
			select {
			case started <- struct{}{}:
			default:
			}
			cancel()
		}
	}, "FAKE_AFTER="+filepath.Join(filepath.Dir(logPath), "status-after.json"), "FAKE_DONE="+donePath)

	err := ensureInspectionGroupWithMigrationContext(ctx, store, client, inspection.AdmissionTimeout, false)
	pending := assertRetainedInspectionPending(t, err)
	settled := false
	t.Cleanup(func() {
		if !settled {
			drainInspectionPending(t, pending)
		}
	})
	select {
	case <-started:
	default:
		t.Fatal("group command did not start before the caller stopped observing")
	}
	drainInspectionPending(t, pending)
	settled = true
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if _, statErr := os.Stat(donePath); statErr != nil {
		t.Fatalf("uncertain create did not settle naturally: %v; log=%s", statErr, logData)
	}
	if count := strings.Count(string(logData), "arg=group\n"); count != 1 {
		t.Fatalf("group creation was launched %d times: err=%v log=%s", count, err, logData)
	}
	if count := strings.Count(string(logData), "arg=status\n"); count != 1 {
		t.Fatalf("unfinished creation caused %d status commands", count)
	}
	if err = ensureInspectionGroup(store, client, inspection.AdmissionTimeout); err != nil {
		t.Fatalf("settled uncertain creation was not observed by the next caller: %v", err)
	}
	logData, readErr = os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if count := strings.Count(string(logData), "arg=group\n"); count != 1 {
		t.Fatalf("settled creation was retried %d times: %s", count, logData)
	}
	if count := strings.Count(string(logData), "arg=status\n"); count != 2 {
		t.Fatalf("next caller did not take one fresh snapshot: %d: %s", count, logData)
	}
}

func TestStopLegacyInspectionSupervisorPreservesDispatchedOwnership(t *testing.T) {
	client, logPath, _ := newUncertainGroupSupervisor(t, "shutdown-ownership")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	dispatched, err := client.ShutdownDispatched(ctx)
	if !dispatched {
		t.Fatalf("shutdown ownership was lost after dispatch: err=%v", err)
	}
	if err == nil {
		t.Fatal("timed-out shutdown was reported as successful")
	}
	waitForTestPath(t, filepath.Join(filepath.Dir(logPath), "shutdown-done"))
	invocations, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if count := strings.Count(string(invocations), "arg=shutdown\n"); count != 1 {
		t.Fatalf("shutdown command was dispatched %d times after ownership transferred: %s", count, invocations)
	}
}

func waitForTestPath(t *testing.T, path string) {
	waitForTestPathWithin(t, path, pueue.DefaultObservationTimeout)
}

func waitForTestPathWithin(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", path)
		case <-ticker.C:
		}
	}
}

func TestAwaitInspectionRetainsInFlightSupervisorObservation(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	request.TaskID = strings.Repeat("b", 32)
	bound, logPath := newBlockingInspectionSupervisor(t, store.RootID)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := newObservedInspectionTestClient(t, bound, logPath, 10*time.Second, func(event pueue.CommandEvent) {
		if event.Stage == "started" && strings.Contains("\x00"+strings.Join(event.Argv, "\x00")+"\x00", "\x00status\x00") {
			cancel()
		}
	})
	digest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: "/usr/bin/true", WorkerSHA256: digest,
		Supervisor: client.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	facts, err := awaitInspection(ctx, operation, client)
	pending := assertRetainedInspectionPending(t, err)
	if facts != nil || !errors.Is(err, pueue.ErrInFlight) {
		t.Fatalf("in-flight observation result facts=%s err=%v", facts, err)
	}
	drainInspectionPending(t, pending)
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 1 {
		t.Fatalf("status command was launched %d times while first remained in flight: %s", count, invocations)
	}
}

func TestSubmitInspectionOnceRetainsInFlightSupervisorCommand(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	request.TaskID = strings.Repeat("c", 32)
	bound, logPath := newBlockingInspectionSupervisor(t, store.RootID)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := newObservedInspectionTestClient(t, bound, logPath, 10*time.Second, func(event pueue.CommandEvent) {
		if event.Stage == "started" && strings.Contains("\x00"+strings.Join(event.Argv, "\x00")+"\x00", "\x00status\x00") {
			cancel()
		}
	})
	runner, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	workerSHA, err := commonprovider.FingerprintExecutable(runner)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: runner, WorkerSHA256: workerSHA,
		Supervisor: client.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	err = submitInspectionOnce(ctx, operation, client, runner, store.Root)
	pending := assertRetainedInspectionPending(t, err)
	if !errors.Is(err, pueue.ErrInFlight) {
		t.Fatalf("in-flight submission returned %v", err)
	}
	drainInspectionPending(t, pending)
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 1 {
		t.Fatalf("status command was launched %d times while submission remained in flight: %s", count, invocations)
	}
}

func TestInspectionLaunchAlwaysUsesGuardian(t *testing.T) {
	launch := guardedInspectionLaunch("/tmp/custom-delegate-run", "/tmp/delegate-state")
	if launch.RunnerExecutable != "/tmp/custom-delegate-run" || launch.RootPath != "/tmp/delegate-state" || !launch.Supervised {
		t.Fatalf("inspection launch is not guardian supervised: %+v", launch)
	}
}

func TestSubmitInspectionOnceRejectsWorkerChangedAfterClaim(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	request.TaskID = strings.Repeat("d", 32)
	client, logPath := newBlockingInspectionSupervisor(t, store.RootID)
	runner, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := commonprovider.FingerprintExecutable(runner)
	if err != nil {
		t.Fatal(err)
	}
	definitionDigest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: definitionDigest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: definitionDigest,
		WorkerExecutable: runner, WorkerSHA256: digest,
		Supervisor: client.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	changedRunner := filepath.Join(t.TempDir(), "changed-worker")
	if err = os.WriteFile(changedRunner, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	changedRunner, err = filepath.EvalSymlinks(changedRunner)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = submitInspectionOnce(ctx, operation, client, changedRunner, store.Root)
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("changed worker reached supervisor boundary: %v", err)
	}
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 0 {
		t.Fatalf("changed worker launched supervisor observation %d times: %s", count, invocations)
	}
}

func TestValidateInspectionRunnerRejectsDigestDrift(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	request.TaskID = strings.Repeat("e", 32)
	client, logPath := newBlockingInspectionSupervisor(t, store.RootID)
	runner, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: runner, WorkerSHA256: digest,
		Supervisor: client.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = validateInspectionRunner(operation, runner); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("worker digest drift was accepted: %v", err)
	}
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 0 {
		t.Fatalf("worker digest check launched supervisor observation %d times: %s", count, invocations)
	}
}

func assertRetainedInspectionPending(t *testing.T, err error) *pueue.Pending {
	t.Helper()
	var inFlight *pueue.InFlightError
	if !errors.As(err, &inFlight) || inFlight == nil || inFlight.Pending == nil {
		t.Fatalf("missing retained in-flight supervisor command: %v", err)
	}
	select {
	case <-inFlight.Pending.Done():
		t.Fatal("returned only after the in-flight supervisor command completed")
	default:
	}
	return inFlight.Pending
}

func drainInspectionPending(t *testing.T, pending *pueue.Pending) {
	t.Helper()
	if pending == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-pending.Done():
	case <-ctx.Done():
		t.Fatalf("timed out waiting for retained supervisor command: %v", ctx.Err())
	}
}

func newBlockingInspectionSupervisor(t *testing.T, rootID string) (*pueue.Client, string) {
	t.Helper()
	rawBase, err := os.MkdirTemp("/tmp", "dlp-app-inspection-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(rawBase); removeErr != nil {
			t.Error(removeErr)
		}
	})
	base, err := filepath.EvalSymlinks(rawBase)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(base, "pueue")
	configPath := filepath.Join(base, "pueue.json")
	statusPath := filepath.Join(base, "status.json")
	logPath := filepath.Join(base, "invocations.log")
	script := `#!/bin/sh
set -eu
{
  printf '%s\n' '---'
  for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
} >> "$FAKE_LOG"
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status) sleep 1; cat "$FAKE_STATUS" ;;
  *) exit 64 ;;
esac
`
	if writeErr := os.WriteFile(executable, []byte(script), 0o700); writeErr != nil {
		t.Fatal(writeErr)
	}
	shared := map[string]any{
		"pueue_directory":    filepath.Join(base, "data"),
		"runtime_directory":  filepath.Join(base, "run"),
		"unix_socket_path":   filepath.Join(base, "socket"),
		"alias_file":         filepath.Join(base, "aliases"),
		"pid_path":           filepath.Join(base, "pid"),
		"shared_secret_path": filepath.Join(base, "secret"),
		"daemon_cert":        filepath.Join(base, "cert"),
		"daemon_key":         filepath.Join(base, "key"),
	}
	config, err := json.Marshal(map[string]any{"shared": shared})
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(configPath, config, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	status, err := json.Marshal(map[string]any{
		"tasks":  map[string]any{},
		"groups": map[string]any{"delegation-inspection-" + rootID: map[string]any{"status": "Running", "parallel_tasks": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(statusPath, status, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	environment := append(os.Environ(), "FAKE_LOG="+logPath, "FAKE_STATUS="+statusPath)
	// The initial bind probes the fixture through /bin/sh. Keep that setup
	// budget at the production default so scheduler contention under -race
	// cannot turn shell startup into a false binding failure. The returned
	// client deliberately uses a shorter observation window below so the
	// operation still exercises retained in-flight status ownership.
	bound, err := pueue.Bind(context.Background(), executable, configPath, pueue.Options{Environment: environment, ObservationTimeout: pueue.DefaultObservationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	client, err := pueue.NewClient(bound.Binding(), pueue.Options{Environment: environment, ObservationTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return client, logPath
}

func newObservedInspectionTestClient(t *testing.T, bound *pueue.Client, logPath string, timeout time.Duration, observer func(pueue.CommandEvent), extraEnvironment ...string) *pueue.Client {
	t.Helper()
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	environment := append(os.Environ(), "FAKE_LOG="+logPath, "FAKE_STATUS="+statusPath)
	environment = append(environment, extraEnvironment...)
	client, err := pueue.NewClient(bound.Binding(), pueue.Options{
		Environment:        environment,
		ObservationTimeout: timeout,
		Observer:           observer,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client
}

func newUncertainGroupSupervisor(t *testing.T, rootID string) (*pueue.Client, string, string) {
	return newUncertainGroupSupervisorWithConfig(t, rootID, "")
}

func newUncertainGroupSupervisorWithConfig(t *testing.T, rootID, configuredPath string) (*pueue.Client, string, string) {
	return newUncertainGroupSupervisorWithConfigAndTimeout(t, rootID, configuredPath, 500*time.Millisecond)
}

func newUncertainGroupSupervisorWithConfigAndTimeout(t *testing.T, rootID, configuredPath string, observationTimeout time.Duration) (*pueue.Client, string, string) {
	t.Helper()
	rawBase, err := os.MkdirTemp("/tmp", "dlp-app-inspection-group-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(rawBase); removeErr != nil {
			t.Error(removeErr)
		}
	})
	base, err := filepath.EvalSymlinks(rawBase)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(base, "pueue")
	configPath := configuredPath
	if configPath == "" {
		configPath = filepath.Join(base, "pueue.json")
	} else if mkdirErr := os.MkdirAll(filepath.Dir(configPath), 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	statusPath := filepath.Join(base, "status.json")
	afterPath := filepath.Join(base, "status-after.json")
	donePath := filepath.Join(base, "group-done")
	logPath := filepath.Join(base, "invocations.log")
	script := `#!/bin/sh
set -eu
{
  printf '%s\n' '---'
  for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
} >> "$FAKE_LOG"
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status)
    if [ -n "${FAKE_STATUS_FAIL_ONCE:-}" ] && [ ! -e "$FAKE_STATUS_FAIL_ONCE" ]; then
      : > "$FAKE_STATUS_FAIL_ONCE"
      exit 23
    fi
    cat "$FAKE_STATUS"
    exit "${FAKE_STATUS_EXIT:-0}"
    ;;
  shutdown)
    sleep 1
    touch "$(dirname "$FAKE_LOG")/shutdown-done"
    exit 1
    ;;
  group)
    [ "${4-}" = add ] && [ "${5-}" = --parallel ] && [ "${6-}" = 1 ]
    sleep 1
    cp "$FAKE_AFTER" "$FAKE_STATUS"
    : > "$FAKE_DONE"
    ;;
  add)
    [ "${4-}" = --help ]
    printf '%s\n' '--stashed --print-task-id'
    ;;
  enqueue)
    [ "${4-}" = --help ]
    printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...'
    ;;
  *) exit 64 ;;
esac
`
	if writeErr := os.WriteFile(executable, []byte(script), 0o700); writeErr != nil {
		t.Fatal(writeErr)
	}
	shared := map[string]any{
		"pueue_directory":    filepath.Join(base, "data"),
		"runtime_directory":  filepath.Join(base, "run"),
		"unix_socket_path":   filepath.Join(base, "socket"),
		"alias_file":         filepath.Join(base, "aliases"),
		"pid_path":           filepath.Join(base, "pid"),
		"shared_secret_path": filepath.Join(base, "secret"),
		"daemon_cert":        filepath.Join(base, "cert"),
		"daemon_key":         filepath.Join(base, "key"),
	}
	config, err := json.Marshal(map[string]any{"shared": shared})
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(configPath, config, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if writeErr := os.WriteFile(statusPath, []byte(`{"tasks":{},"groups":{}}`), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	group := "delegation-inspection-" + rootID
	after := []byte(`{"tasks":{},"groups":{"` + group + `":{"status":"Running","parallel_tasks":1}}}`)
	if writeErr := os.WriteFile(afterPath, after, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	environment := append(os.Environ(), "FAKE_LOG="+logPath, "FAKE_STATUS="+statusPath, "FAKE_AFTER="+afterPath, "FAKE_DONE="+donePath)
	bound, err := pueue.Bind(context.Background(), executable, configPath, pueue.Options{Environment: environment, ObservationTimeout: pueue.DefaultObservationTimeout})
	if err != nil {
		t.Fatalf("bind uncertain-group supervisor fixture: %v", err)
	}
	client, err := pueue.NewClient(bound.Binding(), pueue.Options{Environment: environment, ObservationTimeout: observationTimeout})
	if err != nil {
		t.Fatal(err)
	}
	return client, logPath, donePath
}

func TestInspectionAdmissionRejectsStaticCandidateBeforeSupervisorBinding(t *testing.T) {
	tests := []struct {
		name      string
		candidate func(task.TaskRecord, *taskdir.Store) commonprovider.ProfileCandidate
	}{
		{
			name: "candidate directory mismatch",
			candidate: func(request task.TaskRecord, _ *taskdir.Store) commonprovider.ProfileCandidate {
				return appInvalidInspectionCandidate(request, filepath.Join(request.CanonicalCwd, "unexpected"), nil, nil)
			},
		},
		{
			name: "writable state overlap",
			candidate: func(request task.TaskRecord, store *taskdir.Store) commonprovider.ProfileCandidate {
				return appInvalidInspectionCandidate(request, request.CanonicalCwd, []string{store.Root}, nil)
			},
		},
		{
			name: "malformed inspection definition",
			candidate: func(request task.TaskRecord, _ *taskdir.Store) commonprovider.ProfileCandidate {
				definition := &commonprovider.InspectionDefinition{
					Revision:    "",
					Executable:  "relative-helper",
					Directory:   request.CanonicalCwd,
					Environment: []string{},
					OutputLimit: 1024,
					Project: func([]byte) (json.RawMessage, error) {
						return json.RawMessage(`{"eligible":true}`), nil
					},
				}
				return appInvalidInspectionCandidate(request, request.CanonicalCwd, nil, definition)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newBareInspectionStore(t)
			request := newBareInspectionRequest(t, store)
			candidate := test.candidate(request, store)
			deps := Dependencies{
				PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
					return candidate, nil
				},
				InitialSupervisorExecutable: filepath.Join(t.TempDir(), "missing-pueue"),
			}
			args := Arguments{PueueConfig: filepath.Join(t.TempDir(), "missing-pueue.yml")}
			if _, err := prepareAdmission(args, deps, store, request); !errors.Is(err, ErrProfileUnavailable) {
				t.Fatalf("static candidate crossed supervisor binding: %v", err)
			}
			assertAppInspectionNoOrdinaryTasks(t, store)
		})
	}
}

func appInvalidInspectionCandidate(request task.TaskRecord, directory string, writableRoots []string, definition *commonprovider.InspectionDefinition) commonprovider.ProfileCandidate {
	return commonprovider.ProfileCandidate{
		Directory:     directory,
		WritableRoots: writableRoots,
		Inspection:    definition,
		Finalize: func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
			return appInspectionPreparedProfile(request, directory, writableRoots), nil
		},
	}
}

func newBareInspectionStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(canonicalAppTestTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func publishAppTestGroupMigration(t *testing.T, store *taskdir.Store, legacy, successor inspection.GroupRequestRecord) {
	t.Helper()
	if err := inspection.PrepareGroupMigration(t.Context(), store, legacy, successor); err != nil {
		t.Fatal(err)
	}
	if err := inspection.CompleteGroupMigration(t.Context(), store, legacy, successor); err != nil {
		t.Fatal(err)
	}
}

func newBareInspectionRequest(t *testing.T, store *taskdir.Store) task.TaskRecord {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return task.TaskRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		TaskID:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Provider:      "fixture:test",
		Mode:          "read-only",
		CanonicalCwd:  workspace,
		RequestedConfig: task.TaskConfig{
			Permission: "read-only",
			Budget:     "1m0s",
		},
		BudgetNanos: int64(time.Minute),
		BriefSHA256: task.ComputeSHA256([]byte("inspection brief")),
		BriefLength: int64(len("inspection brief")),
	}
}
