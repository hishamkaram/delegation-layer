package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/gatewaycommand"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Both installations are built from source in their owned kegs. The Native
// store, resident, daemon, inspection group and handoff are real. Only the SDK
// profile and OS connector/GUI edges use the existing composition doubles.
func TestGatewayUpgradeCompletesExistingInspectionGroup(t *testing.T) {
	client, daemon := requireRuntimeRestartCLINativeContext(t)
	f := newRuntimeRestartCLINative(t, client, daemon)
	oldBin, newBin := buildGatewayInspectionUpgradeKegs(t, f)
	selectGatewayInspectionUpgradeKeg(f, oldBin)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	f.ensureOwner(t, ctx)
	t.Cleanup(func() { closeQuietColdUpgradeFixture(t, f) })
	prior := f.binding(t)
	establishGatewayInspectionGroup(t, ctx, f, prior)

	selectGatewayInspectionUpgradeKeg(f, newBin)
	profile := newGatewayUpgradeNativeProfile(t, ctx, f)
	profile.source.DelegateExecutable = filepath.Join(oldBin, "delegate")
	services := &gatewayUpgradeNativeServices{profile: profile.directory}
	prepared := coldUpgradeCLIStep(t, ctx, f, "", infrastructure.UpgradeAwaitingInstallation, "ensure-owner", "--prepare-upgrade")
	t.Cleanup(func() { closeGatewayUpgradeNativeOperation(ctx, t, f, prepared.OperationID) })
	response := runGatewayUpgradeNativeComposition(t, ctx, f, profile, services, prepared.OperationID, true)
	assertGatewayUpgradeNativeCompletion(t, ctx, f, profile, services, response, prepared.OperationID)
	f.preflight(t, ctx) // Retain the current resident for canonical fixture cleanup.
	current := f.binding(t)
	if prior.ConfigDigest == current.ConfigDigest {
		t.Fatal("two-keg handoff did not change the installed callback configuration")
	}
	active := f.executionTarget(t, ctx)
	assertGatewayInspectionConsumersReady(t, ctx, f, prior, current)
	if same, err := lifetime.SameProcess(active, f.executionTarget(t, ctx)); err != nil || !same {
		t.Errorf("post-upgrade inspection validation started another daemon: same=%t err=%v", same, err)
	}
	requireGatewayUpgradeNativeJournal(t, ctx, f, prepared.OperationID)
}

// The private handoff and daemon lifecycle are actual Native work. Only the
// stale prior inspection request is historical DATA, representing the old
// private-completed/derived-edge-not-yet-published boundary without erasing any
// inspection history or inventing process closure.
func TestGatewayUpgradeReadiesCompletedCurrentGenerationAfterDaemonStop(t *testing.T) {
	client, daemon := requireRuntimeRestartCLINativeContext(t)
	f := newRuntimeRestartCLINative(t, client, daemon)
	oldBin, newBin := buildGatewayInspectionUpgradeKegs(t, f)
	selectGatewayInspectionUpgradeKeg(f, oldBin)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	f.ensureOwner(t, ctx)
	t.Cleanup(func() { closeQuietColdUpgradeFixture(t, f) })
	prior := f.binding(t)
	selectGatewayInspectionUpgradeKeg(f, newBin)
	prepared := coldUpgradeCLIStep(t, ctx, f, "", infrastructure.UpgradeAwaitingInstallation, "ensure-owner", "--prepare-upgrade")
	t.Cleanup(func() { closeGatewayUpgradeNativeOperation(ctx, t, f, prepared.OperationID) })
	profile := newGatewayUpgradeNativeProfile(t, ctx, f)
	profile.source.DelegateExecutable = filepath.Join(oldBin, "delegate")
	completeGatewayInspectionPrivateOperation(t, ctx, f, profile, prepared.OperationID)
	f.preflight(t, ctx)
	seedGatewayHistoricalInspectionRequest(t, ctx, f, prior)
	history := gatewayInspectionPrivateHistory(t, f)
	stopped := f.executionTarget(t, ctx)
	if err := f.closeObservedDaemon(ctx); err != nil {
		t.Fatal(err)
	}
	services := &gatewayUpgradeNativeServices{profile: profile.directory}
	response := runGatewayRetainedInspectionUpgrade(t, ctx, f, profile, services, prepared.OperationID)
	assertGatewayUpgradeNativeCompletion(t, ctx, f, profile, services, response, prepared.OperationID)
	active := f.executionTarget(t, ctx)
	if same, err := lifetime.SameProcess(stopped, active); err != nil || same {
		t.Fatalf("ReadyCurrent did not recover stopped target through ordinary owner: same=%t err=%v", same, err)
	}
	assertGatewayInspectionConsumersReady(t, ctx, f, prior, f.binding(t))
	assertGatewayReadyCurrentReplay(t, ctx, f, profile, prepared.OperationID, active)
	if !bytes.Equal(history, gatewayInspectionPrivateHistory(t, f)) {
		t.Fatal("current-admission readiness rewrote A/history")
	}
}

