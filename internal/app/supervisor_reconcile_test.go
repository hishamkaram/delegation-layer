package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestMigrationPreparesInstalledPair(t *testing.T) {
	owner := newComponentSupervisorFixture(t, componentFiniteDaemon)
	store, err := taskdir.InitStore(owner.root)
	owner.ownStore(t, store)
	if err != nil {
		t.Fatal(err)
	}
	bundle := testsupport.CanonicalTempDir(t)
	clientPath, daemonPath := filepath.Join(bundle, "pueue"), filepath.Join(bundle, "pueued")
	writeRunnerFixture(t, clientPath, "#!/bin/sh\nprintf 'pueue 4.0.4\\n'\n")
	writeRunnerFixture(t, daemonPath, "#!/bin/sh\nexit 0\n")
	var prepared *pueue.Client
	err = pueue.WithBootstrapLockContext(t.Context(), store.Root, func(ctx context.Context) error {
		var prepareErr error
		prepared, prepareErr = prepareCurrentPrivateSupervisor(ctx, store.Root, (Dependencies{ResourceChecker: fixtureResources, InitialSupervisorExecutable: clientPath}).supervisorLookup(), owner.setupOptions(pueue.Options{}))
		return prepareErr
	})
	settleSupervisorProbeTestClient(t, prepared, err)
	if err != nil {
		t.Fatal(err)
	}
	binding := prepared.Binding()
	assertInstalledSupervisorPair(t, binding, clientPath, daemonPath)
}

func TestSupervisorFailureReasonsPreserveCausesAndRedactPaths(t *testing.T) {
	cause := errors.Join(pueue.ErrBinding, errors.New("secret /private/user/config"))
	for _, reason := range []string{"identity_conflict", "transition_pending", "published_pair_unavailable_live", "predecessor_pair_unavailable", "historical_target_not_ready", "active_work", "observation_failed", "state_unreadable"} {
		err := &pueue.PrivateRecoveryError{ReasonCode: reason, Err: cause}
		failure := dispatchFailure(admission.WithStage("start-supervisor", errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, err)), "not_created")
		if failure.ReasonCode != reason {
			t.Fatalf("reason lost: %+v", failure)
		}
		public := publicSupervisorError("Recovery failed.", err)
		if !errors.Is(public, pueue.ErrBinding) || bytes.Contains([]byte(public.Error()), []byte("/private/user")) {
			t.Fatalf("cause lost or private path exposed: %v", public)
		}
	}
}

