package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
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

// Explicit migration owns completed private progress; a later inspection
// failure does not require rolling back an already-ready successor.
func TestPublicSupervisorMigrateRepairsStoppedMissingInstalledPair(t *testing.T) {
	fixture := newHistoricalSupervisorFixturePaths(t)
	workspace, _ := prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = newHistoricalSupervisorOptions()
	prepareHistoricalSupervisorTask(t, fixture, workspace)
	assertInstalledSupervisorPair(t, fixture.predecessor, fixture.clientA, fixture.daemonA)
	writeSupervisorGenerationTestGroup(t, fixture.store, fixture.predecessor)
	head := historicalMigrationGroupHead(t, fixture)
	stopHistoricalInstallationPredecessor(t, fixture)
	for _, path := range []string{fixture.clientA, fixture.daemonA} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	resetHistoricalSupervisorCommandLog(t, fixture)
	privateBefore := capturePairFaultWitness(t, filepath.Dir(fixture.predecessor.ConfigPath))
	if privateBefore["bootstrap.pending"].present || privateBefore["migration.json"].present || privateBefore["migration-history.json"].present {
		t.Fatal("stopped installed fixture already authorized a transition")
	}
	taskBefore := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	observerErrors := observeStoppedInstalledMigrationGroup(t, fixture, head.Name)
	deps := fixture.dependencies()
	err := dispatchPairFault(t, "stopped missing installed pair", func(ctx context.Context) (*pueue.Client, error) {
		return supervisor.PreparePrivateAdmissionTransition(ctx, deps.supervisorLookup(), fixture.store, fixture.options)
	}, fixture.root)
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("stopped missing installed pair did not refuse dispatch admission: %v", err)
	}
	assertPairFaultWitness(t, filepath.Dir(fixture.predecessor.ConfigPath), privateBefore)
	result := migratePairFault(t, "stopped missing installed pair", fixture.root, deps, fixture.component)
	control := result.response.SupervisorControl
	if result.code != 0 || result.err != nil || control.State != "migrated" || control.NextAction != "retry_dispatch" {
		t.Fatalf("public migration did not repair the stopped missing installation: code=%d control=%+v err=%v", result.code, control, result.err)
	}
	assertStoppedInstalledMigrationCompleted(t, fixture, head)
	select {
	case observerErr := <-observerErrors:
		t.Fatal(observerErr)
	default:
	}
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(taskBefore, after) {
		t.Fatal("explicit migration changed immutable admitted task evidence")
	}
	ctx, cancel := supervisor.ControlContextWithParent(t.Context(), fixture.options)
	defer cancel()
	waits := fixture.component.daemonWaits()
	if err = fixture.component.stopDaemons(ctx, waits); err != nil {
		t.Fatal(err)
	}
	if err = awaitComponentDaemons(ctx, waits); err != nil {
		t.Fatal(err)
	}
}

// Callback replacement changes logical supervisor identity. The private
// generation and inspection journal must agree on its successor while the
// original task store remains the owner of committed terminal evidence.
func TestPublicSupervisorMigrateReplacesLiveMissingPairAndCallback(t *testing.T) {
	fixture := newHistoricalSupervisorFixturePaths(t)
	head, observerErrors := prepareLiveCallbackMigration(t, fixture)
	taskBefore := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	deps := withSupervisorProbeRouting(t, fixture.dependencies(), fixture.component)
	result := supervisorMigrate(fixture.root, deps, newSupervisorTestResponse())
	control := result.response.SupervisorControl
	if result.code != 0 || result.err != nil || control.State != "migrated" || control.NextAction != "retry_dispatch" {
		t.Fatalf("public callback migration did not replace live missing installation: code=%d control=%+v err=%v", result.code, control, result.err)
	}
	assertLiveCallbackMigrationCompleted(t, fixture, head)
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(taskBefore, after) {
		t.Fatal("callback migration changed committed task evidence")
	}
	assertHistoricalTaskEvidence(t, fixture)
	finishCallbackMigrationFixture(t, fixture, observerErrors)
}

func TestPublicSupervisorMigrateResumesPreparedCallback(t *testing.T) {
	for _, busy := range []bool{false, true} {
		name := "idle"
		if busy {
			name = "busy then idle"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runPreparedCallbackMigrationRetry(t, busy)
		})
	}
}

