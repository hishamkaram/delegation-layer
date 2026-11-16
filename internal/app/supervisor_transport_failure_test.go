package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app/infrastructurehost"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The resident birth, inherited lease, Unix authentication, canonical real-store
// maintenance authorizer, classifier, client reconstruction and public
// projection are exercised here. The EntryHost migration port supplies only
// the real authorizer's refusal, without attempting a private handoff.
// Pueue is an owned component script; this is not native Pueue or managed-image
// eligibility proof. Initial WarmReuse and test-image validation use the
// existing component host. No provider work is dispatched.
func TestSupervisorMigrationTransportKeepsClosureOnlyGuidance(t *testing.T) {
	result, fixture := runMaintenanceTransportMigration(t, pueue.StateEnded)
	if result.code != 1 || !errors.Is(result.err, inspection.ErrGroupMigrationBusy) {
		t.Fatalf("resident did not return the canonical maintenance refusal: code=%d error=%v", result.code, result.err)
	}
	assertSupervisorClosureDiagnostic(t, result, fixture.req.TaskID)
	if result.response.SupervisorControl.DiagnosticSnapshotAt == "" {
		t.Fatal("routed refusal did not retain its later canonical task diagnostic")
	}
	public := maintenanceTransportJSON(t, result)
	if public.SupervisorControl.ReasonCode == "active_work" || strings.Contains(public.Error, "Wait for queued or running work") {
		t.Errorf("resident transport invented queue activity for a closure-only blocker: control=%+v error=%q", public.SupervisorControl, public.Error)
	}
}

func TestSupervisorMigrationTransportPreservesPositiveQueueGuidance(t *testing.T) {
	result, fixture := runMaintenanceTransportMigration(t, pueue.StateRunning)
	if result.code != 1 || !errors.Is(result.err, pueue.ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("resident did not retain canonical positive queue activity: code=%d error=%v", result.code, result.err)
	}
	control := result.response.SupervisorControl
	if control.ActiveWork != "present" || control.MigrationReadiness != "blocked" || control.NextAction != "wait_for_legacy_work" || control.DiagnosticSnapshotAt == "" || control.FirstBlocker == nil {
		t.Fatalf("positive queue diagnostic was lost: %+v", control)
	}
	if blocker := control.FirstBlocker; blocker.Kind != "queue" || blocker.TaskID != fixture.req.TaskID || blocker.ReasonCode != "queue_work_present" {
		t.Fatalf("positive queue diagnostic lost its exact row: %+v", blocker)
	}
	public := maintenanceTransportJSON(t, result)
	if public.SupervisorControl.ReasonCode != "active_work" || !strings.Contains(public.Error, "Wait for queued or running work") {
		t.Errorf("transport lost positive queue guidance: control=%+v error=%q", public.SupervisorControl, public.Error)
	}
}

func runMaintenanceTransportMigration(t *testing.T, state pueue.State) (commandResult, *historicalSupervisorFixture) {
	t.Helper()
	fixture := newMaintenanceTransportFixture(t)
	if state == pueue.StateEnded {
		writeEndedHistoricalMigrationStatus(t, fixture)
	} else if state != pueue.StateRunning {
		t.Fatalf("unsupported transport fixture state %q", state)
	}
	before := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	deps := fixture.dependencies()
	deps.InitialSupervisorExecutable = fixture.clientA
	deps.RunnerExecutable = canonicalAppTestExecutable(t)
	ownMaintenanceTransportResident(t, fixture, deps)
	origin, err := infrastructure.NewOrigin(fixture.root, fixture.store.RootID)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := infrastructure.WithOrigin(nil, origin)
	if err != nil {
		t.Fatal(err)
	}
	_, value, found := strings.Cut(environment[0], "=")
	if !found {
		t.Fatal("canonical origin has no environment value")
	}
	t.Setenv(infrastructure.OriginEnvironment, value)
	deps.invocationSource = infrastructurehost.CaptureInvocation(t.Context())
	result := supervisorMigrate(fixture.root, deps, newSupervisorCommandResponse("migrate"))
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(before, after) {
		t.Error("refused resident migration changed the original task evidence")
	}
	return result, fixture
}

