package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// newPublishedConfigDriftFixture publishes the predecessor pair with the
// canonical queue authorizer, stashes its task row, and retargets the private
// config endpoint under the transition lock, as the canonical upgrade does.
func newPublishedConfigDriftFixture(t *testing.T) *historicalSupervisorFixture {
	t.Helper()
	fixture := newHistoricalSupervisorFixturePaths(t)
	workspace, _ := prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = newHistoricalSupervisorOptions()
	prepareHistoricalSupervisorTask(t, fixture, workspace)
	fixture.options.PrivateMigrationAuthorizer = authorizePrivateMigrationQueue
	writeSupervisorGenerationTestGroup(t, fixture.store, fixture.predecessor)
	writeRunnerStashedStatus(t, fixture.statusPath, fixture.req.RootID, fixture.req.TaskID, 7)
	configPath := pueue.PrivateConfigPath(fixture.root)
	original := readSupervisorProbeTestFile(t, configPath)
	// The fixture's shutdown uses the predecessor client, which refuses the
	// drifted config; restore its bytes first. Cleanups run in reverse order.
	t.Cleanup(func() {
		if err := os.WriteFile(configPath, original, 0o600); err != nil {
			t.Error(err)
		}
	})
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(context.Context) error {
		data, readErr := os.ReadFile(configPath)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(configPath, retargetedPrivateSocketConfig(t, data, fixture.predecessor), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func publishedPairIdentityFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range []string{"supervisor.identity.json", "daemon.identity.json"} {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(pueue.PrivateConfigPath(root)), name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = data
	}
	return files
}

// Explicit migration owns a published pair whose verified executables are
// unchanged but whose config endpoint was retargeted. With a stashed row it
// is blocked by the queue authorizer, keeping the row and the published
// identities; after the row drains the same command commits the successor.
func TestExplicitMigrationOwnsPublishedConfigDrift(t *testing.T) {
	fixture := newPublishedConfigDriftFixture(t)
	identities := publishedPairIdentityFiles(t, fixture.root)
	status := readSupervisorProbeTestFile(t, fixture.statusPath)
	blocked := supervisorMigrate(fixture.root, fixture.dependencies(), newSupervisorTestResponse())
	control := blocked.response.SupervisorControl
	if blocked.code != 1 || control.State != "blocked" || control.ActiveWork != "present" || control.NextAction != "wait_for_legacy_work" || !errors.Is(blocked.err, pueue.ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("config drift with a stashed row: code=%d control=%+v err=%v", blocked.code, control, blocked.err)
	}
	assertConfigDriftPredecessorRetained(t, fixture, identities, status)
	assertConfigDriftPublicConsumers(t, fixture)
	assertConfigDriftPredecessorRetained(t, fixture, identities, status)
	drained := `{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0},"delegation-inspection-` + fixture.store.RootID + `":{"status":"Running","parallel_tasks":1}}}`
	if err := os.WriteFile(fixture.statusPath, []byte(drained), 0o600); err != nil {
		t.Fatal(err)
	}
	migrated := supervisorMigrate(fixture.root, fixture.dependencies(), newSupervisorTestResponse())
	if migrated.code != 0 || migrated.err != nil || migrated.response.SupervisorControl.State != "migrated" {
		t.Fatalf("drained config-drift migration: code=%d control=%+v err=%v", migrated.code, migrated.response.SupervisorControl, migrated.err)
	}
	assertConfigDriftSuccessor(t, fixture)
}

// A blocked migration keeps the published binding and identities, the exact
// stashed row, and the running predecessor.
func assertConfigDriftPredecessorRetained(t *testing.T, fixture *historicalSupervisorFixture, identities map[string][]byte, status []byte) {
	t.Helper()
	published, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found || published != fixture.predecessor {
		t.Fatalf("blocked migration changed the published binding: %+v found=%t err=%v", published, found, err)
	}
	for name, data := range publishedPairIdentityFiles(t, fixture.root) {
		if !bytes.Equal(data, identities[name]) {
			t.Fatalf("blocked migration rewrote %s", name)
		}
	}
	if after := readSupervisorProbeTestFile(t, fixture.statusPath); !bytes.Equal(after, status) {
		t.Fatal("blocked migration changed the stashed row")
	}
	if commands := string(readSupervisorProbeTestFile(t, fixture.commandLog)); strings.Contains(commands, "shutdown") || strings.Contains(commands, "kill") {
		t.Fatalf("blocked migration stopped the predecessor: %q", commands)
	}
}

func assertConfigDriftSuccessor(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	successor, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
	if err != nil || !found {
		t.Fatalf("successor not published: found=%t err=%v", found, err)
	}
	config := readSupervisorProbeTestFile(t, successor.ConfigPath)
	clientB := readSupervisorProbeTestFile(t, fixture.clientB)
	if successor == fixture.predecessor || successor.Endpoint != "unix:"+retargetedPrivateSocketPath(t, fixture.predecessor) || successor.ConfigDigest != task.ComputeSHA256(config) || successor.ClientSHA256 != task.ComputeSHA256(clientB) {
		t.Fatalf("successor does not bind the drifted config and installed client: %+v", successor)
	}
}

// Callers other than explicit migration keep the exact strict refusal for
// the same drifted state: saved reconciliation and admission preparation.
func TestPublishedConfigDriftKeepsStrictRefusalOutsideMigration(t *testing.T) {
	fixture := newPublishedConfigDriftFixture(t)
	identities := publishedPairIdentityFiles(t, fixture.root)
	want := "published private supervisor pair cannot be recovered"
	saved := supervisor.ReconcileSaved(t.Context(), fixture.root, fixture.options, openNativeSupervisorStore)
	var admitted *pueue.Client
	admission := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		var prepareErr error
		admitted, prepareErr = supervisor.PreparePrivateAdmissionTransition(ctx, supervisor.ExecutableLookup{InitialExecutable: fixture.clientB}, fixture.store, fixture.options)
		return prepareErr
	})
	if admitted != nil {
		t.Fatalf("admission bound a drifted published pair: %+v", admitted.Binding())
	}
	for name, err := range map[string]error{"saved reconcile": saved, "admission": admission} {
		if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) || !errors.Is(err, pueue.ErrBinding) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s lost its strict refusal: %v", name, err)
		}
	}
	for name, data := range publishedPairIdentityFiles(t, fixture.root) {
		if !bytes.Equal(data, identities[name]) {
			t.Fatalf("strict refusal rewrote %s", name)
		}
	}
}