func runPreparedCallbackMigrationRetry(t *testing.T, busy bool) {
	t.Helper()
	fixture := newHistoricalSupervisorFixturePaths(t)
	head, observerErrors := prepareLiveCallbackMigration(t, fixture)
	selected := prepareCallbackMigrationCandidate(t, fixture, fixture.component.setupOptions(fixture.options))
	configBefore, err := os.Stat(selected.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	taskBefore := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	if busy {
		assertPreparedCallbackMigrationBusy(t, fixture, selected)
		writeEndedHistoricalMigrationStatus(t, fixture)
		if err = addHistoricalInspectionGroupStatus(fixture.statusPath, head.Name); err != nil {
			t.Fatal(err)
		}
	}
	result := migratePairFault(t, "prepared callback retry", fixture.root, fixture.dependencies(), fixture.component)
	if result.code != 0 || result.err != nil || result.response.SupervisorControl.State != "migrated" || result.response.SupervisorControl.NextAction != "retry_dispatch" {
		t.Fatalf("prepared callback retry failed: control=%+v err=%v", result.response.SupervisorControl, result.err)
	}
	assertLiveCallbackMigrationCompleted(t, fixture, head)
	if fixture.successor != selected {
		t.Fatalf("prepared callback retry changed selected B: got=%+v want=%+v", fixture.successor, selected)
	}
	configAfter, err := os.Stat(selected.ConfigPath)
	if err != nil || !os.SameFile(configBefore, configAfter) || !configBefore.ModTime().Equal(configAfter.ModTime()) {
		t.Fatalf("prepared callback retry rewrote the accepted config: %v", err)
	}
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(taskBefore, after) {
		t.Fatal("prepared callback retry changed committed task bytes")
	}
	assertHistoricalTaskEvidence(t, fixture)
	finishCallbackMigrationFixture(t, fixture, observerErrors)
}

func prepareCallbackMigrationCandidate(t *testing.T, fixture *historicalSupervisorFixture, options pueue.Options) task.SupervisorRef {
	t.Helper()
	before := len(fixture.component.daemonWaits())
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		var prepareErr error
		fixture.current, prepareErr = prepareCurrentPrivateSupervisor(ctx, fixture.root, fixture.dependencies().supervisorLookup(), options)
		fixture.component.ownClient(fixture.current, prepareErr)
		return prepareErr
	})
	fixture.component.ownDaemonClients(fixture.current, before)
	if err != nil {
		t.Fatal(err)
	}
	selected := fixture.current.Binding()
	assertInstalledSupervisorPair(t, selected, fixture.clientB, fixture.daemonB)
	alias, found, err := pueue.ReadPrivateRecoveryBinding(fixture.root)
	if err != nil || !found || !task.SameSupervisorIdentity(alias, fixture.predecessor) || task.SameSupervisorIdentity(selected, fixture.predecessor) {
		t.Fatalf("callback preparation did not retain original A and distinct B: alias=%+v found=%t err=%v", alias, found, err)
	}
	assertCallbackMigrationHasNoIntent(t, fixture)
	return selected
}