func maintenanceTransportJSON(t *testing.T, result commandResult) Response {
	t.Helper()
	var output bytes.Buffer
	if err := writeJSONResponse(&output, "supervisor", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var public Response
	if err := json.Unmarshal(output.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	return public
}

func newMaintenanceTransportFixture(t *testing.T) *historicalSupervisorFixture {
	t.Helper()
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	preparePrivateMaintenanceStatusObservation(t, fixture)
	fixture.options.PrivateMigrationAuthorizer = authorizePrivateMigrationQueue
	writeSupervisorGenerationTestGroup(t, fixture.store, fixture.predecessor)
	configPath := pueue.PrivateConfigPath(fixture.root)
	original := readSupervisorProbeTestFile(t, configPath)
	// Restoration precedes the observation helper's daemon cleanup.
	t.Cleanup(func() {
		if err := os.WriteFile(configPath, original, 0o600); err != nil {
			t.Error(err)
		}
	})
	if err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(context.Context) error {
		return os.WriteFile(configPath, retargetedPrivateSocketConfig(t, original, fixture.predecessor), 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

type maintenanceTransportHost struct{ resourceBoundaryComponentHost }

func (maintenanceTransportHost) MigrateOwnRoot(ctx context.Context, operation infrastructure.Operation, selected infrastructure.LaunchContext, _ infrastructure.ResidentGuards, _ infrastructure.DaemonWaitOwner) (disposition infrastructure.Disposition, report infrastructure.ManualMigrationReport, resultErr error) {
	store, err := openNativeSupervisorStore(ctx, selected.Root)
	if err != nil {
		return disposition, report, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	binding, found, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil || !found {
		return disposition, report, errors.Join(err, infrastructure.ErrIdentity)
	}
	options := pueue.Options{
		ControlOwnerExecutable: operation.Artifacts().Runner, Environment: selected.ControlEnvironment,
		ObservationTimeout: pueue.DefaultObservationTimeout, WaitForPendingClose: true,
	}
	client, err := supervisorMaintenanceClient(ctx, store, binding, selected.SupervisorExecutable, filepath.Join(filepath.Dir(selected.SupervisorExecutable), "pueued"), options)
	owner, err := supervisorcore.AcquireClient(client, err)
	defer func() { resultErr = errors.Join(resultErr, owner.CloseAfterPending(resultErr)) }()
	if err != nil {
		return disposition, report, err
	}
	snapshot, err := owner.Client().Snapshot(ctx)
	if err != nil {
		return disposition, report, err
	}
	cause := pueue.WithBootstrapLockContext(ctx, store.Root, func(locked context.Context) error {
		return authorizePrivateMigrationQueue(locked, store.Root, binding, snapshot.Jobs)
	})
	report, err = infrastructureManualMigrationReport(supervisorMigrationFailure(newSupervisorCommandResponse("migrate"), cause))
	return disposition, report, errors.Join(cause, err)
}

func (maintenanceTransportHost) ClassifyPreparation(cause error) infrastructure.PreparationFailure {
	return nativeInfrastructureHost().ClassifyPreparation(cause)
}

func TestSupervisorMaintenanceTransportResidentProcess(t *testing.T) {
	for index, arg := range os.Args {
		if arg != "--maintenance-transport-resident" {
			continue
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		handled, err := infrastructure.RunOwnerMode(ctx, os.Args[index+1:], maintenanceTransportHost{})
		if !handled || err != nil {
			t.Fatalf("maintenance resident: handled=%t error=%v", handled, err)
		}
		return
	}
}

func ownMaintenanceTransportResident(t *testing.T, fixture *historicalSupervisorFixture, deps Dependencies) {
	t.Helper()
	runner := canonicalAppTestExecutable(t)
	intent := resourceResidentIntent(t, fixture.store, runner)
	reference, err := infrastructure.ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	lease, acquired, err := infrastructure.TryAcquireLease(ctx, filepath.Join(fixture.root, ".infrastructure-owner.lock"))
	if err != nil || !acquired {
		t.Fatalf("resident lease: acquired=%t error=%v", acquired, err)
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, fixture.root, deps.storeDependencies().registry())
	if err != nil {
		t.Fatal(errors.Join(err, lease.Close()))
	}
	command := exec.Command(runner, "-test.run=^TestSupervisorMaintenanceTransportResidentProcess$", "-test.timeout=40s", "--", "--maintenance-transport-resident", "--infrastructure-owner", "--root", fixture.root, "--root-id", fixture.store.RootID, "--sequence", "1", "--epoch", reference.Epoch, "--intent-sha256", reference.IntentSHA256)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	done := make(chan error, 1)
	closed := make(chan struct{})
	// The component may dispose of this namespace only after the separate
	// resident's original Wait and positive native closure have both completed.
	fixture.component.mu.Lock()
	fixture.component.pending = append(fixture.component.pending, closed)
	fixture.component.mu.Unlock()
	carrier := &remoteResourceBoundaryFixture{remoteServerFixture: &remoteServerFixture{store: fixture.store, deps: deps}, retain: true}
	started := false
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if !started {
			close(closed)
		} else {
			cleanup, finish := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
			closeRemoteResourceResident(t, cleanup, carrier, reader, reference, done, &output)
			finish()
			if !carrier.retain {
				close(closed)
			}
		}
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = publishResourceResidentIntent(ctx, fixture.store, intent); err != nil {
		t.Fatal(err)
	}
	if err = lifetime.ConfigureIndependentSession(command); err != nil {
		t.Fatal(err)
	}
	err = lease.WithHeldFile(ctx, func(file *os.File) error {
		command.ExtraFiles = []*os.File{file}
		return command.Start()
	})
	if err != nil {
		t.Fatal(err)
	}
	started = true
	go func() { done <- command.Wait() }()
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	awaitMaintenanceTransportResident(t, ctx, reader, reference)
}

func awaitMaintenanceTransportResident(t *testing.T, ctx context.Context, reader *taskdir.ReadOnlyStore, reference infrastructure.StartupReference) {
	t.Helper()
	for {
		observed, err := infrastructure.ProbeOwner(ctx, reader)
		if err == nil && observed == reference {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("resident did not authenticate: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
