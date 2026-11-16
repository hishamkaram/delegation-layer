package app

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestPublicSupervisorStatusQualifiesSettledMaintenanceBinding(t *testing.T) {
	for _, scenario := range []string{"valid settlement", "corrupt settlement", "different ended row", "corrupt canonical", "missing canonical", "settled logical drift"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
			preparePrivateMaintenanceStatusObservation(t, fixture)
			writeEndedHistoricalMigrationStatus(t, fixture)
			recordSettlementLifetime(t, fixture.td, nil)
			confirmSettlementForTest(t, fixture.td)
			if _, err := os.Lstat(filepath.Join(fixture.root, "inspection-group")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fixture did not exercise the no-group status path: %v", err)
			}
			binding, found, err := pueue.ReadPrivateSupervisorBinding(fixture.root)
			if err != nil || !found || binding.DaemonExecutable == "" {
				t.Fatalf("fixture lacks a full canonical supervisor binding: %+v %v", binding, err)
			}
			prepareMaintenanceBindingDamage(t, fixture, scenario)
			before := snapshotMaintenanceStatusRoot(t, fixture.root)
			deps := fixture.dependencies()
			deps.InitialSupervisorExecutable = fixture.clientA
			result := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
			assertSettledMaintenanceBindingStatus(t, result, scenario == "valid settlement")
			if scenario == "corrupt canonical" && !errors.Is(result.err, pueue.ErrBinding) {
				t.Fatalf("canonical read fault was dropped: %v", result.err)
			}
			assertMaintenanceStatusRootUnchanged(t, fixture.root, before)
			evidence, err := taskdir.LoadLifetimeEvidenceContext(t.Context(), fixture.td)
			requireSettlementTest(t, err)
			closure, err := task.ResolveLifetimeClosure(evidence)
			if err != nil || closure.Closed() {
				t.Fatalf("status fabricated native task closure: %+v %v", closure, err)
			}
		})
	}
}

func preparePrivateMaintenanceStatusObservation(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	var witnesses []maintenanceDaemonWitness
	for _, wait := range fixture.component.daemonWaits() {
		process, err := lifetime.ObserveProcess(wait.pid)
		if err != nil {
			t.Fatal(err)
		}
		witnesses = append(witnesses, maintenanceDaemonWitness{wait: wait, process: process})
		t.Logf("owned mock daemon root=%s pid=%d start=%s", fixture.root, wait.pid, process.StartToken())
	}
	fixture.component.listen(t, fixture.predecessor.Endpoint)
	endpoint := fixture.component.endpoints[len(fixture.component.endpoints)-1]
	// Close this observation-only listener before the component verifies
	// daemon shutdown. The component's later close is explicitly idempotent.
	t.Cleanup(func() {
		if err := endpoint.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
		<-endpoint.done
		fixture.component.cleanup(t)
		assertMaintenanceDaemonClosure(t, witnesses)
	})
	if err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(context.Context) error {
		return pueue.EnsurePrivateTaskObservationWithBootstrapLock(fixture.root)
	}); err != nil {
		t.Fatal(err)
	}
}

type maintenanceDaemonWitness struct {
	wait    componentDaemonCompletion
	process lifetime.ProcessRef
}

func assertMaintenanceDaemonClosure(t *testing.T, witnesses []maintenanceDaemonWitness) {
	t.Helper()
	for _, witness := range witnesses {
		joined := false
		select {
		case <-witness.wait.done:
			joined = true
		default:
		}
		present, err := lifetime.ProcessPresent(witness.process)
		t.Logf("owned mock daemon pid=%d start=%s native_wait_joined=%t present=%t err=%v", witness.wait.pid, witness.process.StartToken(), joined, present, err)
		if !joined || present || err != nil {
			t.Error("owned mock daemon closure remains unverified")
		}
	}
}

type maintenanceStatusFile struct {
	mode     fs.FileMode
	modified time.Time
	contents string
}

type maintenanceStatusSnapshot struct {
	files   map[string]maintenanceStatusFile
	sockets map[string]fs.FileInfo
}