func assertPreparedCallbackMigrationBusy(t *testing.T, fixture *historicalSupervisorFixture, selected task.SupervisorRef) {
	t.Helper()
	writeRunnerStashedStatus(t, fixture.statusPath, fixture.req.RootID, fixture.req.TaskID, fixture.receipt.NumericTaskID)
	status := readSupervisorProbeTestFile(t, fixture.statusPath)
	identities := publishedPairIdentityFiles(t, fixture.root)
	taskBefore := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	result := migratePairFault(t, "busy prepared callback", fixture.root, fixture.dependencies(), fixture.component)
	control := result.response.SupervisorControl
	if result.code != 1 || control.State != "blocked" || control.NextAction != "wait_for_legacy_work" || !errors.Is(result.err, pueue.ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("prepared callback did not preserve busy refusal: control=%+v err=%v", control, result.err)
	}
	assertConfigDriftPredecessorRetained(t, fixture, identities, status)
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(taskBefore, after) {
		t.Fatal("busy prepared callback changed committed task bytes")
	}
	assertHistoricalTaskEvidence(t, fixture)
	proposals, err := inspection.PreparedGroupMigrationSuccessors(t.Context(), fixture.store)
	if err != nil || len(proposals) != 1 || proposals[0] != selected {
		t.Fatalf("busy callback lost selected inspection B: proposals=%+v err=%v", proposals, err)
	}
	err = pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(context.Context) error {
		matches, matchErr := pueue.PrivateMigrationTargetsWithLock(fixture.root, selected)
		if matchErr != nil {
			return matchErr
		}
		if !matches {
			return errors.New("busy callback private intent lost selected B")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func finishCallbackMigrationFixture(t *testing.T, fixture *historicalSupervisorFixture, observerErrors <-chan error) {
	t.Helper()
	select {
	case observerErr := <-observerErrors:
		t.Fatal(observerErr)
	default:
	}
	ctx, cancel := supervisor.ControlContextWithParent(t.Context(), fixture.options)
	defer cancel()
	waits := fixture.component.daemonWaits()
	if err := fixture.component.stopDaemons(ctx, waits); err != nil {
		t.Fatal(err)
	}
	if err := awaitComponentDaemons(ctx, waits); err != nil {
		t.Fatal(err)
	}
}

func assertCallbackMigrationHasNoIntent(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	for _, directory := range []string{".supervisor", "inspection-group"} {
		if _, err := os.Stat(filepath.Join(fixture.root, directory, "migration.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("callback preparation created %s migration intent: %v", directory, err)
		}
	}
}

func TestPublicSupervisorMigrateKeepsPublishedCallbackSuccessor(t *testing.T) {
	fixture := newHistoricalSupervisorFixturePaths(t)
	legacy, observerErrors := prepareLiveCallbackMigration(t, fixture)
	selected := publishCallbackMigrationBeforeAliasCleanup(t, fixture, legacy)
	assertPublishedCallbackPredecessorRetained(t, fixture, selected, legacy)
	assertPublishedCallbackRecoveryPending(t, fixture)
	starts := readSupervisorProbeTestFile(t, fixture.daemonStartLog)
	historyPath := filepath.Join(fixture.root, ".supervisor", "migration-history.json")
	history := readSupervisorProbeTestFile(t, historyPath)
	taskBefore := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	paths := []string{
		fixture.statusPath, historyPath, selected.ConfigPath,
		filepath.Join(fixture.root, ".supervisor", "supervisor.identity.json"),
		filepath.Join(fixture.root, ".supervisor", "daemon.identity.json"),
		filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid"),
		filepath.Join(fixture.root, "inspection-group", "request.json"),
	}
	before := snapshotConfigDriftFiles(t, paths)
	result := migratePairFault(t, "published callback successor retry", fixture.root, fixture.dependencies(), fixture.component)
	control := result.response.SupervisorControl
	if result.code != 0 || result.err != nil || control.State != "migrated" || control.NextAction != "retry_dispatch" {
		t.Fatalf("published B retry did not finish existing private/group recovery: control=%+v err=%v", control, result.err)
	}
	assertLiveCallbackMigrationCompleted(t, fixture, legacy)
	if fixture.successor != selected || !maps.Equal(before, snapshotConfigDriftFiles(t, paths)) {
		t.Fatal("published B retry changed canonical B, queue, identity or original inspection provenance")
	}
	if !bytes.Equal(starts, readSupervisorProbeTestFile(t, fixture.daemonStartLog)) || !bytes.Equal(history, readSupervisorProbeTestFile(t, historyPath)) {
		t.Fatal("published B retry demoted the generation, started another daemon, or changed private history")
	}
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(taskBefore, after) {
		t.Fatal("published B retry changed committed task bytes")
	}
	assertHistoricalTaskEvidence(t, fixture)
	finishCallbackMigrationFixture(t, fixture, observerErrors)
}

func assertPublishedCallbackRecoveryPending(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	// Recovery alone cannot retire the historical alias. Explicit migration
	// must first let the existing private owner finish that committed edge.
	recoveryErr := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		return supervisor.RecoverInspectionGroupGenerationWithLock(ctx, fixture.store, fixture.current, false)
	})
	if !errors.Is(recoveryErr, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("published fixture did not retain the direct recovery precondition: %v", recoveryErr)
	}
}

func TestPublicSupervisorMigrateRefusesUnprovenPublishedCallbackHistory(t *testing.T) {
	for _, name := range []string{"missing history", "corrupt history"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newHistoricalSupervisorFixturePaths(t)
			legacy, observerErrors := prepareLiveCallbackMigration(t, fixture)
			selected := publishCallbackMigrationBeforeAliasCleanup(t, fixture, legacy)
			assertPublishedCallbackPredecessorRetained(t, fixture, selected, legacy)
			damageCallbackMigrationRecord(t, fixture, name)
			paths := append(configDriftDurableFiles(t, fixture), fixture.statusPath,
				filepath.Join(fixture.root, ".supervisor", "supervisor.identity.json"),
				filepath.Join(fixture.root, ".supervisor", "daemon.identity.json"),
				filepath.Join(fixture.root, ".supervisor", "migration-history.json"),
				filepath.Join(fixture.root, "inspection-group", "request.json"),
				filepath.Join(fixture.root, "inspection-group", "migration.json"))
			before := snapshotConfigDriftFiles(t, paths)
			result := migratePairFault(t, name, fixture.root, fixture.dependencies(), fixture.component)
			if result.code != 1 || !errors.Is(result.err, pueue.ErrPrivateSupervisorMigrationRequired) {
				t.Fatalf("unproven private history authorized callback retirement: control=%+v err=%v", result.response.SupervisorControl, result.err)
			}
			if !maps.Equal(before, snapshotConfigDriftFiles(t, paths)) {
				t.Fatal("history refusal changed canonical B, history, queue, task, alias or group intent")
			}
			assertPublishedCallbackPredecessorRetained(t, fixture, selected, legacy)
			assertHistoricalTaskEvidence(t, fixture)
			finishCallbackMigrationFixture(t, fixture, observerErrors)
		})
	}
}

