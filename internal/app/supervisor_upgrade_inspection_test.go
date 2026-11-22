package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSupervisorCompletedUpgradeReplayPreservesStaleInspection(t *testing.T) {
	store := newBareInspectionStore(t)
	target := supervisorGenerationTestBinding(store.Root, "completed", true)
	target.ExecutableIdentity, target.ClientSHA256, target.DaemonSHA256 = task.ExecutableIdentityInstalledPathV1, "", ""
	legacy := target
	legacy.ConfigDigest = task.ComputeSHA256([]byte("prior inspection configuration"))
	writeSupervisorGenerationTestPrivateGeneration(t, store, target)
	writeSupervisorGenerationTestGroup(t, store, legacy)
	operation := writeCompletedInspectionUpgradeData(t, store, legacy, target)
	before := snapshotMaintenanceStatusRoot(t, store.Root)
	outcome, done, err := observeSupervisorUpgradeCompletion(t.Context(), store, operation)
	if err != nil || !done || !outcome.Report.Completed || outcome.Report.OperationID != operation {
		t.Fatalf("historical Complete ceased to be read-only: done=%t outcome=%+v err=%v", done, outcome, err)
	}
	assertMaintenanceStatusRootUnchanged(t, store.Root, before)
	publishAppTestGroupMigration(t, store, supervisorcore.InspectionGroup(store.RootID, legacy), supervisorcore.InspectionGroup(store.RootID, target))
	before = snapshotMaintenanceStatusRoot(t, store.Root)
	outcome, done, err = observeSupervisorUpgradeCompletion(t.Context(), store, operation)
	if err != nil || !done || !outcome.Report.Completed {
		t.Fatalf("current edge lost read-only historical replay: done=%t outcome=%+v err=%v", done, outcome, err)
	}
	assertMaintenanceStatusRootUnchanged(t, store.Root, before)
}

// Strictly validated historical DATA. No process is created or closed by this
// helper; actual bound daemon custody is exercised by the Native CLI fixture.
func writeCompletedInspectionUpgradeData(t *testing.T, store *taskdir.Store, legacy, target task.SupervisorRef) infrastructure.UpgradeOperationID {
	t.Helper()
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	operation := infrastructure.UpgradeOperationID("upgrade-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	record := map[string]any{
		"schema_version": task.SchemaVersion, "legacy": historicalPrivateDaemonIdentityFor(legacy), "successor": historicalPrivateDaemonIdentityFor(target),
		"legacy_pid": 101, "legacy_process_start": "historical-predecessor", "legacy_shutdown_requested": true, "legacy_shutdown_dispatched": true, "legacy_stopped": true,
		"upgrade_generation": map[string]any{"operation_id": operation, "root": map[string]any{"path": pin.Root(), "id": pin.RootID(), "device": pin.Device(), "inode": pin.Inode()}, "predecessor": legacy, "mode": "operation_required", "target": target, "legacy_boot_id": boot, "accepted_start": true, "successor_boot_id": boot, "successor_pid": 102, "successor_process_start": "historical-successor", "completed": true},
	}
	data, err := task.MarshalCanonical(map[string]any{"schema_version": task.SchemaVersion, "migrations": []any{record}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, ".supervisor", "migration-history.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return operation
}

func TestSupervisorUninitializedPublicReadinessUsesInstalledPairMetadata(t *testing.T) {
	store := newBareInspectionStore(t)
	client, _ := requireRuntimeRestartCLINativeContext(t)
	deps := Dependencies{InitialSupervisorExecutable: client}
	before := snapshotMaintenanceStatusRoot(t, store.Root)
	result := supervisorStatus(store.Root, deps, newSupervisorCommandResponse("status"))
	control := result.response.SupervisorControl
	if result.code != 0 || result.err != nil || control.State != "not_initialized" || control.NextAction != "none" || control.InstallationBinding == nil {
		t.Fatalf("uninitialized status changed: code=%d control=%+v err=%v", result.code, control, result.err)
	}
	binding := control.InstallationBinding
	if binding.Relation != installation.BindingNotInitialized || binding.Client.SelectedAvailability != installation.AvailabilityPresent || binding.Daemon.SelectedAvailability != installation.AvailabilityPresent {
		t.Fatalf("uninitialized public pair metadata is not ready for bootstrap-on-dispatch: %+v", binding)
	}
	data, err := json.Marshal(result.response)
	if err != nil || !json.Valid(data) {
		t.Fatalf("invalid public status: %v", err)
	}
	if pending, err := inspection.GroupMigrationPending(t.Context(), store); err != nil || pending {
		t.Fatalf("passive status created an inspection edge: %v", err)
	}
	assertMaintenanceStatusRootUnchanged(t, store.Root, before)
	if _, found, err := pueue.ReadPrivateSupervisorBinding(store.Root); err != nil || found {
		t.Fatalf("passive status bootstrapped Native: found=%t err=%v", found, err)
	}
}