func TestHealthyPublishedPairIgnoresUnusedRetainedInventory(t *testing.T) {
	store, deps, current := newSupervisorProbeTestFixture(t)
	if err := os.WriteFile(filepath.Join(store.Root, ".supervisor", "pueue-pair.json"), []byte("retired inventory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	var admitted *pueue.Client
	err = pueue.WithBootstrapLockContext(t.Context(), store.Root, func(ctx context.Context) error {
		var prepareErr error
		admitted, prepareErr = supervisor.PreparePrivateAdmissionTransition(ctx, deps.supervisorLookup(), store, deps.SupervisorOptions)
		return prepareErr
	})
	settleSupervisorProbeTestClient(t, admitted, err)
	if err != nil || admitted.Binding() != current.Binding() {
		t.Fatalf("healthy published generation depended on unused inventory: %v", err)
	}
}

func TestMissingInstallationDoesNotSuggestSupervisorMigration(t *testing.T) {
	cause := admission.WithStage("prepare-supervisor-executables", &os.PathError{Op: "stat", Path: "/missing/private/pueued", Err: os.ErrNotExist})
	response := newResponse("dispatch")
	response.TaskRecord = "not_created"
	var stdout bytes.Buffer
	if err := writeJSONResponse(&stdout, "dispatch", &response, cause); err != nil {
		t.Fatal(err)
	}
	if response.Failure == nil || response.Failure.ReasonCode != "" || response.Error != dispatchFailureMessage("supervisor_setup_failed") {
		t.Fatalf("missing installation misclassified as upgrade: %+v", response)
	}
	if bytes.Contains(stdout.Bytes(), []byte("/missing/private")) || bytes.Contains(stdout.Bytes(), []byte("supervisor migrate")) {
		t.Fatalf("misleading setup response: %s", stdout.String())
	}
}

func TestRestartIntentDoesNotYieldToMissingPair(t *testing.T) {
	store, deps, current, owner := newOwnedSupervisorProbeTestFixture(t)
	deps = withSupervisorProbeRouting(t, deps, owner)
	original := current.Binding()
	client := filepath.Join(testsupport.CanonicalTempDir(t), "missing-accepted", "pueue")
	target := original
	target.ClientExecutable, target.DaemonExecutable = client, filepath.Join(filepath.Dir(client), "pueued")
	target.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	target.ClientSHA256, target.DaemonSHA256 = "", ""
	record := struct {
		SchemaVersion int `json:"schema_version"`
		Restart       struct {
			Predecessor task.SupervisorRef `json:"predecessor"`
			Target      task.SupervisorRef `json:"target"`
		} `json:"restart"`
	}{SchemaVersion: task.SchemaVersion}
	record.Restart.Predecessor, record.Restart.Target = original, target
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(original.ConfigPath)
	if err = os.WriteFile(filepath.Join(base, "bootstrap.pending"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	result := supervisorMigrate(store.Root, deps, newSupervisorTestResponse())
	if result.err == nil || result.response.SupervisorControl.ReasonCode != "predecessor_pair_unavailable" {
		t.Fatalf("missing target yielded restart ownership: %+v %v", result.response.SupervisorControl, result.err)
	}
	active, found, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil || !found || active != original {
		t.Fatalf("missing restart target changed active generation: %+v %v", active, err)
	}
	if _, err = os.Stat(filepath.Join(base, "migration.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new migration started over restart intent: %v", err)
	}
}

func TestSupervisorMigrateReplacesRemovedPackagePair(t *testing.T) {
	t.Parallel()
	fixture := newHistoricalSupervisorFixturePaths(t)
	workspace, _ := prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = newHistoricalSupervisorOptions()
	prepareHistoricalSupervisorTask(t, fixture, workspace)
	writeSupervisorGenerationTestGroup(t, fixture.store, fixture.predecessor)
	if err := os.WriteFile(fixture.statusPath, []byte(`{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	metaBefore, err := os.ReadFile(filepath.Join(fixture.root, "tasks", fixture.req.TaskID, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fixture.clientA, fixture.daemonA} {
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	var successor *pueue.Client
	beforeWaits := len(fixture.component.daemonWaits())
	err = pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		var prepareErr error
		successor, prepareErr = prepareCurrentPrivateSupervisor(ctx, fixture.root, fixture.dependencies().supervisorLookup(), fixture.component.setupOptions(fixture.options))
		fixture.component.ownClient(successor, prepareErr)
		if prepareErr != nil {
			return prepareErr
		}
		if prepareErr = supervisor.RecoverInspectionGroupGenerationWithLock(ctx, fixture.store, successor, false); prepareErr != nil {
			return prepareErr
		}
		return supervisor.MigrateInspectionSupervisorLocked(ctx, fixture.store, successor, pueue.DefaultObservationTimeout)
	})
	fixture.component.ownDaemonClients(successor, beforeWaits)
	if successor != nil {
		fixture.current = successor
	}
	if err != nil {
		t.Fatal(err)
	}
	published, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found || published != successor.Binding() {
		t.Fatalf("explicit migration did not commit staged successor: %+v found=%t err=%v", published, found, err)
	}
	metaAfter, err := os.ReadFile(filepath.Join(fixture.root, "tasks", fixture.req.TaskID, "meta.json"))
	if err != nil || !bytes.Equal(metaBefore, metaAfter) {
		t.Fatalf("explicit migration changed historical metadata: %v", err)
	}
}

// An unmarked historical publication is immutable provenance, not permission
// to restart its missing installation. Explicit migration uses installed B and
// retains that provenance in history and task evidence.
func TestPublicSupervisorMigrateReplacesMissingHistoricalInstallation(t *testing.T) {
	t.Parallel()
	fixture := newHistoricalInstallationMigrationFixture(t, true)
	previous := fixture.component.daemonWaits()
	if len(previous) != 1 {
		t.Fatalf("historical predecessor Wait count = %d, want one actual Start", len(previous))
	}
	before := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	deps := withSupervisorProbeRouting(t, fixture.dependencies(), fixture.component)
	result := supervisorMigrate(fixture.root, deps, newSupervisorTestResponse())
	if result.code != 0 || result.err != nil || result.response.SupervisorControl.State != "migrated" {
		t.Fatalf("public migration did not replace missing historical installation: code=%d control=%+v err=%v", result.code, result.response.SupervisorControl, result.err)
	}
	assertHistoricalInstalledMigrationCompleted(t, fixture, previous[0].pid)
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(before, after) {
		t.Fatal("public migration changed historical task bytes")
	}
	waitContext, cancelWait := supervisor.ControlContextWithParent(t.Context(), fixture.options)
	defer cancelWait()
	if err := awaitComponentDaemons(waitContext, previous); err != nil {
		t.Fatal(err)
	}
}

func assertInstalledSupervisorPair(t *testing.T, binding task.SupervisorRef, clientPath, daemonPath string) {
	t.Helper()
	if binding.ClientExecutable != clientPath || binding.DaemonExecutable != daemonPath || binding.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || binding.ClientSHA256 != "" || binding.DaemonSHA256 != "" {
		t.Fatalf("migration did not bind selected installed roles: %+v", binding)
	}
}

func assertHistoricalInstalledMigrationCompleted(t *testing.T, fixture *historicalSupervisorFixture, predecessorPID int) {
	t.Helper()
	published, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found {
		t.Fatalf("public migration did not publish installed B: found=%t err=%v", found, err)
	}
	assertInstalledSupervisorPair(t, published, fixture.clientB, fixture.daemonB)
	if complete, completeErr := pueue.PrivateSupervisorMigrationComplete(fixture.root, published); completeErr != nil || !complete {
		t.Fatalf("public migration did not complete installed B: complete=%t err=%v", complete, completeErr)
	}
	var history historicalPrivateMigrationHistory
	readHistoricalRecord(t, filepath.Join(fixture.root, ".supervisor", "migration-history.json"), &history)
	if len(history.Migrations) != 1 {
		t.Fatalf("historical migration count = %d, want one A-to-B edge", len(history.Migrations))
	}
	migration := history.Migrations[0]
	assertHistoricalDaemonIdentity(t, "historical predecessor", migration.Legacy, fixture.predecessor)
	assertHistoricalDaemonIdentity(t, "installed successor", migration.Successor, published)
	if migration.LegacyPID != predecessorPID || !migration.LegacyShutdownDispatched || !migration.LegacyStopped {
		t.Fatalf("migration lost actual predecessor shutdown evidence: %+v", migration)
	}
	if starts := string(readSupervisorProbeTestFile(t, fixture.daemonStartLog)); starts != "A\nB\n" {
		t.Fatalf("public migration started an unexpected daemon: %q", starts)
	}
}

func TestPublicSupervisorMigratePreservesMissingHistoricalConfig(t *testing.T) {
	fixture := newHistoricalInstallationMigrationFixture(t, true)
	previous := fixture.component.daemonWaits()
	if len(previous) != 1 {
		t.Fatalf("historical predecessor Wait count = %d, want one actual Start", len(previous))
	}
	writeSupervisorGenerationTestGroup(t, fixture.store, fixture.predecessor)
	head := historicalMigrationGroupHead(t, fixture)
	configPath := pueue.PrivateConfigPath(fixture.root)
	config := readSupervisorProbeTestFile(t, configPath)
	// Restore the exact accepted config before the component's supported
	// shutdown and Wait cleanup, including when an assertion fails.
	t.Cleanup(func() {
		if err := os.WriteFile(configPath, config, 0o600); err != nil {
			t.Error(err)
		}
	})
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	privateBefore := capturePairFaultWitness(t, filepath.Dir(configPath))
	startsBefore := readSupervisorProbeTestFile(t, fixture.daemonStartLog)
	taskBefore := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	deps := withSupervisorProbeRouting(t, fixture.dependencies(), fixture.component)
	result := supervisorMigrate(fixture.root, deps, newSupervisorTestResponse())
	if result.code == 0 || result.err == nil {
		t.Fatal("public migration accepted a missing historical config")
	}
	assertPairFaultWitness(t, filepath.Dir(configPath), privateBefore)
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused migration recreated historical config: %v", err)
	}
	if !bytes.Equal(startsBefore, readSupervisorProbeTestFile(t, fixture.daemonStartLog)) {
		t.Fatal("missing config migration started a daemon")
	}
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(taskBefore, after) {
		t.Fatal("missing config migration changed historical task bytes")
	}
	assertHistoricalMigrationGroupUnchanged(t, fixture, head)
	select {
	case <-previous[0].done:
		t.Fatal("missing config migration stopped the historical predecessor")
	default:
	}
}

func TestPublicSupervisorMigrateRefusesStoppedHistoricalGenerationBeforeIntent(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "installation present", true: "installation missing"}[missing], func(t *testing.T) {
			runStoppedHistoricalMigrationRefusal(t, missing)
		})
	}
}

func runStoppedHistoricalMigrationRefusal(t *testing.T, missing bool) {
	t.Helper()
	fixture := newHistoricalInstallationMigrationFixture(t, missing)
	writeSupervisorGenerationTestGroup(t, fixture.store, fixture.predecessor)
	head := historicalMigrationGroupHead(t, fixture)
	stopHistoricalInstallationPredecessor(t, fixture)
	configPath := pueue.PrivateConfigPath(fixture.root)
	configBefore := readSupervisorProbeTestFile(t, configPath)
	privateBefore := capturePairFaultWitness(t, filepath.Dir(configPath))
	if privateBefore["migration.json"].present || privateBefore["migration-history.json"].present {
		t.Fatal("stopped fixture already authorized a migration")
	}
	startsBefore := readSupervisorProbeTestFile(t, fixture.daemonStartLog)
	taskBefore := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	deps := withSupervisorProbeRouting(t, fixture.dependencies(), fixture.component)
	result := supervisorMigrate(fixture.root, deps, newSupervisorTestResponse())
	if result.code == 0 || result.err == nil {
		t.Fatal("fresh migration accepted a stopped historical generation")
	}
	control := result.response.SupervisorControl
	if control.ReasonCode != "historical_target_not_ready" || control.NextAction != "stop_and_report" || !errors.Is(result.err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("stopped historical migration lost its refusal reason or action: %+v: %v", control, result.err)
	}
	assertPairFaultWitness(t, filepath.Dir(configPath), privateBefore)
	if !bytes.Equal(configBefore, readSupervisorProbeTestFile(t, configPath)) {
		t.Fatal("refused stopped migration changed historical config")
	}
	if !bytes.Equal(startsBefore, readSupervisorProbeTestFile(t, fixture.daemonStartLog)) {
		t.Fatal("refused stopped migration started a daemon")
	}
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(taskBefore, after) {
		t.Fatal("refused stopped migration changed historical task bytes")
	}
	assertHistoricalMigrationGroupUnchanged(t, fixture, head)
}

func stopHistoricalInstallationPredecessor(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	waits := fixture.component.daemonWaits()
	if len(waits) != 1 {
		t.Fatalf("historical predecessor Wait count = %d, want one actual Start", len(waits))
	}
	ctx, cancel := supervisor.ControlContextWithParent(t.Context(), fixture.options)
	defer cancel()
	err := pueue.WithBootstrapLockContext(ctx, fixture.root, func(locked context.Context) error {
		if shutdownErr := fixture.predecessorClient.Shutdown(locked); shutdownErr != nil {
			return shutdownErr
		}
		return fixture.predecessorClient.WaitUnavailable(locked)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = awaitComponentDaemons(ctx, waits); err != nil {
		t.Fatal(err)
	}
}

func historicalMigrationGroupHead(t *testing.T, fixture *historicalSupervisorFixture) inspection.GroupRequestRecord {
	t.Helper()
	head, migrated, err := inspection.ReadGroupHead(t.Context(), fixture.store)
	if err != nil || migrated {
		t.Fatalf("fixture has no fresh historical group head: migrated=%t err=%v", migrated, err)
	}
	assertHistoricalMigrationGroupUnchanged(t, fixture, head)
	return head
}

func assertHistoricalMigrationGroupUnchanged(t *testing.T, fixture *historicalSupervisorFixture, before inspection.GroupRequestRecord) {
	t.Helper()
	if pending, err := inspection.GroupMigrationPending(t.Context(), fixture.store); err != nil || pending {
		t.Fatalf("refused migration left an inspection proposal: pending=%t err=%v", pending, err)
	}
	head, migrated, err := inspection.ReadGroupHead(t.Context(), fixture.store)
	if err != nil || migrated || head != before {
		t.Fatalf("refused migration changed historical inspection head: migrated=%t head=%+v err=%v", migrated, head, err)
	}
}

func newHistoricalInstallationMigrationFixture(t *testing.T, missing bool) *historicalSupervisorFixture {
	t.Helper()
	fixture := newHistoricalSupervisorFixturePaths(t)
	workspace, _ := prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = newHistoricalSupervisorOptions()
	bindHistoricalSupervisorFixture(t, fixture)
	historical := fixture.predecessor
	historical.ExecutableIdentity = ""
	// Frozen main-era record data; these values are never derived from programs.
	historical.ClientSHA256, historical.DaemonSHA256 = strings.Repeat("a", 64), strings.Repeat("b", 64)
	options := fixture.options
	options.RuntimeClientExecutable = fixture.clientB
	legacy, err := pueue.NewClient(historical, options)
	fixture.component.ownClient(legacy, err)
	if err != nil {
		t.Fatal(err)
	}
	// Preserve the original options and existing A Wait. Its supported cleanup
	// can use installed B transport even after A's installation disappears.
	fixture.component.ownDaemonClients(legacy, 0)
	if err = os.Remove(filepath.Join(fixture.root, ".supervisor", "supervisor.identity.json")); err != nil {
		t.Fatal(err)
	}
	writeSupervisorGenerationTestPrivateGeneration(t, fixture.store, historical)
	fixture.predecessor, fixture.predecessorClient, fixture.current = historical, legacy, legacy
	brief := []byte("historical installation migration")
	fixture.req = newHistoricalSupervisorTaskRecord(fixture.store.RootID, workspace, brief)
	profile := appTestExecutionProfile(&fixture.req, fixture.provider, nil)
	fixture.meta = tasklifecycle.NewMeta(fixture.req, profile, historical, fixture.runner, task.RunnerOwnershipCustom, "historical-supervisor-test")
	fixture.td, err = fixture.store.CreateTask(fixture.req.TaskID, &fixture.req, brief, &fixture.meta)
	fixture.component.ownTask(fixture.td)
	if err != nil {
		t.Fatal(err)
	}
	recordHistoricalSupervisorAdmission(t, fixture)
	if missing {
		for _, path := range []string{fixture.clientA, fixture.daemonA} {
			if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
	}
	return fixture
}

func TestExplicitMigrationResumesPrivateJournal(t *testing.T) {
	t.Parallel()
	fixture := newHistoricalSupervisorFixturePaths(t)
	seed, err := taskdir.InitStore(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	rootID := seed.RootID
	if err = seed.Close(); err != nil {
		t.Fatal(err)
	}
	workspace, _ := prepareHistoricalSupervisorExecutables(t, fixture)
	pid := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	for _, installed := range []struct{ path, generation string }{{fixture.clientA, "A"}, {fixture.clientB, "B"}} {
		source := historicalSupervisorClientScript(installed.generation, fixture.readyPath, pid, fixture.statusPath, fixture.commandLog)
		old := `"default":{"status":"Running","parallel_tasks":0}`
		groups := old + `,"delegation-inspection-` + rootID + `":{"status":"Running","parallel_tasks":1}`
		writeRunnerFixture(t, installed.path, strings.ReplaceAll(source, old, groups))
	}
	fixture.options = newHistoricalSupervisorOptions()
	prepareHistoricalSupervisorTask(t, fixture, workspace)
	writeSupervisorGenerationTestGroup(t, fixture.store, fixture.predecessor)
	if err = os.WriteFile(fixture.statusPath, []byte(`{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := fixture.options
	blocked.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []pueue.Job) error {
		return pueue.ErrPrivateSupervisorMigrationBusy
	}
	err = pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		current, prepareErr := prepareCurrentPrivateSupervisor(ctx, fixture.root, fixture.dependencies().supervisorLookup(), blocked)
		if prepareErr != nil {
			return prepareErr
		}
		migrationErr := current.MigratePrivateSupervisorWithLock(ctx, fixture.root)
		return errors.Join(migrationErr, current.CloseAfterPending(migrationErr))
	})
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("fixture did not interrupt the real migration path: %v", err)
	}
	if err = os.Remove(fixture.statusPath); err != nil {
		t.Fatal(err)
	}
	deps := withSupervisorProbeRouting(t, fixture.dependencies(), fixture.component)
	result := supervisorMigrate(fixture.root, deps, newSupervisorTestResponse())
	if result.code != 0 || result.err != nil || result.response.SupervisorControl.State != "migrated" {
		t.Fatalf("explicit retry did not resume pending private journal: code=%d err=%v control=%+v", result.code, result.err, result.response.SupervisorControl)
	}
}

func prepareInterruptedInspectionMigration(t *testing.T, scenario *historicalInspectionMigrationScenario, packagePaths bool) {
	t.Helper()
	fixture := scenario.fixture
	blocked := fixture.options
	blocked.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []pueue.Job) error {
		return pueue.ErrPrivateSupervisorMigrationBusy
	}
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		var current *pueue.Client
		var prepareErr error
		if packagePaths {
			current, prepareErr = pueue.PreparePrivateForMigrationWithLock(ctx, fixture.clientB, fixture.daemonB, fixture.root, blocked)
		} else {
			current, prepareErr = prepareCurrentPrivateSupervisor(ctx, fixture.root, fixture.dependencies().supervisorLookup(), blocked)
		}
		fixture.component.ownClient(current, prepareErr)
		if prepareErr != nil {
			return prepareErr
		}
		if prepareErr = fixture.current.Close(); prepareErr != nil {
			return errors.Join(prepareErr, current.Close())
		}
		fixture.current = current
		scenario.successorGroup.Supervisor = current.Binding()
		firstProposal := scenario.successorGroup
		firstProposal.Supervisor.DaemonExecutable += "-earlier"
		if prepareErr = inspection.PrepareGroupMigration(ctx, fixture.store, scenario.legacyGroup, firstProposal); prepareErr != nil {
			return prepareErr
		}
		if prepareErr = inspection.PrepareGroupMigration(ctx, fixture.store, scenario.legacyGroup, scenario.successorGroup); prepareErr != nil {
			return prepareErr
		}
		migrationErr := current.MigratePrivateSupervisorWithLock(ctx, fixture.root)
		fixture.component.ownClient(current, migrationErr)
		return migrationErr
	})
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("fixture did not create both pending journals: %v", err)
	}
}