func TestInspectionCallbackMigrationRefusesUnrelatedHead(t *testing.T) {
	fixture := newHistoricalSupervisorFixturePaths(t)
	prepareCommittedHistoricalCallbackTask(t, fixture)
	unrelated := fixture.predecessor
	unrelated.ConfigDigest = task.ComputeSHA256([]byte("unrelated inspection configuration\n"))
	// The group owner can store a valid request without granting private
	// generation authority. No migration/completion edge connects this head.
	group, err := inspection.OpenGroupContext(t.Context(), fixture.store, unrelated)
	if err != nil {
		t.Fatal(err)
	}
	if err = group.Close(); err != nil {
		t.Fatal(err)
	}
	head := historicalMigrationGroupHead(t, fixture)
	if head.Supervisor != unrelated || task.SameSupervisorIdentity(head.Supervisor, fixture.predecessor) {
		t.Fatal("fixture did not read a valid unrelated inspection head")
	}
	observerErrors := prepareMissingHistoricalCallbackPair(t, fixture, head)
	prepareCallbackMigrationCandidate(t, fixture, fixture.component.setupOptions(fixture.options))
	paths := append(configDriftDurableFiles(t, fixture), fixture.statusPath,
		filepath.Join(fixture.root, ".supervisor", "supervisor.identity.json"),
		filepath.Join(fixture.root, ".supervisor", "daemon.identity.json"),
		filepath.Join(fixture.root, "inspection-group", "request.json"),
		filepath.Join(fixture.root, "inspection-group", "migration.json"))
	before := snapshotConfigDriftFiles(t, paths)
	err = pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		return supervisor.MigrateInspectionSupervisorLocked(ctx, fixture.store, fixture.current, pueue.DefaultObservationTimeout)
	})
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) || errors.Is(err, inspection.ErrGroupMigrationRequired) {
		t.Fatalf("unrelated head bypassed the existing candidate recovery refusal: %v", err)
	}
	if !maps.Equal(before, snapshotConfigDriftFiles(t, paths)) {
		t.Fatal("unrelated head granted a migration intent or changed predecessor evidence")
	}
	assertCallbackMigrationHasNoIntent(t, fixture)
	assertHistoricalTaskEvidence(t, fixture)
	select {
	case observerErr := <-observerErrors:
		t.Fatal(observerErr)
	default:
	}
}

func TestInspectionCallbackMigrationRejectsDifferentPreparedInstallation(t *testing.T) {
	fixture := newHistoricalSupervisorFixturePaths(t)
	legacy, observerErrors := prepareLiveCallbackMigration(t, fixture)
	selected := publishCallbackMigrationBeforeAliasCleanup(t, fixture, legacy)
	status := readSupervisorProbeTestFile(t, fixture.statusPath)
	clientC, daemonC := installInspectionMigrationTargetC(t, fixture)
	// The existing retarget fixture initializes an empty status. Preserve the
	// original committed queue row before any migration consumer observes C.
	if err := os.WriteFile(fixture.statusPath, status, 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := preparePublishedCallbackCandidate(t, fixture, clientC, daemonC)
	if !task.SameSupervisorIdentity(selected, candidate.Binding()) || candidate.Binding().ClientExecutable == selected.ClientExecutable {
		t.Fatal("fixture did not prepare a different installed pair for the same logical config")
	}
	paths := append(configDriftDurableFiles(t, fixture), fixture.statusPath,
		filepath.Join(fixture.root, ".supervisor", "supervisor.identity.json"),
		filepath.Join(fixture.root, ".supervisor", "daemon.identity.json"),
		filepath.Join(fixture.root, ".supervisor", "migration-history.json"),
		filepath.Join(fixture.root, "inspection-group", "request.json"),
		filepath.Join(fixture.root, "inspection-group", "migration.json"))
	before := snapshotConfigDriftFiles(t, paths)
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		return supervisor.MigrateInspectionSupervisorLocked(ctx, fixture.store, candidate, pueue.DefaultObservationTimeout)
	})
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) || errors.Is(err, pueue.ErrBinding) {
		t.Fatalf("prepared installed C bypassed the existing candidate recovery refusal: %v", err)
	}
	if !maps.Equal(before, snapshotConfigDriftFiles(t, paths)) {
		t.Fatal("prepared C changed canonical B, history, queue, task, alias or group intent")
	}
	assertPublishedCallbackPredecessorRetained(t, fixture, selected, legacy)
	assertHistoricalTaskEvidence(t, fixture)
	finishCallbackMigrationFixture(t, fixture, observerErrors)
}

