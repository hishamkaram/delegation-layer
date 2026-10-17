package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestResolvePrivateSupervisorGenerationRejectsExternalHeadWithoutPublishedBinding(t *testing.T) {
	store := newBareInspectionStore(t)
	saved := supervisorGenerationTestBinding(store.Root, "saved", true)
	external := supervisorGenerationTestBinding(t.TempDir(), "external", false)
	writeSupervisorGenerationTestGroup(t, store, external)

	err := resolveSupervisorGenerationTestWithLock(t, store, saved)
	assertExternalSupervisorGenerationError(t, err)
}

func TestResolvePrivateSupervisorGenerationRejectsExternalHeadWithPublishedBinding(t *testing.T) {
	store := newBareInspectionStore(t)
	current := supervisorGenerationTestBinding(store.Root, "current", true)
	writeSupervisorGenerationTestPrivateGeneration(t, store, current)
	external := supervisorGenerationTestBinding(t.TempDir(), "external", false)
	writeSupervisorGenerationTestGroup(t, store, external)

	err := resolveSupervisorGenerationTestWithLock(t, store, current)
	assertExternalSupervisorGenerationError(t, err)
}

func TestResolvePrivateSupervisorGenerationPreservesCompletedPrivateGroupChain(t *testing.T) {
	store := newBareInspectionStore(t)
	legacy := supervisorGenerationTestBinding(store.Root, "legacy", true)
	successor := supervisorGenerationTestBinding(store.Root, "successor", true)
	writeSupervisorGenerationTestPrivateGeneration(t, store, successor)
	writeSupervisorGenerationTestHistory(t, store, legacy, successor)
	writeSupervisorGenerationTestGroup(t, store, legacy)

	resolution, err := resolveSupervisorGenerationTestWithResult(t, store, legacy)
	if err != nil {
		t.Fatalf("completed private group chain failed: %v", err)
	}
	if !resolution.PrivateAuthority || resolution.Active != successor || !resolution.GroupPresent || !resolution.GroupCleanupDue {
		t.Fatalf("completed private group chain was not preserved: %+v", resolution)
	}
}

func assertExternalSupervisorGenerationError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, inspection.ErrGroupMigrationRequired) {
		t.Fatalf("external inspection head error=%v, want ErrGroupMigrationRequired", err)
	}
	if !errors.Is(err, supervisorcore.ErrExternalInspectionHandoffRequired) {
		t.Fatalf("external inspection head error=%v, want errExternalInspectionHandoffRequired", err)
	}
	failure := dispatchFailure(admission.WithStage("start-supervisor", err), "not_created")
	if failure.Code != "supervisor_upgrade_required" || failure.Stage != "start-supervisor" || failure.NextAction != "run_supervisor_migrate" {
		t.Fatalf("external inspection error was not actionable: %+v", failure)
	}
	if failure.RecoveryCommand != "delegate supervisor migrate --root <same-state-root> --json" {
		t.Fatalf("external inspection error omitted managed recovery command: %+v", failure)
	}
	if dispatchFailureMessage(failure.Code) == "Delegate could not complete dispatch." {
		t.Fatalf("external inspection failure has no user-facing message: %+v", failure)
	}
}

func resolveSupervisorGenerationTestWithLock(t *testing.T, store *taskdir.Store, saved task.SupervisorRef) error {
	t.Helper()
	_, err := resolveSupervisorGenerationTestWithResult(t, store, saved)
	return err
}

func resolveSupervisorGenerationTestWithResult(t *testing.T, store *taskdir.Store, saved task.SupervisorRef) (supervisorcore.Generation, error) {
	t.Helper()
	var resolution supervisorcore.Generation
	err := pueue.WithBootstrapLockContext(t.Context(), store.Root, func(ctx context.Context) error {
		var resolveErr error
		resolution, resolveErr = supervisorcore.ResolveGenerationWithLock(ctx, store, saved)
		return resolveErr
	})
	return resolution, err
}