func snapshotMaintenanceStatusRoot(t *testing.T, root string) maintenanceStatusSnapshot {
	t.Helper()
	snapshot := maintenanceStatusSnapshot{files: make(map[string]maintenanceStatusFile), sockets: make(map[string]fs.FileInfo)}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		file := maintenanceStatusFile{mode: info.Mode(), modified: info.ModTime()}
		if path == filepath.Join(root, ".probe") {
			file.modified = time.Time{}
		}
		switch {
		case info.Mode()&os.ModeSocket != 0:
			snapshot.sockets[path] = info
		case info.Mode().IsRegular():
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			file.contents = string(data)
		case info.IsDir():
		default:
			return os.ErrInvalid
		}
		snapshot.files[path] = file
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertMaintenanceStatusRootUnchanged(t *testing.T, root string, before maintenanceStatusSnapshot) {
	t.Helper()
	after := snapshotMaintenanceStatusRoot(t, root)
	if !maps.Equal(before.files, after.files) || len(before.sockets) != len(after.sockets) {
		t.Fatal("status changed durable data or runtime endpoint metadata")
	}
	for path, prior := range before.sockets {
		current, found := after.sockets[path]
		if !found || !os.SameFile(prior, current) {
			t.Fatalf("status replaced runtime socket %s", path)
		}
	}
}

func assertSettledMaintenanceBindingStatus(t *testing.T, result commandResult, valid bool) {
	t.Helper()
	control := result.response.SupervisorControl
	if !valid {
		if result.code == 0 && control.MigrationReadiness == "clear" {
			t.Fatalf("invalid settlement evidence authorized maintenance: control=%+v", control)
		}
		return
	}
	if result.code != 0 || result.err != nil || control.MigrationReadiness != "clear" || control.FirstBlocker != nil || control.ActiveWork != "none" || control.NextAction != "none" {
		t.Fatalf("qualified settled row became a false maintenance blocker: code=%d control=%+v err=%v", result.code, control, result.err)
	}
}

func prepareMaintenanceBindingDamage(t *testing.T, fixture *historicalSupervisorFixture, scenario string) {
	t.Helper()
	switch scenario {
	case "valid settlement":
	case "corrupt settlement":
		if err := os.WriteFile(filepath.Join(fixture.td.Dir, "operator-disposition.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "different ended row":
		writeSavedPrivateCancelStatus(t, fixture.statusPath, "delegate:"+fixture.store.RootID+":"+strings.Repeat("9", 32), pueue.StateEnded)
	case "corrupt canonical", "missing canonical":
		path := filepath.Join(fixture.root, ".supervisor", "supervisor.identity.json")
		original := readSupervisorProbeTestFile(t, path)
		t.Cleanup(func() {
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Error(err)
			}
		})
		if scenario == "missing canonical" {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "settled logical drift":
		config := pueue.PrivateConfigPath(fixture.root)
		original := readSupervisorProbeTestFile(t, config)
		t.Cleanup(func() {
			if err := os.WriteFile(config, original, 0o600); err != nil {
				t.Error(err)
			}
		})
		if err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(context.Context) error {
			return os.WriteFile(config, retargetedPrivateSocketConfig(t, original, fixture.predecessor), 0o600)
		}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported maintenance binding fixture %q", scenario)
	}
}

func TestSupervisorMaintenanceBindingRetargetBeforeAssessmentFailsClosed(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	preparePrivateMaintenanceStatusObservation(t, fixture)
	writeEndedHistoricalMigrationStatus(t, fixture)
	recordSettlementLifetime(t, fixture.td, nil)
	confirmSettlementForTest(t, fixture.td)
	deps := fixture.dependencies()
	deps.InitialSupervisorExecutable = fixture.clientA
	current, err := currentPrivateSupervisor(t.Context(), fixture.root, deps, supervisorControlOptions(deps))
	fixture.component.ownClient(current, err)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := current.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	attributed, err := supervisorMaintenanceBinding(fixture.root, current.Binding())
	if err != nil || attributed != fixture.predecessor {
		t.Fatalf("canonical attribution did not retain the full published reference: %+v %v", attributed, err)
	}
	// A deterministic canonical DATA interleaving, not a native daemon handoff:
	// retarget after the public reader and before the actual leased assessor.
	retarget := fixture.predecessor
	retarget.ClientExecutable, retarget.DaemonExecutable = fixture.clientB, fixture.daemonB
	retargetMaintenanceGeneration(t, fixture, retarget)
	complete, err := pueue.PrivateSupervisorMigrationComplete(fixture.root, retarget)
	if err != nil || !complete {
		t.Fatalf("retarget fixture is not a coherent canonical generation: %t %v", complete, err)
	}
	before := snapshotMaintenanceStatusRoot(t, fixture.root)
	assessment, err := privateCleanupOwner().AssessMaintenance(t.Context(), fixture.store, attributed, snapshot.Jobs)
	if !errors.Is(err, task.ErrIdentityMismatch) || assessment.Readiness == "clear" {
		t.Fatalf("stale attributed generation cleared maintenance: %+v %v", assessment, err)
	}
	assertMaintenanceStatusRootUnchanged(t, fixture.root, before)
}

func retargetMaintenanceGeneration(t *testing.T, fixture *historicalSupervisorFixture, binding task.SupervisorRef) {
	t.Helper()
	base := filepath.Join(fixture.root, ".supervisor")
	publicationPath, daemonPath := filepath.Join(base, "supervisor.identity.json"), filepath.Join(base, "daemon.identity.json")
	publicationBefore, daemonBefore := readSupervisorProbeTestFile(t, publicationPath), readSupervisorProbeTestFile(t, daemonPath)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), componentFixtureCleanupTimeout)
		defer cancel()
		if err := pueue.WithBootstrapLockContext(ctx, fixture.root, func(context.Context) error {
			return errors.Join(os.WriteFile(publicationPath, publicationBefore, 0o600), os.WriteFile(daemonPath, daemonBefore, 0o600))
		}); err != nil {
			t.Error(err)
		}
	})
	publication, err := task.MarshalCanonical(struct {
		SchemaVersion int                `json:"schema_version"`
		Binding       task.SupervisorRef `json:"binding"`
	}{task.SchemaVersion, binding})
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := task.MarshalCanonical(historicalPrivateDaemonIdentity{
		Executable: binding.DaemonExecutable, ExecutableIdentity: binding.ExecutableIdentity,
		ConfigPath: binding.ConfigPath, ConfigDigest: binding.ConfigDigest,
		ResolvedConfigSHA256: binding.ResolvedConfigSHA256, Endpoint: binding.Endpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(context.Context) error {
		return errors.Join(os.WriteFile(publicationPath, publication, 0o600), os.WriteFile(daemonPath, daemon, 0o600))
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPublicSupervisorStatusMaintenanceBindingPreservesLogicalDrift(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	config := pueue.PrivateConfigPath(fixture.root)
	original := readSupervisorProbeTestFile(t, config)
	t.Cleanup(func() {
		if err := os.WriteFile(config, original, 0o600); err != nil {
			t.Error(err)
		}
	})
	if err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(context.Context) error {
		return os.WriteFile(config, retargetedPrivateSocketConfig(t, original, fixture.predecessor), 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotMaintenanceStatusRoot(t, fixture.root)
	deps := fixture.dependencies()
	deps.InitialSupervisorExecutable = fixture.clientA
	result := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
	control := result.response.SupervisorControl
	if result.code != 0 || result.err != nil || control.State != "legacy_present" || control.ActiveWork != "present" || control.NextAction != "wait_for_legacy_work" {
		t.Fatalf("binding qualification hid logical configuration drift: code=%d control=%+v err=%v", result.code, control, result.err)
	}
	assertMaintenanceStatusRootUnchanged(t, fixture.root, before)
}

func TestPublicSupervisorStatusSettledMaintenanceSurvivesDeletedRecordedPair(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	preparePrivateMaintenanceStatusObservation(t, fixture)
	writeEndedHistoricalMigrationStatus(t, fixture)
	recordSettlementLifetime(t, fixture.td, nil)
	confirmSettlementForTest(t, fixture.td)
	options := componentPublicationOptions(t, fixture.root, fixture.component, fixture.options)
	options.RuntimeClientExecutable = fixture.clientB
	cleanup, err := pueue.NewClient(fixture.predecessor, options)
	fixture.component.ownClient(cleanup, err)
	if err != nil {
		t.Fatal(err)
	}
	// Give the existing actual Wait a surviving shutdown transport before
	// removing either selected fixture executable.
	fixture.component.ownDaemonClients(cleanup, 0)
	if err = errors.Join(os.Remove(fixture.clientA), os.Remove(fixture.daemonA)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := supervisorcore.ControlContextWithParent(t.Context(), options)
	defer cancel()
	runtime, err := pueue.ProbeSavedPrivateCompatible(ctx, fixture.root, fixture.predecessor, options, func(context.Context) (string, string, error) {
		return fixture.clientB, fixture.daemonB, nil
	})
	fixture.component.ownClient(runtime, err)
	if err != nil || runtime == nil {
		t.Fatalf("fixture has no compatible live transport after package removal: %v", err)
	}
	if err = supervisorcore.EnsureAdmissionSchedulingContext(ctx, fixture.root, runtime); err != nil {
		t.Fatal(err)
	}
	before := snapshotMaintenanceStatusRoot(t, fixture.root)
	deps := fixture.dependencies()
	result := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
	control := result.response.SupervisorControl
	if result.code != 0 || result.err != nil || control.MigrationReadiness != "clear" || control.FirstBlocker != nil || control.ActiveWork != "none" {
		t.Fatalf("deleted recorded pair hid valid live settled maintenance: code=%d control=%+v err=%v", result.code, control, result.err)
	}
	assertMaintenanceStatusRootUnchanged(t, fixture.root, before)
}