func TestInspectionCallbackMigrationAcceptsRefreshedVersionDiagnostic(t *testing.T) {
	fixture := newHistoricalSupervisorFixturePaths(t)
	legacy, observerErrors := prepareLiveCallbackMigration(t, fixture)
	selected := publishCallbackMigrationBeforeAliasCleanup(t, fixture, legacy)
	pid := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	source := historicalSupervisorClientScript("B", fixture.readyPath, pid, fixture.statusPath, fixture.commandLog)
	writeRunnerFixture(t, fixture.clientB, strings.ReplaceAll(source, "pueue 4.0.4", "pueue 4.0.5"))
	candidate := preparePublishedCallbackCandidate(t, fixture, fixture.clientB, fixture.daemonB)
	refreshed := candidate.Binding()
	if refreshed.ObservedVersion == selected.ObservedVersion {
		t.Fatal("fixture did not bind a valid refreshed version for the same installed logical target")
	}
	refreshed.ObservedVersion = selected.ObservedVersion
	if refreshed != selected {
		t.Fatal("refreshed candidate changed fields other than the version diagnostic")
	}
	starts := readSupervisorProbeTestFile(t, fixture.daemonStartLog)
	historyPath := filepath.Join(fixture.root, ".supervisor", "migration-history.json")
	history := readSupervisorProbeTestFile(t, historyPath)
	queue := readSupervisorProbeTestFile(t, fixture.statusPath)
	taskBefore := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	result := migratePairFault(t, "refreshed version public retry", fixture.root, fixture.dependencies(), fixture.component)
	control := result.response.SupervisorControl
	if result.code != 0 || result.err != nil || control.State != "migrated" || control.NextAction != "retry_dispatch" {
		t.Fatalf("refreshed version blocked public private/group retry: control=%+v err=%v", control, result.err)
	}
	assertLiveCallbackMigrationCompleted(t, fixture, legacy)
	if fixture.successor != selected || !bytes.Equal(starts, readSupervisorProbeTestFile(t, fixture.daemonStartLog)) {
		t.Fatal("version refresh changed canonical B or started a daemon")
	}
	if !bytes.Equal(history, readSupervisorProbeTestFile(t, historyPath)) || !bytes.Equal(queue, readSupervisorProbeTestFile(t, fixture.statusPath)) {
		t.Fatal("version refresh changed private history or the original queue")
	}
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(taskBefore, after) {
		t.Fatal("version refresh changed original task bytes")
	}
	assertHistoricalTaskEvidence(t, fixture)
	finishCallbackMigrationFixture(t, fixture, observerErrors)
}

func preparePublishedCallbackCandidate(t *testing.T, fixture *historicalSupervisorFixture, client, daemon string) *pueue.Client {
	t.Helper()
	before := len(fixture.component.daemonWaits())
	var candidate *pueue.Client
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		var prepareErr error
		candidate, prepareErr = pueue.PreparePrivateForMigrationWithLock(ctx, client, daemon, fixture.root, fixture.component.setupOptions(fixture.options))
		fixture.component.ownClient(candidate, prepareErr)
		return prepareErr
	})
	fixture.component.ownDaemonClients(candidate, before)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.component.daemonWaits()) != before {
		t.Fatal("binding a published callback candidate started a daemon")
	}
	return candidate
}

func assertPublishedCallbackPredecessorRetained(t *testing.T, fixture *historicalSupervisorFixture, selected task.SupervisorRef, legacy inspection.GroupRequestRecord) {
	t.Helper()
	published, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found || published != selected || task.SameSupervisorIdentity(legacy.Supervisor, published) {
		t.Fatalf("fixture did not publish B beyond original head A: published=%+v found=%t err=%v", published, found, err)
	}
	alias, found, err := pueue.ReadPrivateRecoveryBinding(fixture.root)
	if err != nil || !found || !task.SameSupervisorIdentity(alias, legacy.Supervisor) {
		t.Fatalf("fixture did not retain original alias A: alias=%+v found=%t err=%v", alias, found, err)
	}
	head, migrated, err := inspection.ReadGroupHead(t.Context(), fixture.store)
	if err != nil || migrated || head != legacy {
		t.Fatalf("fixture advanced the original head before retry: head=%+v migrated=%t err=%v", head, migrated, err)
	}
}

func publishCallbackMigrationBeforeAliasCleanup(t *testing.T, fixture *historicalSupervisorFixture, legacy inspection.GroupRequestRecord) task.SupervisorRef {
	t.Helper()
	cleanupFault := errors.New("injected callback alias retirement fault")
	var selected task.SupervisorRef
	options := fixture.component.setupOptions(fixture.options)
	options.PrivateGenerationGuard = infrastructurePreparationGuard(func(ctx context.Context, root string, commit func() error) error {
		published, found, err := pueue.ReadPrivateSupervisorBinding(root)
		if err != nil {
			return err
		}
		pending, err := pueue.PrivateSupervisorMigrationPending(root)
		if err != nil {
			return err
		}
		aliasPresent, err := pueue.PrivateRecoveryBindingPresent(root)
		if err != nil {
			return err
		}
		if found && published == selected && !pending && aliasPresent {
			return cleanupFault
		}
		return fixture.component.GuardPrivateGenerationCommit(ctx, root, commit)
	})
	selected = prepareCallbackMigrationCandidate(t, fixture, options)
	before := len(fixture.component.daemonWaits())
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		successor := supervisor.InspectionGroup(fixture.store.RootID, selected)
		if prepareErr := inspection.PrepareGroupMigration(ctx, fixture.store, legacy, successor); prepareErr != nil {
			return prepareErr
		}
		return fixture.current.MigratePrivateSupervisorWithLock(ctx, fixture.root)
	})
	fixture.component.ownClient(fixture.current, err)
	fixture.component.ownDaemonClients(fixture.current, before)
	if !errors.Is(err, cleanupFault) {
		t.Fatalf("fixture did not reach actual post-publication alias retirement: %v", err)
	}
	if pending, pendingErr := pueue.PrivateSupervisorMigrationPending(fixture.root); pendingErr != nil || pending {
		t.Fatalf("published fixture still has private intent: pending=%t err=%v", pending, pendingErr)
	}
	if pending, pendingErr := inspection.GroupMigrationPending(t.Context(), fixture.store); pendingErr != nil || !pending {
		t.Fatalf("published fixture lost the original pending group edge: pending=%t err=%v", pending, pendingErr)
	}
	return selected
}