func TestExplicitMigrationFinishesRecordedInspectionSuccessor(t *testing.T) {
	t.Parallel()
	scenario := newHistoricalInspectionMigrationScenario(t)
	fixture := scenario.fixture
	prepareInterruptedInspectionMigration(t, scenario, false)
	pid := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	writeRunnerFixture(t, fixture.clientB, historicalSupervisorClientScript("B", fixture.readyPath, pid, fixture.statusPath, fixture.commandLog)+"\n# later installation\n")
	writeRunnerFixture(t, fixture.daemonB, historicalSupervisorDaemonScript("B", fixture.readyPath, pid, fixture.daemonStartLog)+"\n# later installation\n")
	status := `{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0},"delegation-inspection-` + fixture.store.RootID + `":{"status":"Running","parallel_tasks":1}}}`
	if err := os.WriteFile(fixture.statusPath, []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := withSupervisorProbeRouting(t, fixture.dependencies(), fixture.component)
	result := supervisorMigrate(fixture.root, deps, newSupervisorTestResponse())
	if result.code != 0 || result.err != nil {
		t.Fatalf("recorded successor was displaced by later installation: %v", result.err)
	}
	active, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found || active != scenario.successorGroup.Supervisor {
		t.Fatalf("wrong pending successor committed: %+v found=%t err=%v", active, found, err)
	}
	complete, err := inspection.GroupMigrationComplete(t.Context(), fixture.store, scenario.legacyGroup, scenario.successorGroup)
	if err != nil || !complete {
		t.Fatalf("exact inspection edge was not completed: %t %v", complete, err)
	}
}

func TestMissingMigrationInstallationPreservesBoundaryErrors(t *testing.T) {
	scenario := newHistoricalInspectionMigrationScenario(t)
	fixture := scenario.fixture
	prepareInterruptedInspectionMigration(t, scenario, true)
	path := filepath.Join(fixture.root, ".supervisor", "migration.json")
	before := readSupervisorProbeTestFile(t, path)
	proposals := snapshotRetainedInspectionProposals(t, fixture.root)
	commands := readSupervisorProbeTestFile(t, fixture.commandLog)
	starts := readSupervisorProbeTestFile(t, fixture.daemonStartLog)
	// Re-author only fixture-owned script source so the existing finalizer can
	// close its original native waits even when the assertions fail.
	t.Cleanup(func() {
		pid := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
		writeRunnerFixture(t, fixture.clientB, historicalSupervisorClientScript("B", fixture.readyPath, pid, fixture.statusPath, fixture.commandLog))
		writeRunnerFixture(t, fixture.daemonB, historicalSupervisorDaemonScript("B", fixture.readyPath, pid, fixture.daemonStartLog))
	})
	for _, accepted := range []string{fixture.clientB, fixture.daemonB} {
		if err := os.Remove(accepted); err != nil {
			t.Fatal(err)
		}
	}
	result := supervisorMigrate(fixture.root, fixture.dependencies(), newSupervisorTestResponse())
	if result.code == 0 || !errors.Is(result.err, pueue.ErrConfiguration) || !errors.Is(result.err, os.ErrNotExist) {
		t.Fatalf("missing current installation lost its original cause: %v", result.err)
	}
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		options := componentPublicationOptions(t, fixture.root, fixture.component, fixture.options)
		client, prepareErr := prepareInspectionMigrationSupervisorWithLock(ctx, fixture.store, fixture.dependencies().supervisorLookup(), options)
		fixture.component.ownClient(client, prepareErr)
		return prepareErr
	})
	assertPendingMissingInstallationCause(t, err)
	if !bytes.Equal(commands, readSupervisorProbeTestFile(t, fixture.commandLog)) {
		t.Fatal("unavailable current selection issued a supervisor command")
	}
	clientC, daemonC := installInspectionMigrationTargetC(t, fixture)
	if err = os.Remove(daemonC); err != nil {
		t.Fatal(err)
	}
	deps := fixture.dependencies()
	deps.InitialSupervisorExecutable = clientC
	deps = withSupervisorProbeRouting(t, deps, fixture.component)
	result = supervisorMigrate(fixture.root, deps, newSupervisorTestResponse())
	if result.code == 0 || result.err == nil {
		t.Fatal("pending migration accepted an installation with no daemon")
	}
	if result.response.SupervisorControl == nil || result.response.SupervisorControl.ReasonCode != "transition_pending" {
		t.Fatalf("pending migration lost its refusal reason: %+v", result.response.SupervisorControl)
	}
	if !bytes.Equal(before, readSupervisorProbeTestFile(t, path)) {
		t.Fatal("refused target rewrote accepted migration")
	}
	assertRetainedInspectionProposals(t, proposals)
	if !bytes.Equal(starts, readSupervisorProbeTestFile(t, fixture.daemonStartLog)) {
		t.Fatal("refused preparation started a daemon")
	}
}

