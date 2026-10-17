package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestInstallStateRunnerRejectsContentAddressCollision(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	source := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, source, "trusted runner")
	digest, err := stateExecutableSourceDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, ".supervisor")
	if err = os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	collision := filepath.Join(base, stateRunnerName+"-"+digest)
	supervisorfixture.WriteExecutable(t, collision, "different runner")
	if _, err = InstallStateRunner(root, source); err == nil {
		t.Fatal("content-addressed path collision was overwritten")
	}
	if data, readErr := os.ReadFile(collision); readErr != nil || string(data) != "different runner" {
		t.Fatalf("collision target changed = %q, err=%v", data, readErr)
	}
}

func TestManagedRunnerExecutableSkipsExternalCustomRunner(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	custom := filepath.Join(supervisorfixture.CanonicalTempDir(t), "custom-delegate-run")
	supervisorfixture.WriteExecutable(t, custom, "custom runner")
	if managedRunnerExecutable(root, custom, task.RunnerOwnershipCustom) {
		t.Fatal("external custom runner was classified as a managed Delegation Layer runner")
	}
}

func TestManagedRunnerExecutablePreservesAmbiguousRemovedLegacyRunner(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	removed := filepath.Join(supervisorfixture.CanonicalTempDir(t), stateRunnerName)
	if managedRunnerExecutable(root, removed, "") {
		t.Fatal("ambiguous removed legacy runner was guessed to be managed")
	}
	if managedRunnerExecutable(root, removed, task.RunnerOwnershipCustom) {
		t.Fatal("explicitly custom runner was reclassified as managed")
	}
	custom := filepath.Join(supervisorfixture.CanonicalTempDir(t), "custom-runner")
	if managedRunnerExecutable(root, custom, "") {
		t.Fatal("missing custom runner was classified as managed")
	}
	removedStateRunner := filepath.Join(root, ".supervisor", stateRunnerName+"-"+strings.Repeat("a", 64))
	if !managedRunnerExecutable(root, removedStateRunner, "") {
		t.Fatal("removed immutable state runner lost its durable managed identity")
	}
}

func TestManagedInspectionWorkerRequiresMatchingContentAddress(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	source := filepath.Join(supervisorfixture.CanonicalTempDir(t), stateRunnerName)
	supervisorfixture.WriteExecutable(t, source, "managed runner bytes")
	managed := installStateRunnerFixture(t, root, source)
	digest, err := stateExecutableSourceDigest(managed)
	if err != nil {
		t.Fatal(err)
	}
	if !ManagedInspectionWorkerExecutable(root, inspection.Binding{
		WorkerExecutable: managed, WorkerSHA256: digest, RunnerOwnership: task.RunnerOwnershipManaged,
	}) {
		t.Fatal("verified explicitly managed state-root worker was not recognized")
	}
	if ManagedInspectionWorkerExecutable(root, inspection.Binding{
		WorkerExecutable: managed, WorkerSHA256: digest, RunnerOwnership: task.RunnerOwnershipCustom,
	}) {
		t.Fatal("explicit custom state-root worker was reclassified as managed")
	}
	if ManagedInspectionWorkerExecutable(root, inspection.Binding{WorkerExecutable: managed, WorkerSHA256: digest}) {
		t.Fatal("legacy worker with unknown ownership was guessed to be managed")
	}

	spoofed := filepath.Join(root, ".supervisor", stateRunnerName+"-"+strings.Repeat("a", 64))
	supervisorfixture.WriteExecutable(t, spoofed, "runner bytes")
	digest, err = stateExecutableSourceDigest(spoofed)
	if err != nil {
		t.Fatal(err)
	}
	if digest == strings.Repeat("a", 64) {
		t.Fatal("fixture unexpectedly matched the spoofed content address")
	}
	if ManagedInspectionWorkerExecutable(root, inspection.Binding{
		WorkerExecutable: spoofed, WorkerSHA256: digest, RunnerOwnership: task.RunnerOwnershipManaged,
	}) {
		t.Fatal("state-root worker with a mismatched content address was authorized as managed")
	}
}