func TestInspectionCallbackMigrationRefusesUntrustedPredecessorState(t *testing.T) {
	for _, name := range []string{"different alias identity", "corrupt head", "corrupt private binding", "corrupt alias"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newHistoricalSupervisorFixturePaths(t)
			_, observerErrors := prepareLiveCallbackMigration(t, fixture)
			prepareCallbackMigrationCandidate(t, fixture, fixture.component.setupOptions(fixture.options))
			damageCallbackMigrationRecord(t, fixture, name)
			paths := append(configDriftDurableFiles(t, fixture),
				fixture.statusPath, fixture.predecessor.ConfigPath,
				filepath.Join(fixture.root, "inspection-group", "request.json"),
				filepath.Join(fixture.root, "inspection-group", "migration.json"),
				filepath.Join(fixture.root, ".supervisor", "supervisor.identity.json"),
				filepath.Join(fixture.root, ".supervisor", "daemon.identity.json"))
			before := snapshotConfigDriftFiles(t, paths)
			err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
				return supervisor.MigrateInspectionSupervisorLocked(ctx, fixture.store, fixture.current, pueue.DefaultObservationTimeout)
			})
			assertUntrustedCallbackMigrationError(t, name, err)
			if after := snapshotConfigDriftFiles(t, paths); !maps.Equal(before, after) {
				t.Fatal("refused callback migration changed task, queue, generation, alias or migration intent")
			}
			assertCallbackMigrationHasNoIntent(t, fixture)
			assertHistoricalTaskEvidence(t, fixture)
			select {
			case observerErr := <-observerErrors:
				t.Fatal(observerErr)
			default:
			}
		})
	}
}

func damageCallbackMigrationRecord(t *testing.T, fixture *historicalSupervisorFixture, name string) {
	t.Helper()
	path := filepath.Join(fixture.root, ".supervisor", "legacy-supervisor.json")
	switch name {
	case "corrupt head":
		path = filepath.Join(fixture.root, "inspection-group", "request.json")
	case "corrupt private binding":
		path = filepath.Join(fixture.root, ".supervisor", "supervisor.identity.json")
	case "missing history", "corrupt history":
		path = filepath.Join(fixture.root, ".supervisor", "migration-history.json")
	}
	original := readSupervisorProbeTestFile(t, path)
	t.Cleanup(func() {
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Error(err)
		}
	})
	if name == "missing history" {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return
	}
	replacement := []byte("not-json\n")
	if name == "different alias identity" {
		var record struct {
			SchemaVersion int                `json:"schema_version"`
			Supervisor    task.SupervisorRef `json:"supervisor"`
			ConfigPath    string             `json:"config_path"`
			ConfigSHA256  string             `json:"config_sha256"`
		}
		if err := task.DecodeStrict(original, &record); err != nil {
			t.Fatal(err)
		}
		config := readSupervisorProbeTestFile(t, record.ConfigPath)
		record.Supervisor.ConfigDigest = task.ComputeSHA256(append(config, []byte("\n# distinct historical config\n")...))
		var err error
		replacement, err = task.MarshalCanonical(record)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if name == "different alias identity" {
		alias, found, err := pueue.ReadPrivateRecoveryBinding(fixture.root)
		if err != nil || !found || task.SameSupervisorIdentity(alias, fixture.predecessor) {
			t.Fatalf("fixture did not create a valid different-identity alias: found=%t err=%v", found, err)
		}
	}
}

func assertUntrustedCallbackMigrationError(t *testing.T, name string, err error) {
	t.Helper()
	if name == "different alias identity" {
		if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
			t.Fatalf("different-identity alias bypassed canonical refusal: %v", err)
		}
		return
	}
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("corrupt predecessor record lost its read error: %v", err)
	}
}