func assertPendingMissingInstallationCause(t *testing.T, err error) {
	t.Helper()
	var recovery *pueue.PrivateRecoveryError
	if !errors.As(err, &recovery) || recovery.ReasonCode != "transition_pending" || !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) || !errors.Is(err, pueue.ErrConfiguration) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical pending preparation lost its refusal or original cause: %v", err)
	}
}

func TestExplicitMigrationSelectsInstalledSuccessorBeforeShutdown(t *testing.T) {
	t.Parallel()
	for _, window := range []struct {
		name           string
		privateJournal bool
		appendSelected bool
	}{
		{name: "prepared private journal", privateJournal: true},
		{name: "unique unavailable inspection proposal only"},
		{name: "selected proposal already appended", privateJournal: true, appendSelected: true},
		{name: "ambiguous inspection proposals only", appendSelected: true},
	} {
		t.Run(window.name, func(t *testing.T) {
			t.Parallel()
			scenario := newRetargetableInspectionMigration(t, window.privateJournal)
			fixture := scenario.fixture
			proposals := snapshotRetainedInspectionProposals(t, fixture.root)
			privatePath := filepath.Join(fixture.root, ".supervisor", "migration.json")
			var privateBefore []byte
			if window.privateJournal {
				privateBefore = readSupervisorProbeTestFile(t, privatePath)
			}
			clientC, daemonC := installInspectionMigrationTargetC(t, fixture)
			selected := prepareInspectionMigrationTargetC(t, scenario, clientC, daemonC, window.appendSelected)
			if window.privateJournal && !bytes.Equal(privateBefore, readSupervisorProbeTestFile(t, privatePath)) {
				t.Fatal("preparing C's inspection proposal replaced the existing private B journal")
			}
			removeAcceptedInspectionInstallation(t, fixture)
			deps := fixture.dependencies()
			deps.InitialSupervisorExecutable = clientC
			shutdownOrder := observeSelectedInspectionProposalBeforeShutdown(t, fixture, selected.Supervisor, &deps)
			deps = withSupervisorProbeRouting(t, deps, fixture.component)
			result := supervisorMigrate(fixture.root, deps, newSupervisorTestResponse())
			shutdownOrder()
			assertRetainedInspectionProposals(t, proposals)
			assertInspectionProposalIncomplete(t, scenario, scenario.successorGroup)
			if result.code != 0 || result.err != nil || result.response.SupervisorControl.State != "migrated" {
				t.Fatalf("installed C did not supersede missing pre-shutdown B: code=%d control=%+v err=%v", result.code, result.response.SupervisorControl, result.err)
			}
			assertInstalledInspectionMigrationCompleted(t, scenario, selected)
		})
	}
}