func TestExplicitInspectionWorkerCannotBeAuthorizedForMigration(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	supervisor, _ := supervisorfixture.NewBlockingSupervisor(t, store.RootID)
	source := filepath.Join(supervisorfixture.CanonicalTempDir(t), stateRunnerName)
	supervisorfixture.WriteExecutable(t, source, "explicit custom runner")
	worker := installStateRunnerFixture(t, store.Root, source)
	digest, err := stateExecutableSourceDigest(worker)
	if err != nil {
		t.Fatal(err)
	}
	binding := inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: strings.Repeat("a", 64),
		HelperExecutable: "/usr/bin/true", HelperSHA256: strings.Repeat("a", 64),
		WorkerExecutable: worker, WorkerSHA256: digest, RunnerOwnership: task.RunnerOwnershipCustom,
		Supervisor: supervisor.Binding(),
	}
	operation, err := inspection.OpenOperation(store, request, binding, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = operation.AuthorizeManagedWorkerUpgradeContext(context.Background()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("explicit custom worker created migration authority: %v", err)
	}
	control, err := store.OpenInspection(request.TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = control.Put("worker-upgrade.json", inspection.ManagedWorkerUpgradeRecord{
		SchemaVersion: task.SchemaVersion,
		RequestSHA256: operation.Digest(),
	})
	err = errors.Join(err, control.Close())
	if err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	identity := pueue.InspectionIdentity{RootID: store.RootID, TaskID: request.TaskID}
	label := identity.Label()
	job := pueue.Job{ID: 17, Label: &label, Group: identity.Group(), State: pueue.StateQueued}
	if _, eligible, prepareErr := PrepareQueuedInspectionRunnerCommand(context.Background(), store, supervisor, job, request.TaskID); prepareErr != nil || eligible {
		t.Fatalf("explicit custom worker was eligible for migration: eligible=%v err=%v", eligible, prepareErr)
	}
	operation, err = inspection.LoadOperation(store, request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = authorizeInspectionManagedWorker(context.Background(), store.Root, operation, binding); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("existing migration marker overrode explicit custom ownership: %v", err)
	}
}

func TestManagedStateRunnerPathsOnlyReturnsVerifiedContentAddresses(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	source := filepath.Join(supervisorfixture.CanonicalTempDir(t), stateRunnerName)
	supervisorfixture.WriteExecutable(t, source, "runner v1")
	first, err := InstallStateRunner(root, source)
	if err != nil {
		t.Fatal(err)
	}
	supervisorfixture.WriteExecutable(t, source, "runner v2")
	second, err := InstallStateRunner(root, source)
	if err != nil {
		t.Fatal(err)
	}
	spoofed := filepath.Join(root, ".supervisor", stateRunnerName+"-"+strings.Repeat("b", 64))
	supervisorfixture.WriteExecutable(t, spoofed, "not the named digest")

	got, err := managedStateRunnerPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, path := range got {
		seen[path] = true
	}
	if len(got) != 2 || !seen[first] || !seen[second] {
		t.Fatalf("verified managed runners = %v, want [%q %q]", got, first, second)
	}
}

func TestPrepareQueuedInspectionRunnerCommandAuthorizesManagedUpgrade(t *testing.T) {
	fixture := newQueuedInspectionUpgradeFixture(t)
	if !fixture.replacement.Inspection || fixture.replacement.OldRunner != fixture.oldRunner || fixture.replacement.Label != (pueue.InspectionIdentity{RootID: fixture.store.RootID, TaskID: fixture.request.TaskID}).Label() {
		t.Fatalf("inspection runner migration = %+v", fixture.replacement)
	}
	wrongGroup := pueue.Job{ID: fixture.replacement.NumericID, Label: &fixture.replacement.Label, Group: "default", State: pueue.StateQueued}
	if _, eligible, err := PrepareQueuedInspectionRunnerCommand(context.Background(), fixture.store, fixture.supervisor, wrongGroup, fixture.request.TaskID); !errors.Is(err, pueue.ErrBinding) || eligible {
		t.Fatalf("inspection worker in a different group was accepted: eligible=%v err=%v", eligible, err)
	}
}

type queuedInspectionUpgradeFixture struct {
	store       *taskdir.Store
	supervisor  *pueue.Client
	request     task.TaskRecord
	replacement pueue.RunnerCommandReplacement
	oldRunner   string
}

func newQueuedInspectionUpgradeFixture(t *testing.T) queuedInspectionUpgradeFixture {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	supervisor, _ := supervisorfixture.NewBlockingSupervisor(t, store.RootID)
	oldSource := filepath.Join(supervisorfixture.CanonicalTempDir(t), stateRunnerName)
	supervisorfixture.WriteExecutable(t, oldSource, "old inspection worker")
	oldRunner := installStateRunnerFixture(t, store.Root, oldSource)
	digest, err := stateExecutableSourceDigest(oldRunner)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: strings.Repeat("a", 64),
		HelperExecutable: "/usr/bin/true", HelperSHA256: strings.Repeat("a", 64),
		WorkerExecutable: oldRunner, WorkerSHA256: digest,
		RunnerOwnership: task.RunnerOwnershipManaged,
		Supervisor:      supervisor.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	identity := pueue.InspectionIdentity{RootID: store.RootID, TaskID: request.TaskID}
	label := identity.Label()
	job := pueue.Job{ID: 17, Label: &label, Group: identity.Group(), State: pueue.StateQueued}
	replacement, eligible, err := PrepareQueuedInspectionRunnerCommand(context.Background(), store, supervisor, job, request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !eligible {
		t.Fatal("managed inspection worker was not eligible for queue migration")
	}
	newSource := filepath.Join(supervisorfixture.CanonicalTempDir(t), stateRunnerName)
	supervisorfixture.WriteExecutable(t, newSource, "new inspection worker")
	replacement.NewRunner = installStateRunnerFixture(t, store.Root, newSource)
	return queuedInspectionUpgradeFixture{store: store, supervisor: supervisor, request: request, replacement: replacement, oldRunner: oldRunner}
}

func TestQueuedRunnerMigrationRecordsLegacyOwnership(t *testing.T) {
	root := filepath.Join(supervisorfixture.CanonicalTempDir(t), "state")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(root, ".supervisor", stateRunnerName)
	supervisorfixture.WriteExecutable(t, runner, "managed state runner")
	td, req, _ := newLegacyRunnerTask(t, store, runner)
	defer closeAppTestTask(t, store, td)

	replacement := pueue.RunnerCommandReplacement{TaskID: req.TaskID, OldRunner: runner}
	if err = recordQueuedManagedRunnerMigrations(context.Background(), store, []pueue.RunnerCommandReplacement{replacement}); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(runner); err != nil {
		t.Fatal(err)
	}
	if err = recordQueuedManagedRunnerMigrations(context.Background(), store, []pueue.RunnerCommandReplacement{replacement}); err != nil {
		t.Fatalf("repeat migration required the removed legacy runner: %v", err)
	}
	if recorded, err := td.HasManagedRunnerMigration(); err != nil || !recorded {
		t.Fatalf("queued runner migration receipt = %v, err=%v", recorded, err)
	}
}

func newLegacyRunnerTask(t *testing.T, store *taskdir.Store, runner string) (*taskdir.TaskDir, *task.TaskRecord, *task.MetaRecord) {
	t.Helper()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("legacy task")
	requested := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	digest := task.ComputeSHA256([]byte("runner migration test"))
	req := &task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: strings.Repeat("e", 32),
		Provider: "fixture:test", Mode: "read-only", CanonicalCwd: cwd, RequestedConfig: requested,
		BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)),
	}
	meta := &task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: req.TaskID,
		RequestedConfig: requested, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest},
		Containment: "fixture-only", Approval: "never", ProviderExecutable: "/tmp/fixture-provider", ProviderVersion: "fixture-v1",
		RunnerExecutable: runner, Environment: []string{"HOME=" + supervisorfixture.CanonicalTempDir(t)},
		PublisherBuild: "runner-migration-test", PublisherVersion: "runner-migration-test", Predicate: task.FixturePredicateRef(),
		SupervisorConfig: task.SupervisorRef{
			ClientExecutable: "/tmp/pueue", ClientSHA256: digest, ResolvedConfigSHA256: digest,
			ConfigPath: pueue.PrivateConfigPath(store.Root), ConfigDigest: digest,
			Endpoint: "unix:" + filepath.Join(store.Root, ".supervisor", "run", "pueue.sock"), ObservedVersion: pueue.FixtureVersion,
		},
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	td, err := store.CreateTask(req.TaskID, req, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	return td, req, meta
}

func TestManagedRunnerBuildInfoUsesExecutablePath(t *testing.T) {
	if !ManagedRunnerBuildInfo(&debug.BuildInfo{Path: managedRunnerMain}) {
		t.Fatal("official delegate-run executable was not recognized")
	}
	if ManagedRunnerBuildInfo(&debug.BuildInfo{
		Path: "github.com/hishamkaram/delegation-layer",
		Main: debug.Module{Path: managedRunnerMain},
	}) {
		t.Fatal("module path was incorrectly treated as the executable package path")
	}
}