func prepareLiveCallbackMigration(t *testing.T, fixture *historicalSupervisorFixture) (inspection.GroupRequestRecord, <-chan error) {
	t.Helper()
	prepareCommittedHistoricalCallbackTask(t, fixture)
	writeSupervisorGenerationTestGroup(t, fixture.store, fixture.predecessor)
	head := historicalMigrationGroupHead(t, fixture)
	return head, prepareMissingHistoricalCallbackPair(t, fixture, head)
}

func prepareCommittedHistoricalCallbackTask(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	workspace, _ := prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = withDefaultSupervisorCallbacks(pueue.Options{
		ObservationTimeout:         pueue.DefaultObservationTimeout,
		Environment:                pueue.DefaultEnvironment(),
		PrivateTerminalEventRunner: fixture.runner,
	})
	prepareHistoricalSupervisorTask(t, fixture, workspace)
	outcome, err := sealFixtureEvidence(fixture.td, task.FixturePredicateRef(), "completed migration fixture\n", func() error { return nil })
	if err != nil || outcome == nil || outcome.Verdict != task.VerdictCommitted {
		t.Fatalf("commit original admitted task: outcome=%+v err=%v", outcome, err)
	}
}

func prepareMissingHistoricalCallbackPair(t *testing.T, fixture *historicalSupervisorFixture, head inspection.GroupRequestRecord) <-chan error {
	t.Helper()
	observerErrors := observeStoppedInstalledMigrationGroup(t, fixture, head.Name)
	writeCommittedHistoricalMigrationStatus(t, fixture)
	if err := addHistoricalInspectionGroupStatus(fixture.statusPath, head.Name); err != nil {
		t.Fatal(err)
	}
	previous := fixture.component.daemonWaits()
	if len(previous) != 1 {
		t.Fatalf("live predecessor Wait count=%d, want one actual Start", len(previous))
	}
	select {
	case <-previous[0].done:
		t.Fatal("predecessor exited before callback migration")
	default:
	}
	prepareLiveCallbackMigrationCleanup(t, fixture, previous[0])
	callbackB := filepath.Join(filepath.Dir(fixture.clientB), "delegate-run")
	writeRunnerFixture(t, callbackB, "#!/bin/sh\nexit 0\n")
	fixture.options.PrivateTerminalEventRunner = callbackB
	for _, path := range []string{fixture.clientA, fixture.daemonA} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	return observerErrors
}

func writeCommittedHistoricalMigrationStatus(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	writeEndedHistoricalMigrationStatus(t, fixture)
	snapshot, err := fixture.predecessorClient.Snapshot(t.Context())
	if err != nil || len(snapshot.Jobs) != 1 || snapshot.Jobs[0].ID != fixture.receipt.NumericTaskID || snapshot.Jobs[0].State != pueue.StateEnded {
		t.Fatalf("original committed task has no exact ended queue row: snapshot=%+v err=%v", snapshot, err)
	}
	if err = authorizePrivateMigrationQueue(t.Context(), fixture.root, fixture.predecessor, snapshot.Jobs); err != nil {
		t.Fatalf("canonical policy rejected committed predecessor task: %v", err)
	}
}