// Only retained invocation intent is historical DATA. The real Manager
// publishes/claims/finishes its request; the child constructs the actual App
// Native maintenance owner. This does not prove how an earlier receipt arose.
type gatewayRetainedInspectionControl struct {
	gatewayservice.GUIUpgradeControl
	operation string
}

func (c gatewayRetainedInspectionControl) RunGUIUpgrade(ctx context.Context, request gatewayservice.GUIUpgradeRequest) (gatewayservice.GUIUpgradeResult, error) {
	if request.ExpectedOperation != "" && request.ExpectedOperation != c.operation {
		return gatewayservice.GUIUpgradeResult{}, task.ErrRequestConflict
	}
	request.ExpectedOperation = c.operation
	return c.GUIUpgradeControl.RunGUIUpgrade(ctx, request)
}

func runGatewayRetainedInspectionUpgrade(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, profile *gatewayUpgradeNativeProfile, services *gatewayUpgradeNativeServices, operation string) gatewaycommand.Response {
	t.Helper()
	deps := Dependencies{Gateway: &gatewaycommand.Dependencies{Client: profile, Executable: profile.selected, Services: services, Environment: f.environment}, Environment: f.environment, RunnerExecutable: filepath.Join(f.bin, "delegate-run"), InitialSupervisorExecutable: f.originalClient}
	gui := newGatewayGUIFixture(t, ctx, f.base, deps, profile)
	deps.Gateway.GUIUpgrade = gatewayRetainedInspectionControl{GUIUpgradeControl: gui.manager, operation: operation}
	gui.dependencies = deps
	args := []string{"gateway", "upgrade", "--root", f.root, "--profile", profile.directory, "--accept-execution-policy-removal", "--json"}
	ticks := time.NewTicker(coldUpgradePollCadence)
	defer ticks.Stop()
	for {
		var output, diagnostic bytes.Buffer
		code := RunContext(ctx, args, &output, &diagnostic, deps)
		gui.observeFinishedInvocation()
		var reply gatewaycommand.Response
		if err := json.Unmarshal(output.Bytes(), &reply); err != nil || reply.Upgrade == nil {
			t.Fatalf("retained-A public result unavailable: exit=%d err=%v", code, err)
		}
		if code == 0 {
			gui.assertCompleted(t, ctx, args, deps, reply, operation)
			return reply
		}
		if code != 3 || reply.Failure != "gateway_upgrade_pending" || reply.NextAction != "retry_gateway_upgrade_same_profile" || reply.Upgrade.Supervisor.OperationID != operation {
			t.Fatalf("retained-A public upgrade failed: exit=%d reply=%+v", code, reply)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticks.C:
		}
	}
}

