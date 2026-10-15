package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestMigrationPreparesInstalledPair(t *testing.T) {
	store := newBareInspectionStore(t)
	bundle := canonicalAppTestTempDir(t)
	clientPath, daemonPath := filepath.Join(bundle, "pueue"), filepath.Join(bundle, "pueued")
	writeRunnerFixture(t, clientPath, "#!/bin/sh\nprintf 'pueue 4.0.4\\n'\n")
	writeRunnerFixture(t, daemonPath, "#!/bin/sh\nexit 0\n")
	var prepared *pueue.Client
	err := pueue.WithBootstrapLockContext(t.Context(), store.Root, func(ctx context.Context) error {
		var prepareErr error
		prepared, prepareErr = prepareCurrentPrivateSupervisor(ctx, store.Root, Dependencies{InitialSupervisorExecutable: clientPath}, pueue.Options{})
		return prepareErr
	})
	settleSupervisorProbeTestClient(t, prepared, err)
	if err != nil {
		t.Fatal(err)
	}
	binding := prepared.Binding()
	client, daemon, err := resolveRetainedSupervisorPair(store.Root, binding)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ClientExecutable != client || binding.DaemonExecutable != daemon || client == clientPath || daemon == daemonPath {
		t.Fatalf("migration bound installation paths instead of installed pair: %+v", binding)
	}
	// Staging a newer inventory must not change the exact returned generation.
	writeRunnerFixture(t, daemonPath, "#!/bin/sh\nexit 0\n# newer\n")
	if err = installStateSupervisorPair(t.Context(), store.Root, clientPath, daemonPath); err != nil {
		t.Fatal(err)
	}
	gotClient, gotDaemon, err := resolveRetainedSupervisorPair(store.Root, binding)
	if err != nil || gotClient != client || gotDaemon != daemon {
		t.Fatalf("mutable inventory redirected retained pair: %s %s %v", gotClient, gotDaemon, err)
	}
}

func TestAdmissionReconcilesPublishedPair(t *testing.T) {
	store, deps, originalClient := newSupervisorProbeTestFixture(t)
	original := originalClient.Binding()
	clientPath, daemonPath, err := installStateSupervisorPairPaths(t.Context(), store.Root, original.ClientExecutable, original.DaemonExecutable)
	if err != nil {
		t.Fatal(err)
	}
	candidate := original
	candidate.ClientExecutable, candidate.DaemonExecutable = clientPath, daemonPath
	writeSupervisorGenerationTestGroup(t, store, candidate)
	identityPath := filepath.Join(filepath.Dir(original.ConfigPath), "daemon.identity.json")
	identityData, err := task.MarshalCanonical(historicalPrivateDaemonIdentityFor(candidate))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(identityPath, identityData, 0o600); err != nil {
		t.Fatal(err)
	}
	var repaired *pueue.Client
	err = pueue.WithBootstrapLockContext(t.Context(), store.Root, func(ctx context.Context) error {
		var prepareErr error
		repaired, prepareErr = preparePrivateAdmissionTransition(ctx, deps, store, deps.SupervisorOptions)
		return prepareErr
	})
	settleSupervisorProbeTestClient(t, repaired, err)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Binding() != candidate {
		t.Fatalf("admission did not bind repaired active generation: %+v", repaired.Binding())
	}
	after, err := os.ReadFile(identityPath)
	if err != nil || !bytes.Equal(after, identityData) {
		t.Fatalf("admission rewrote launch provenance: %v", err)
	}
	starts, err := os.ReadFile(filepath.Join(filepath.Dir(original.ConfigPath), "daemon-started"))
	if err != nil || string(starts) != "started\n" {
		t.Fatalf("admission relaunched provider supervisor: %q %v", starts, err)
	}
}