func writeSupervisorGenerationTestGroup(t *testing.T, store *taskdir.Store, supervisor task.SupervisorRef) {
	t.Helper()
	group := inspection.GroupRequestRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		Supervisor:    supervisor,
		Name:          (pueue.InspectionIdentity{RootID: store.RootID}).Group(),
		ParallelTasks: 1,
	}
	control, err := store.OpenInspectionGroup(true)
	if err != nil {
		t.Fatal(err)
	}
	created, putErr := control.Put("request.json", group)
	closeErr := control.Close()
	if putErr != nil {
		t.Fatal(putErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if !created {
		t.Fatal("inspection group fixture was not created")
	}
}

func writeSupervisorGenerationTestPrivateGeneration(t *testing.T, store *taskdir.Store, binding task.SupervisorRef) {
	t.Helper()
	base := filepath.Dir(pueue.PrivateConfigPath(store.Root))
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := pueue.PublishPrivateSupervisorBindingIfAbsent(store.Root, binding); err != nil {
		t.Fatal(err)
	}
	identity := historicalPrivateDaemonIdentity{
		Executable:           binding.DaemonExecutable,
		SHA256:               binding.DaemonSHA256,
		ConfigPath:           binding.ConfigPath,
		ConfigDigest:         binding.ConfigDigest,
		ResolvedConfigSHA256: binding.ResolvedConfigSHA256,
		Endpoint:             binding.Endpoint,
	}
	data, err := task.MarshalCanonical(identity)
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(filepath.Join(base, "daemon.identity.json"), data, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	complete, err := pueue.PrivateSupervisorMigrationComplete(store.Root, binding)
	if err != nil || !complete {
		t.Fatalf("private generation fixture is incomplete: complete=%t err=%v", complete, err)
	}
}

func writeSupervisorGenerationTestHistory(t *testing.T, store *taskdir.Store, legacy, successor task.SupervisorRef) {
	t.Helper()
	history := historicalPrivateMigrationHistory{
		SchemaVersion: task.SchemaVersion,
		Migrations: []historicalPrivateMigrationRecord{{
			SchemaVersion: task.SchemaVersion,
			Legacy:        historicalPrivateDaemonIdentityFor(legacy),
			Successor:     historicalPrivateDaemonIdentityFor(successor),
			LegacyPID:     101,
			LegacyStopped: true,
		}},
	}
	data, err := task.MarshalCanonical(history)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(pueue.PrivateConfigPath(store.Root))
	if err := os.WriteFile(filepath.Join(base, "migration-history.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func supervisorGenerationTestBinding(root, label string, private bool) task.SupervisorRef {
	base := filepath.Join(root, "supervisor-"+label)
	configPath := filepath.Join(base, "pueue.yml")
	if private {
		configPath = pueue.PrivateConfigPath(root)
		base = filepath.Dir(configPath)
	}
	return task.SupervisorRef{
		ClientExecutable:     filepath.Join(base, "pueue"),
		ClientSHA256:         task.ComputeSHA256([]byte("client:" + label)),
		DaemonExecutable:     filepath.Join(base, "pueued"),
		DaemonSHA256:         task.ComputeSHA256([]byte("daemon:" + label)),
		ResolutionOS:         "linux",
		ResolutionHome:       filepath.Join(root, "home"),
		ResolutionDataLocal:  filepath.Join(root, "data-local"),
		ResolutionConfig:     filepath.Join(root, "config"),
		ResolutionRuntime:    filepath.Join(root, "runtime"),
		ResolutionUsername:   "fixture",
		ResolvedConfigSHA256: task.ComputeSHA256([]byte("resolved-config:" + label)),
		Endpoint:             "unix:" + filepath.Join(base, "run", "pueue.sock"),
		ConfigPath:           configPath,
		ConfigDigest:         task.ComputeSHA256([]byte("config:" + label)),
		ObservedVersion:      "pueue 4.0.4",
	}
}

func historicalPrivateDaemonIdentityFor(binding task.SupervisorRef) historicalPrivateDaemonIdentity {
	return historicalPrivateDaemonIdentity{
		Executable:           binding.DaemonExecutable,
		SHA256:               binding.DaemonSHA256,
		ConfigPath:           binding.ConfigPath,
		ConfigDigest:         binding.ConfigDigest,
		ResolvedConfigSHA256: binding.ResolvedConfigSHA256,
		Endpoint:             binding.Endpoint,
	}
}