func TestGatewayUpgradeFreshMaintenancePreservesHealthyNativeGeneration(t *testing.T) {
	client, daemon := requireRuntimeRestartCLINativeContext(t)
	f := newRuntimeRestartCLINative(t, client, daemon)
	f.build(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	f.ensureOwner(t, ctx)
	t.Cleanup(func() {
		cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cleanupCancel()
		if err := rememberGatewayInspectionServingOwner(cleanup, f); err != nil {
			t.Error(err)
		}
		closeQuietColdUpgradeFixture(t, f)
	})
	active, binding := f.executionTarget(t, ctx), f.binding(t)
	before := gatewayInspectionPrivateRecordSnapshot(t, f)
	profile := newGatewayUpgradeNativeProfile(t, ctx, f)
	services := &gatewayUpgradeNativeServices{profile: profile.directory}
	response := runGatewayUpgradeNativeComposition(t, ctx, f, profile, services, "", true)
	if response.Upgrade.Phase != "complete" || response.Upgrade.Supervisor != (gatewaycommand.NativeTransition{}) || !profile.updated || !profile.migrated {
		t.Fatalf("fresh ordinary maintenance invented a private upgrade: %+v", response.Upgrade)
	}
	if err := rememberGatewayInspectionServingOwner(ctx, f); err != nil {
		t.Fatal(err)
	}
	if same, err := lifetime.SameProcess(active, f.executionTarget(t, ctx)); err != nil || !same {
		t.Fatalf("healthy ordinary maintenance started another daemon: same=%t err=%v", same, err)
	}
	if f.binding(t) != binding || !bytes.Equal(before, gatewayInspectionPrivateRecordSnapshot(t, f)) {
		t.Fatal("fresh maintenance rewrote private target/history or accepted a new A")
	}
	services.assertHandoff(t, profile.originalExecutable, profile.selected)
}

func rememberGatewayInspectionServingOwner(ctx context.Context, f *runtimeRestartCLINative) (resultErr error) {
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, f.root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	if reader.RootID() != f.rootID {
		return task.ErrIdentityMismatch
	}
	startup, _, err := observeRuntimeRestartCLICleanupOwner(ctx, reader)
	if err != nil {
		return err
	}
	token, err := infrastructure.FormatRuntimeRestartReference(infrastructure.RuntimeRestartReference{RootID: f.rootID, Startup: startup})
	if err != nil {
		return err
	}
	return f.rememberStartup(token)
}

func gatewayInspectionPrivateRecordSnapshot(t *testing.T, f *runtimeRestartCLINative) []byte {
	t.Helper()
	var snapshot bytes.Buffer
	for _, name := range []string{"bootstrap.pending", "migration.json", "migration-history.json", "supervisor.identity.json", "daemon.identity.json"} {
		data, err := os.ReadFile(filepath.Join(f.root, ".supervisor", name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		snapshot.WriteString(name)
		snapshot.WriteByte(0)
		snapshot.Write(data)
		snapshot.WriteByte(0)
	}
	return snapshot.Bytes()
}

func completeGatewayInspectionPrivateOperation(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, profile *gatewayUpgradeNativeProfile, operation string) {
	t.Helper()
	maintenance := gatewayNativeMaintenance{dependencies: Dependencies{Environment: f.environment, RunnerExecutable: filepath.Join(f.bin, "delegate-run"), InitialSupervisorExecutable: f.originalClient}.withCapturedEnvironmentContext(ctx).normalized()}
	ticks := time.NewTicker(coldUpgradePollCadence)
	defer ticks.Stop()
	for {
		observed, err := maintenance.Ensure(ctx, gatewaycommand.NativeMaintenanceRequest{Root: f.root, ExpectedRoot: profile.source.DelegateRootContext, OperationID: operation})
		if observed.OperationID != operation {
			t.Fatalf("setup replaced exact A: %+v %v", observed, err)
		}
		if err == nil && !observed.Pending {
			requireGatewayUpgradeNativeJournal(t, ctx, f, operation)
			return
		}
		if !errors.Is(err, gatewaycommand.ErrNativeUpgradePending) || !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, gatewaycommand.ErrNativeUpgradePending) }) {
			t.Fatalf("exact-A setup returned a hard fault: %+v %v", observed, err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticks.C:
		}
	}
}

func seedGatewayHistoricalInspectionRequest(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, prior task.SupervisorRef) {
	t.Helper()
	store, err := taskdir.OpenExistingStoreMetadataContext(ctx, f.root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if present, _, headErr := inspection.GroupHeadMatchesBinding(ctx, store, prior); headErr != nil || present {
		t.Fatalf("historical fixture would overwrite an inspection head: present=%t err=%v", present, headErr)
	}
	control, err := store.OpenInspectionGroupContext(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	_, putErr := control.PutContext(ctx, "request.json", supervisorcore.InspectionGroup(store.RootID, prior))
	if err = errors.Join(putErr, control.Close()); err != nil {
		t.Fatal(err)
	}
}

func assertGatewayReadyCurrentReplay(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, profile *gatewayUpgradeNativeProfile, operation string, active lifetime.ProcessRef) {
	t.Helper()
	// Same-target publication DATA represents an interrupted publication after
	// the actual ordinary restart. It invents neither birth nor process closure.
	publication := filepath.Join(f.root, ".supervisor", "identity.pending.json")
	data, err := task.MarshalCanonical(struct {
		SchemaVersion int                `json:"schema_version"`
		Binding       task.SupervisorRef `json:"binding"`
	}{task.SchemaVersion, f.binding(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err = pueue.WithExistingBootstrapLockContext(ctx, f.root, func(context.Context) error {
		file, createErr := os.OpenFile(publication, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return createErr
		}
		_, writeErr := file.Write(data)
		return errors.Join(writeErr, file.Close())
	}); err != nil {
		t.Fatal(err)
	}
	maintenance := gatewayNativeMaintenance{dependencies: Dependencies{Environment: f.environment, RunnerExecutable: filepath.Join(f.bin, "delegate-run"), InitialSupervisorExecutable: f.originalClient}.withCapturedEnvironmentContext(ctx).normalized()}
	observed, err := maintenance.Ensure(ctx, gatewaycommand.NativeMaintenanceRequest{Intent: gatewaycommand.NativeMaintenanceReadyCurrent, Root: f.root, ExpectedRoot: profile.source.DelegateRootContext, OperationID: operation})
	if err != nil || observed.OperationID != operation || observed.Pending {
		t.Fatalf("explicit ReadyCurrent replay failed: observed=%+v err=%v", observed, err)
	}
	if same, err := lifetime.SameProcess(active, f.executionTarget(t, ctx)); err != nil || !same {
		t.Fatalf("healthy ReadyCurrent created another birth: same=%t err=%v", same, err)
	}
	if _, err := os.Lstat(publication); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadyCurrent did not settle exact current publication: %v", err)
	}
}

func buildGatewayInspectionUpgradeKegs(t *testing.T, f *runtimeRestartCLINative) (string, string) {
	t.Helper()
	oldBin := filepath.Join(f.base, "predecessor-keg", "bin")
	if err := os.MkdirAll(oldBin, 0o700); err != nil {
		t.Fatal(err)
	}
	selectGatewayInspectionUpgradeKeg(f, oldBin)
	f.build(t)
	prepareGatewayUpgradeNativeKeg(t, f)
	f.build(t)
	return oldBin, f.bin
}

func selectGatewayInspectionUpgradeKeg(f *runtimeRestartCLINative, bin string) {
	f.bin = bin
	f.environment = runtimeRestartCLIEnvironment(f.environment, map[string]string{
		"PATH": bin + string(os.PathListSeparator) + filepath.Dir(f.originalClient) + string(os.PathListSeparator) + "/usr/bin:/bin",
	})
}

func establishGatewayInspectionGroup(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, binding task.SupervisorRef) {
	t.Helper()
	store, err := taskdir.OpenExistingStoreMetadataContext(ctx, f.root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	client := gatewayInspectionClient(t, f, binding)
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = supervisorcore.EnsureInspectionGroupWithMigrationContext(ctx, store, client, pueue.DefaultObservationTimeout, false); err != nil {
		t.Fatalf("initial inspection group was not established through its owner: %v", err)
	}
	if present, matches, headErr := inspection.GroupHeadMatchesBinding(ctx, store, binding); headErr != nil || !present || !matches {
		t.Fatalf("initial inspection group is not valid for the predecessor: present=%t matches=%t err=%v", present, matches, headErr)
	}
	if pending, pendingErr := inspection.GroupMigrationPending(ctx, store); pendingErr != nil || pending {
		t.Fatalf("initial inspection group already requires migration: pending=%t err=%v", pending, pendingErr)
	}
}

func gatewayInspectionClient(t *testing.T, f *runtimeRestartCLINative, binding task.SupervisorRef) *pueue.Client {
	t.Helper()
	client, err := pueue.NewClient(binding, pueue.Options{
		Environment: f.environment, ObservationTimeout: pueue.DefaultObservationTimeout,
		ControlOwnerExecutable: filepath.Join(f.bin, "delegate-run"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func assertGatewayInspectionConsumersReady(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, prior, current task.SupervisorRef) {
	t.Helper()
	assertGatewayInspectionStatusReady(t, ctx, f)
	store, err := taskdir.OpenExistingStoreMetadataContext(ctx, f.root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if present, matches, headErr := inspection.GroupHeadMatchesBinding(ctx, store, current); headErr != nil || !present || !matches {
		t.Errorf("completed Gateway upgrade left a stale inspection head: present=%t matches=%t err=%v", present, matches, headErr)
	}
	client := gatewayInspectionClient(t, f, current)
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	before := gatewayInspectionPrivateHistory(t, f)
	continuationErr := pueue.WithExistingBootstrapLockContext(ctx, f.root, func(locked context.Context) error {
		ready, validateErr := supervisorcore.RevalidateContinuationSuccessor(locked, f.root, prior, func(openContext context.Context, root string) (*taskdir.Store, error) {
			return taskdir.OpenExistingStoreMetadataContext(openContext, root, predicate.Default())
		})
		if validateErr == nil && !task.SameSupervisorIdentity(ready, current) {
			return task.ErrIdentityMismatch
		}
		return validateErr
	})
	if continuationErr != nil {
		t.Errorf("completed Gateway upgrade still refuses continuation validation: migration_required=%t err=%v", errors.Is(continuationErr, inspection.ErrGroupMigrationRequired), continuationErr)
	}
	if serialErr := supervisorcore.EnsureInspectionGroupWithMigrationContext(ctx, store, client, pueue.DefaultObservationTimeout, false); serialErr != nil {
		t.Errorf("completed Gateway upgrade still refuses serial inspection: migration_required=%t err=%v", errors.Is(serialErr, inspection.ErrGroupMigrationRequired), serialErr)
	}
	if !bytes.Equal(before, gatewayInspectionPrivateHistory(t, f)) {
		t.Error("inspection validation rewrote private upgrade history")
	}
	if ids, idsErr := store.TaskIDs(ctx); idsErr != nil || len(ids) != 0 {
		t.Errorf("inspection maintenance fabricated task/outcome records: count=%d err=%v", len(ids), idsErr)
	}
}

func assertGatewayInspectionStatusReady(t *testing.T, ctx context.Context, f *runtimeRestartCLINative) {
	t.Helper()
	status, code, statusErr := f.command(ctx, "status")
	if statusErr != nil || code != 0 || status.SupervisorControl == nil {
		t.Fatalf("post-upgrade public status unavailable: exit=%d err=%v", code, statusErr)
	}
	if status.SupervisorControl.NextAction == "run_supervisor_migrate" {
		t.Errorf("completed Gateway upgrade left public inspection migration advice: state=%s next=%s", status.SupervisorControl.State, status.SupervisorControl.NextAction)
	}
}

func gatewayInspectionPrivateHistory(t *testing.T, f *runtimeRestartCLINative) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, ".supervisor", "migration-history.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