func TestSupervisorFailureReasonsPreserveCausesAndRedactPaths(t *testing.T) {
	cause := errors.Join(pueue.ErrBinding, errors.New("secret /private/user/config"))
	for _, reason := range []string{"identity_conflict", "transition_pending", "published_pair_unavailable_live", "predecessor_pair_unavailable", "active_work", "observation_failed", "state_unreadable"} {
		err := &pueue.PrivateRecoveryError{ReasonCode: reason, Err: cause}
		failure := dispatchFailure(withDispatchStage("start-supervisor", errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, err)), "not_created")
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
	client, _, err := installStateSupervisorPairPaths(t.Context(), store.Root, current.Binding().ClientExecutable, current.Binding().DaemonExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(filepath.Dir(client), 0o755); err != nil {
		t.Fatal(err)
	}
	var admitted *pueue.Client
	err = pueue.WithBootstrapLockContext(t.Context(), store.Root, func(ctx context.Context) error {
		var prepareErr error
		admitted, prepareErr = preparePrivateAdmissionTransition(ctx, deps, store, deps.SupervisorOptions)
		return prepareErr
	})
	settleSupervisorProbeTestClient(t, admitted, err)
	if err != nil || admitted.Binding() != current.Binding() {
		t.Fatalf("healthy published generation depended on unused inventory: %v", err)
	}
}

func TestMissingInstallationDoesNotSuggestSupervisorMigration(t *testing.T) {
	cause := withDispatchStage("prepare-supervisor-executables", &os.PathError{Op: "stat", Path: "/missing/private/pueued", Err: os.ErrNotExist})
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
	store, deps, current := newSupervisorProbeTestFixture(t)
	original := current.Binding()
	client, daemon, err := installStateSupervisorPairPaths(t.Context(), store.Root, original.ClientExecutable, original.DaemonExecutable)
	if err != nil {
		t.Fatal(err)
	}
	target := original
	target.ClientExecutable, target.DaemonExecutable = client, daemon
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
	if err = os.Remove(client); err != nil {
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

func TestSavedSupervisorReconciliationBoundsBackgroundLockWait(t *testing.T) {
	store, _, _ := newSupervisorProbeTestFixture(t)
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- pueue.WithBootstrapLockContext(t.Context(), store.Root, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	t.Cleanup(func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	err := reconcileSavedSupervisor(context.Background(), store.Root, pueue.Options{ObservationTimeout: time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("background recovery did not receive a finite deadline: %v", err)
	}
}

func TestSupervisorMigrateReplacesRemovedPackagePair(t *testing.T) {
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
	err = pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		var prepareErr error
		successor, prepareErr = prepareCurrentPrivateSupervisor(ctx, fixture.root, fixture.dependencies(), fixture.options)
		if prepareErr != nil {
			return prepareErr
		}
		if prepareErr = recoverInspectionGroupGenerationWithLock(ctx, fixture.store, successor, false); prepareErr != nil {
			return prepareErr
		}
		return migrateInspectionSupervisorLocked(ctx, fixture.store, successor, pueue.DefaultObservationTimeout)
	})
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

func TestExplicitMigrationResumesPrivateJournal(t *testing.T) {
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
	for _, path := range []string{fixture.clientA, fixture.clientB} {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		old := `"default":{"status":"Running","parallel_tasks":0}`
		groups := old + `,"delegation-inspection-` + rootID + `":{"status":"Running","parallel_tasks":1}`
		writeRunnerFixture(t, path, strings.ReplaceAll(string(data), old, groups))
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
		current, prepareErr := prepareCurrentPrivateSupervisor(ctx, fixture.root, fixture.dependencies(), blocked)
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
	result := supervisorMigrate(fixture.root, fixture.dependencies(), newSupervisorTestResponse())
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
			current, prepareErr = prepareCurrentPrivateSupervisor(ctx, fixture.root, fixture.dependencies(), blocked)
		}
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
		firstProposal.Supervisor.DaemonSHA256 = task.ComputeSHA256([]byte("earlier prepared daemon"))
		if prepareErr = inspection.PrepareGroupMigration(ctx, fixture.store, scenario.legacyGroup, firstProposal); prepareErr != nil {
			return prepareErr
		}
		if prepareErr = inspection.PrepareGroupMigration(ctx, fixture.store, scenario.legacyGroup, scenario.successorGroup); prepareErr != nil {
			return prepareErr
		}
		return current.MigratePrivateSupervisorWithLock(ctx, fixture.root)
	})
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("fixture did not create both pending journals: %v", err)
	}
}

func TestExplicitMigrationFinishesRecordedInspectionSuccessor(t *testing.T) {
	scenario := newHistoricalInspectionMigrationScenario(t)
	fixture := scenario.fixture
	prepareInterruptedInspectionMigration(t, scenario, false)
	for _, path := range []string{fixture.clientB, fixture.daemonB} {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		writeRunnerFixture(t, path, string(data)+"\n# later installation\n")
	}
	status := `{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0},"delegation-inspection-` + fixture.store.RootID + `":{"status":"Running","parallel_tasks":1}}}`
	if err := os.WriteFile(fixture.statusPath, []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	result := supervisorMigrate(fixture.root, fixture.dependencies(), newSupervisorTestResponse())
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

func TestExplicitMigrationRetargetsMissingPreShutdownSuccessor(t *testing.T) {
	scenario := newHistoricalInspectionMigrationScenario(t)
	fixture := scenario.fixture
	prepareInterruptedInspectionMigration(t, scenario, true)
	oldClient, oldDaemon := fixture.clientB, fixture.daemonB
	directory := canonicalAppTestTempDir(t)
	for _, source := range []string{oldClient, oldDaemon} {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		writeRunnerFixture(t, filepath.Join(directory, filepath.Base(source)), string(data)+"\n# replacement installation\n")
	}
	fixture.clientB, fixture.daemonB = filepath.Join(directory, "pueue"), filepath.Join(directory, "pueued")
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		current, prepareErr := prepareCurrentPrivateSupervisor(ctx, fixture.root, fixture.dependencies(), fixture.options)
		if prepareErr != nil {
			return prepareErr
		}
		if prepareErr = fixture.current.Close(); prepareErr != nil {
			return errors.Join(prepareErr, current.Close())
		}
		fixture.current = current
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{oldClient, oldDaemon} {
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	status := `{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0},"delegation-inspection-` + fixture.store.RootID + `":{"status":"Running","parallel_tasks":1}}}`
	if err = os.WriteFile(fixture.statusPath, []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	result := supervisorMigrate(fixture.root, fixture.dependencies(), newSupervisorTestResponse())
	if result.err != nil || result.code != 0 {
		t.Fatalf("pre-shutdown removed successor could not safely retarget: %v", result.err)
	}
	active, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found || active != fixture.current.Binding() {
		t.Fatalf("replacement installation was not committed: %+v %v", active, err)
	}
}

func TestInspectionMigrationUsesActivePredecessorAfterPathRelocation(t *testing.T) {
	scenario := newHistoricalInspectionMigrationScenario(t)
	fixture := scenario.fixture
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		if writeErr := os.WriteFile(scenario.configPath, scenario.config, 0o600); writeErr != nil {
			return writeErr
		}
		client, daemon, prepareErr := installStateSupervisorPairPaths(ctx, fixture.root, fixture.clientA, fixture.daemonA)
		if prepareErr != nil {
			return prepareErr
		}
		relocated, prepareErr := pueue.PreparePrivateForMigrationWithLock(ctx, client, daemon, fixture.root, fixture.options)
		if prepareErr != nil {
			return prepareErr
		}
		if prepareErr = fixture.current.Close(); prepareErr != nil {
			return errors.Join(prepareErr, relocated.Close())
		}
		fixture.current = relocated
		if prepareErr = relocated.MigratePrivateSupervisorWithLock(ctx, fixture.root); prepareErr != nil {
			return prepareErr
		}
		if prepareErr = os.WriteFile(scenario.configPath, scenario.changedConfig, 0o600); prepareErr != nil {
			return prepareErr
		}
		successor, prepareErr := prepareCurrentPrivateSupervisor(ctx, fixture.root, fixture.dependencies(), fixture.options)
		if prepareErr != nil {
			return prepareErr
		}
		if prepareErr = relocated.Close(); prepareErr != nil {
			return errors.Join(prepareErr, successor.Close())
		}
		fixture.current = successor
		scenario.successorGroup.Supervisor = successor.Binding()
		return migrateInspectionGroup(ctx, fixture.store, successor, pueue.DefaultObservationTimeout, scenario.legacyGroup)
	})
	if err != nil {
		t.Fatalf("retained inspection head could not follow the actual private predecessor: %v", err)
	}
	complete, err := inspection.GroupMigrationComplete(t.Context(), fixture.store, scenario.legacyGroup, scenario.successorGroup)
	if err != nil || !complete {
		t.Fatalf("inspection edge was not committed after relocation: %t %v", complete, err)
	}
}