func observeSelectedInspectionProposalBeforeShutdown(t *testing.T, fixture *historicalSupervisorFixture, selected task.SupervisorRef, deps *Dependencies) func() {
	t.Helper()
	faults := make(chan error, 1)
	deps.SupervisorOptions.Observer = func(event pueue.CommandEvent) {
		if event.Stage != "entry" || !historicalCommandSequence(event.Argv, "shutdown") {
			return
		}
		proposals, err := inspection.PreparedGroupMigrationSuccessors(t.Context(), fixture.store)
		found := false
		for _, proposal := range proposals {
			found = found || proposal == selected
		}
		if err == nil && !found {
			err = errors.New("predecessor shutdown entered before selected C inspection proposal was durable")
		}
		if err != nil {
			select {
			case faults <- err:
			default:
			}
		}
	}
	return func() {
		select {
		case err := <-faults:
			t.Fatal(err)
		default:
		}
	}
}

func newRetargetableInspectionMigration(t *testing.T, privateJournal bool) *historicalInspectionMigrationScenario {
	t.Helper()
	// This existing fixture registers original-config restoration before the
	// first retargeting write, while the component owner retains A's real Wait.
	fixture := newPublishedConfigDriftFixture(t)
	legacyGroup, migrated, err := inspection.ReadGroupHead(t.Context(), fixture.store)
	if err != nil || migrated {
		t.Fatalf("retargeting fixture lost A's group head: migrated=%t err=%v", migrated, err)
	}
	var successor *pueue.Client
	err = pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		var prepareErr error
		options := componentPublicationOptions(t, fixture.root, fixture.component, fixture.options)
		successor, prepareErr = pueue.PreparePrivateForMigrationWithLock(ctx, fixture.clientB, fixture.daemonB, fixture.root, options)
		fixture.component.ownClient(successor, prepareErr)
		return prepareErr
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.current, fixture.successor = successor, successor.Binding()
	successorGroup := legacyGroup
	successorGroup.Supervisor = successor.Binding()
	scenario := &historicalInspectionMigrationScenario{fixture: fixture, legacyGroup: legacyGroup, successorGroup: successorGroup}
	if privateJournal {
		prepareInterruptedInspectionMigration(t, scenario, true)
		fixture.component.ownClient(fixture.current, nil)
		assertConfigDriftPreparedJournal(t, fixture)
	} else {
		if err := inspection.PrepareGroupMigration(t.Context(), fixture.store, scenario.legacyGroup, scenario.successorGroup); err != nil {
			t.Fatal(err)
		}
		if pending, err := pueue.PrivateSupervisorMigrationPending(fixture.root); err != nil || pending {
			t.Fatalf("proposal-only window has a private migration journal: pending=%t err=%v", pending, err)
		}
	}
	return scenario
}