func writeEndedHistoricalMigrationStatus(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	writeRunnerStatus(t, fixture.statusPath, fixture.req.RootID, fixture.req.TaskID, fixture.receipt.NumericTaskID)
	var status struct {
		Tasks  map[string]map[string]json.RawMessage `json:"tasks"`
		Groups map[string]json.RawMessage            `json:"groups"`
	}
	if err := json.Unmarshal(readSupervisorProbeTestFile(t, fixture.statusPath), &status); err != nil {
		t.Fatal(err)
	}
	status.Tasks["7"]["status"] = json.RawMessage(`{"Done":{"enqueued_at":"2026-09-14T00:00:00.123456789Z","start":"2026-09-14T00:00:00.123456789Z","end":"2026-09-14T00:00:01.123456789Z","result":"Success"}}`)
	status.Groups["default"] = json.RawMessage(`{"status":"Running","parallel_tasks":0}`)
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(fixture.statusPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func prepareLiveCallbackMigrationCleanup(t *testing.T, fixture *historicalSupervisorFixture, previous componentDaemonCompletion) {
	t.Helper()
	options := fixture.options
	options.RuntimeClientExecutable = fixture.clientB
	transport, err := pueue.NewClient(fixture.predecessor, options)
	fixture.component.ownClient(transport, err)
	if err != nil {
		t.Fatal(err)
	}
	fixture.component.ownDaemonClients(transport, 0)
	config := readSupervisorProbeTestFile(t, fixture.predecessor.ConfigPath)
	t.Cleanup(func() {
		select {
		case <-previous.done:
			return
		default:
		}
		// A failed callback preparation may have replaced the config before
		// retiring A. Restore only the fixture-owned bytes needed by A's
		// retained shutdown client; the component then stops and joins A.
		if restoreErr := os.WriteFile(fixture.predecessor.ConfigPath, config, 0o600); restoreErr != nil {
			t.Error(restoreErr)
		}
	})
}

func assertLiveCallbackMigrationCompleted(t *testing.T, fixture *historicalSupervisorFixture, legacy inspection.GroupRequestRecord) {
	t.Helper()
	active, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found {
		t.Fatalf("read callback successor: found=%t err=%v", found, err)
	}
	assertInstalledSupervisorPair(t, active, fixture.clientB, fixture.daemonB)
	if active.ConfigDigest == fixture.predecessor.ConfigDigest || task.SameSupervisorIdentity(active, fixture.predecessor) {
		t.Fatal("callback replacement did not change logical supervisor identity")
	}
	fixture.successor = active
	assertHistoricalSupervisorState(t, fixture)
	successor := supervisor.InspectionGroup(fixture.store.RootID, active)
	complete, err := inspection.GroupMigrationComplete(t.Context(), fixture.store, legacy, successor)
	if err != nil || !complete {
		t.Fatalf("callback migration did not complete original inspection edge: complete=%t err=%v", complete, err)
	}
	head, migrated, err := inspection.ReadGroupHead(t.Context(), fixture.store)
	if err != nil || !migrated || head != successor {
		t.Fatalf("callback migration published wrong inspection head: migrated=%t head=%+v err=%v", migrated, head, err)
	}
	if pending, pendingErr := inspection.GroupMigrationPending(t.Context(), fixture.store); pendingErr != nil || pending {
		t.Fatalf("callback migration left a group proposal pending: pending=%t err=%v", pending, pendingErr)
	}
	assertCallbackMigrationAliasRetired(t, fixture.root)
}

func assertCallbackMigrationAliasRetired(t *testing.T, root string) {
	t.Helper()
	if present, aliasErr := pueue.PrivateRecoveryBindingPresent(root); aliasErr != nil || present {
		t.Fatalf("callback migration retained predecessor alias: present=%t err=%v", present, aliasErr)
	}
	for _, name := range []string{"legacy-pueue.yml", "legacy-supervisor.json", "legacy-pueue.pending", "legacy-supervisor.pending.json"} {
		if _, aliasErr := os.Stat(filepath.Join(root, ".supervisor", name)); !errors.Is(aliasErr, os.ErrNotExist) {
			t.Fatalf("callback migration retained alias artifact %s: %v", name, aliasErr)
		}
	}
}

func observeStoppedInstalledMigrationGroup(t *testing.T, fixture *historicalSupervisorFixture, groupName string) <-chan error {
	t.Helper()
	if err := os.WriteFile(fixture.statusPath, []byte(`{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	observerErrors := make(chan error, 1)
	fixture.options.Observer = func(event pueue.CommandEvent) {
		if event.Stage != "completed" {
			return
		}
		var observationErr error
		if historicalCommandSequence(event.Argv, "group", "add") {
			// The finite client does not persist group changes. Reuse the
			// established status update at its actual command boundary.
			observationErr = addHistoricalInspectionGroupStatus(fixture.statusPath, groupName)
		} else if !historicalCommandSequence(event.Argv, "--help") && (historicalCommandSequence(event.Argv, "add") || historicalCommandSequence(event.Argv, "enqueue")) {
			observationErr = errors.New("explicit migration replayed an ordinary task queue mutation")
		}
		if observationErr != nil {
			select {
			case observerErrors <- observationErr:
			default:
			}
		}
	}
	return observerErrors
}

func assertStoppedInstalledMigrationCompleted(t *testing.T, fixture *historicalSupervisorFixture, head inspection.GroupRequestRecord) {
	t.Helper()
	active, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found {
		t.Fatalf("read completed private successor: found=%t err=%v", found, err)
	}
	assertInstalledSupervisorPair(t, active, fixture.clientB, fixture.daemonB)
	fixture.successor = active
	if complete, completeErr := pueue.PrivateSupervisorMigrationComplete(fixture.root, active); completeErr != nil || !complete {
		t.Fatalf("installed B migration incomplete: complete=%t err=%v", complete, completeErr)
	}
	assertHistoricalActiveDaemon(t, fixture)
	assertHistoricalMigrationHistory(t, fixture)
	assertHistoricalMigrationJournalRetired(t, fixture)
	// Installation-only replacement retains the exact inspection request:
	// its owner's logical group identity ignores executable coordinates.
	assertHistoricalMigrationGroupUnchanged(t, fixture, head)
	present, matches, err := inspection.GroupHeadMatchesBinding(t.Context(), fixture.store, active)
	if err != nil || !present || !matches {
		t.Fatalf("original inspection provenance does not match B: present=%t matches=%t err=%v", present, matches, err)
	}
	client, err := pueue.NewClient(active, fixture.dependencies().SupervisorOptions)
	fixture.component.ownClient(client, err)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Ready(t.Context()); err != nil {
		t.Fatalf("committed B does not accept its status protocol: %v", err)
	}
	assertHistoricalTaskEvidence(t, fixture)
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