// In the same blocked, endpoint-retargeted, prepared state, the public status
// command observes the saved task through its validated recovery alias, and a
// public dispatch is refused with migration guidance before any task record.
// Neither consumer changes task bytes, the journal, the alias, the daemon
// start log or the PID file, and alias retirement stays refused.
func assertConfigDriftPublicConsumers(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	assertConfigDriftPreparedJournal(t, fixture)
	assertConfigDriftReserializedAlias(t, fixture)
	durable := configDriftDurableFiles(t, fixture)
	before := snapshotConfigDriftFiles(t, durable)
	assertConfigDriftStatusObserved(t, fixture)
	assertConfigDriftDispatchRefused(t, fixture)
	assertConfigDriftAliasRetirementRefused(t, fixture)
	assertHistoricalTaskEvidence(t, fixture)
	if after := snapshotConfigDriftFiles(t, durable); !reflect.DeepEqual(after, before) {
		t.Fatal("public consumers changed task bytes, the journal, the alias, daemon starts or the PID file")
	}
}

// Status reports the exact receipted stashed row through the alias.
func assertConfigDriftStatusObserved(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	code, stdout, _, status := runHistoricalSupervisorCommand(t, fixture, []string{"status", fixture.req.TaskID, "--root", fixture.root, "--json"})
	t.Logf("status exit=%d json=%s", code, stdout)
	row := status.Supervisor
	if code != 0 || status.TaskID != fixture.req.TaskID || status.Admission != task.AdmissionAdmitted.String() || status.Liveness != task.LivenessRunning.String() || status.Error != "" {
		t.Fatalf("status of the saved task in the drifted state: exit=%d json=%s", code, stdout)
	}
	if !historicalSupervisorResponseMatches(row, 7) || row.State != "stashed" {
		t.Fatalf("status did not match the receipted stashed row: json=%s", stdout)
	}
}

func assertConfigDriftDispatchRefused(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	code, dispatched := runConfigDriftDispatch(t, fixture)
	t.Logf("dispatch exit=%d json=%s", code, dispatched.raw)
	failure := dispatched.response.Failure
	if code != 2 || dispatched.response.TaskRecord != "not_created" || dispatched.response.TaskID == "" || failure == nil {
		t.Fatalf("dispatch in the drifted state: exit=%d json=%s", code, dispatched.raw)
	}
	if failure.Code != "supervisor_upgrade_required" || failure.Stage != "start-supervisor" || failure.NextAction != "run_supervisor_migrate" {
		t.Fatalf("dispatch lost its migration guidance: %+v", failure)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "tasks", dispatched.response.TaskID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused dispatch created a task: %v", err)
	}
}

type configDriftDispatch struct {
	response Response
	raw      []byte
}

// runConfigDriftDispatch uses the canonical preflight provider fixture; its
// brief and working directory live outside the state root.
func runConfigDriftDispatch(t *testing.T, fixture *historicalSupervisorFixture) (int, configDriftDispatch) {
	t.Helper()
	deps := fixture.dependencies()
	provider := preflightFixture{root: fixture.root, cwd: testsupport.CanonicalTempDir(t)}
	deps.Catalog = preflightFixtureCatalog(t, provider.candidate)
	deps.RunnerExecutable = canonicalAppTestExecutable(t)
	deps.PublisherVersion = "config-drift-test"
	brief := filepath.Join(provider.cwd, "brief.md")
	if err := os.WriteFile(brief, []byte("must not create a task while the published config drifted"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--root", fixture.root, "dispatch", "--provider", "fixture:preflight",
		"--cwd", provider.cwd, "--brief", brief, "--runner", deps.RunnerExecutable,
		"--permission", "read-only", "--budget", "1m", "--json",
	}, &stdout, &stderr, deps)
	raw := append([]byte(nil), stdout.Bytes()...)
	return code, configDriftDispatch{response: decodeHistoricalResponse(t, raw, stderr.Bytes(), code), raw: raw}
}

func snapshotConfigDriftFiles(t *testing.T, paths []string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			snapshot[path] = "<absent>"
		case err != nil:
			t.Fatal(err)
		default:
			snapshot[path] = string(data)
		}
	}
	return snapshot
}