func installInspectionMigrationTargetC(t *testing.T, fixture *historicalSupervisorFixture) (string, string) {
	t.Helper()
	installation := filepath.Join(fixture.component.base, "selected-C")
	client, daemon := filepath.Join(installation, "pueue"), filepath.Join(installation, "pueued")
	pid := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	writeRunnerFixture(t, client, historicalSupervisorClientScript("C", fixture.readyPath, pid, fixture.statusPath, fixture.commandLog))
	writeRunnerFixture(t, daemon, historicalSupervisorDaemonScript("C", fixture.readyPath, pid, fixture.daemonStartLog))
	status := `{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0},"delegation-inspection-` + fixture.store.RootID + `":{"status":"Running","parallel_tasks":1}}}`
	if err := os.WriteFile(fixture.statusPath, []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	return client, daemon
}

func prepareInspectionMigrationTargetC(t *testing.T, scenario *historicalInspectionMigrationScenario, clientPath, daemonPath string, appendProposal bool) inspection.GroupRequestRecord {
	t.Helper()
	fixture := scenario.fixture
	selected := scenario.successorGroup
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		options := componentPublicationOptions(t, fixture.root, fixture.component, fixture.options)
		client, prepareErr := pueue.PreparePrivateForMigrationWithLock(ctx, clientPath, daemonPath, fixture.root, options)
		fixture.component.ownClient(client, prepareErr)
		if prepareErr != nil {
			return prepareErr
		}
		selected.Supervisor = client.Binding()
		var proposalErr error
		if appendProposal {
			proposalErr = inspection.PrepareGroupMigration(ctx, fixture.store, scenario.legacyGroup, selected)
			if proposalErr == nil {
				// Replay the exact C append before ordinary migration resumes B's
				// still-prepared private journal.
				proposalErr = inspection.PrepareGroupMigration(ctx, fixture.store, scenario.legacyGroup, selected)
			}
		}
		return errors.Join(proposalErr, client.CloseAfterPending(proposalErr))
	})
	if err != nil {
		t.Fatal(err)
	}
	return selected
}

func removeAcceptedInspectionInstallation(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	if err := fixture.current.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fixture.clientB, fixture.daemonB} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func snapshotRetainedInspectionProposals(t *testing.T, root string) map[string][]byte {
	t.Helper()
	directory := filepath.Join(root, "inspection-group")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	proposals := make(map[string][]byte)
	for _, entry := range entries {
		if entry.Name() == "migration.json" || strings.HasPrefix(entry.Name(), "migration-") {
			path := filepath.Join(directory, entry.Name())
			proposals[path] = readSupervisorProbeTestFile(t, path)
		}
	}
	if len(proposals) == 0 {
		t.Fatal("fixture did not publish an immutable B proposal")
	}
	return proposals
}

func assertRetainedInspectionProposals(t *testing.T, proposals map[string][]byte) {
	t.Helper()
	for path, before := range proposals {
		if !bytes.Equal(before, readSupervisorProbeTestFile(t, path)) {
			t.Fatalf("retargeting rewrote retained inspection proposal %s", path)
		}
	}
}

func assertInspectionProposalIncomplete(t *testing.T, scenario *historicalInspectionMigrationScenario, proposal inspection.GroupRequestRecord) {
	t.Helper()
	if complete, err := inspection.GroupMigrationComplete(t.Context(), scenario.fixture.store, scenario.legacyGroup, proposal); err != nil || complete {
		t.Fatalf("missing B was completed: complete=%t err=%v", complete, err)
	}
}

func assertInstalledInspectionMigrationCompleted(t *testing.T, scenario *historicalInspectionMigrationScenario, selected inspection.GroupRequestRecord) {
	t.Helper()
	fixture := scenario.fixture
	active, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found || active != selected.Supervisor {
		t.Fatalf("installed C was not published: found=%t active=%+v want=%+v err=%v", found, active, selected.Supervisor, err)
	}
	if complete, completeErr := inspection.GroupMigrationComplete(t.Context(), fixture.store, scenario.legacyGroup, selected); completeErr != nil || !complete {
		t.Fatalf("exact A-to-C inspection edge did not complete: complete=%t err=%v", complete, completeErr)
	}
	if starts := string(readSupervisorProbeTestFile(t, fixture.daemonStartLog)); starts != "A\nC\n" {
		t.Fatalf("retargeted migration started an unexpected daemon: %q", starts)
	}
}

func TestInspectionMigrationUsesActivePredecessorAfterPathRelocation(t *testing.T) {
	scenario := newHistoricalInspectionMigrationScenario(t)
	fixture := scenario.fixture
	installation := filepath.Join(fixture.component.base, "relocated-A")
	client := filepath.Join(installation, "pueue")
	daemon := filepath.Join(installation, "pueued")
	pid := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	writeRunnerFixture(t, client, historicalSupervisorClientScript("A", fixture.readyPath, pid, fixture.statusPath, fixture.commandLog))
	writeRunnerFixture(t, daemon, historicalSupervisorDaemonScript("A", fixture.readyPath, pid, fixture.daemonStartLog))
	options := fixture.component.setupOptions(fixture.options)
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		if writeErr := os.WriteFile(scenario.configPath, scenario.config, 0o600); writeErr != nil {
			return writeErr
		}
		before := len(fixture.component.daemonWaits())
		relocated, prepareErr := pueue.PreparePrivateForMigrationWithLock(ctx, client, daemon, fixture.root, options)
		fixture.component.ownClient(relocated, prepareErr)
		fixture.component.ownDaemonClients(relocated, before)
		if prepareErr != nil {
			return prepareErr
		}
		if prepareErr = fixture.current.Close(); prepareErr != nil {
			return errors.Join(prepareErr, relocated.Close())
		}
		fixture.current = relocated
		before = len(fixture.component.daemonWaits())
		prepareErr = relocated.MigratePrivateSupervisorWithLock(ctx, fixture.root)
		fixture.component.ownClient(relocated, prepareErr)
		fixture.component.ownDaemonClients(relocated, before)
		if prepareErr != nil {
			return prepareErr
		}
		if prepareErr = os.WriteFile(scenario.configPath, scenario.changedConfig, 0o600); prepareErr != nil {
			return prepareErr
		}
		before = len(fixture.component.daemonWaits())
		successor, prepareErr := prepareCurrentPrivateSupervisor(ctx, fixture.root, fixture.dependencies().supervisorLookup(), options)
		fixture.component.ownClient(successor, prepareErr)
		fixture.component.ownDaemonClients(successor, before)
		if prepareErr != nil {
			return prepareErr
		}
		if prepareErr = relocated.Close(); prepareErr != nil {
			return errors.Join(prepareErr, successor.Close())
		}
		fixture.current = successor
		scenario.successorGroup.Supervisor = successor.Binding()
		before = len(fixture.component.daemonWaits())
		migrationErr := supervisor.MigrateInspectionGroup(ctx, fixture.store, successor, pueue.DefaultObservationTimeout, scenario.legacyGroup)
		fixture.component.ownClient(successor, migrationErr)
		fixture.component.ownDaemonClients(successor, before)
		return migrationErr
	})
	if err != nil {
		t.Fatalf("retained inspection head could not follow the actual private predecessor: %v", err)
	}
	complete, err := inspection.GroupMigrationComplete(t.Context(), fixture.store, scenario.legacyGroup, scenario.successorGroup)
	if err != nil || !complete {
		t.Fatalf("inspection edge was not committed after relocation: %t %v", complete, err)
	}
}
