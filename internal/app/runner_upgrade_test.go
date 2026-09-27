package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestInstallStateRunnerKeepsExecutableVersionsImmutable(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	source := filepath.Join(canonicalAppTestTempDir(t), "delegate-run")
	writeRunnerFixture(t, source, "old runner")

	oldRunner := installStateRunnerFixture(t, root, source)
	if !isStateRunnerPath(root, oldRunner) || oldRunner == filepath.Join(root, ".supervisor", stateRunnerName) {
		t.Fatalf("state runner path = %q, want an immutable content-addressed path", oldRunner)
	}
	requireRunnerFixture(t, oldRunner, "old runner")

	writeRunnerFixture(t, source, "upgraded runner")
	newRunner := installStateRunnerFixture(t, root, source)
	if newRunner == oldRunner {
		t.Fatal("different runner bytes reused an immutable path")
	}
	requireRunnerFixture(t, oldRunner, "old runner")
	requireRunnerFixture(t, newRunner, "upgraded runner")
	info, err := os.Stat(newRunner)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("state runner permissions = %o, want 700", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(newRunner))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("immutable runner installation left unexpected files: %+v", entries)
	}
	if !managedRunnerExecutable(root, oldRunner, task.RunnerOwnershipManaged) {
		t.Fatal("old content-addressed state runner was not recognized as managed")
	}
	if !managedRunnerExecutable(root, newRunner, task.RunnerOwnershipManaged) {
		t.Fatal("new content-addressed state runner was not recognized as managed")
	}
}

func TestUnprotectedStateRunnerIsRejectedByInstallationAndScanners(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	source := filepath.Join(canonicalAppTestTempDir(t), "delegate-run")
	writeRunnerFixture(t, source, "runner")
	runner := installStateRunnerFixture(t, root, source)
	digest, err := stateExecutableSourceDigest(runner)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(runner, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err = stateExecutableMatches(runner, digest); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("state executable matcher accepted mode 0755: %v", err)
	}
	if _, err = installStateRunner(root, runner); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("state runner installer accepted its content-addressed source with mode 0755: %v", err)
	}
	if _, err = installStateRunner(root, source); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("state runner installation accepted mode 0755: %v", err)
	}
	if currentManagedRunnerExecutable(root, runner) {
		t.Fatal("managed runner scanner accepted mode 0755")
	}
	if _, err = managedStateRunnerPaths(root); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("state runner path scan accepted mode 0755: %v", err)
	}
}

func installStateRunnerFixture(t *testing.T, root, source string) string {
	t.Helper()
	runner, err := installStateRunner(root, source)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func requireRunnerFixture(t *testing.T, runner, contents string) {
	t.Helper()
	data, err := os.ReadFile(runner)
	if err != nil || string(data) != contents {
		t.Fatalf("runner content = %q, want %q (err=%v)", data, contents, err)
	}
}

func TestRemoveStateExecutableStageSyncsAfterUnlink(t *testing.T) {
	base := canonicalAppTestTempDir(t)
	stagePath := filepath.Join(base, ".delegate-run-stage")
	if err := os.WriteFile(stagePath, []byte("stage"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("directory barrier failed")
	called := false
	err := removeStateExecutableStage(base, stagePath, func(path string) error {
		called = true
		if path != base {
			t.Fatalf("directory barrier path = %q, want %q", path, base)
		}
		if _, statErr := os.Lstat(stagePath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("directory barrier ran before stage unlink: %v", statErr)
		}
		return wantErr
	})
	if !called || !errors.Is(err, wantErr) {
		t.Fatalf("stage cleanup result = %v, barrier called=%v", err, called)
	}
}

func TestInstallStateRunnerRejectsSupervisorSymlink(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	target := canonicalAppTestTempDir(t)
	if err := os.Symlink(target, filepath.Join(root, ".supervisor")); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(canonicalAppTestTempDir(t), "delegate-run")
	writeRunnerFixture(t, source, "runner")
	if _, err := installStateRunner(root, source); err == nil {
		t.Fatal("state runner accepted a symlinked supervisor directory")
	}
}

func TestPrepareAdmissionExecutablesKeepsExplicitRunner(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	custom := filepath.Join(canonicalAppTestTempDir(t), "custom-delegate-run")
	writeRunnerFixture(t, custom, "custom runner")
	got, err := prepareAdmissionExecutables(Arguments{
		Runner:      custom,
		PueueConfig: filepath.Join(canonicalAppTestTempDir(t), "external-pueue.yml"),
	}, Dependencies{}, root)
	if err != nil {
		t.Fatal(err)
	}
	if got != custom {
		t.Fatalf("explicit runner changed to %q, want %q", got, custom)
	}
	if _, err = os.Lstat(filepath.Join(root, ".supervisor")); !os.IsNotExist(err) {
		t.Fatalf("custom runner created a state supervisor directory: %v", err)
	}
}

func TestPrepareAdmissionExecutablesRejectsPrivateConfigBeforeInstallingState(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	_, err := prepareAdmissionExecutables(Arguments{PueueConfig: pueue.PrivateConfigPath(root)}, Dependencies{}, root)
	if !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("private config collision error = %v, want configuration error", err)
	}
	if _, err = os.Lstat(filepath.Join(root, ".supervisor")); !os.IsNotExist(err) {
		t.Fatalf("rejected config collision left supervisor state: %v", err)
	}
}

func TestPrepareAdmissionExecutablesUsesSavedPrivateSupervisorPair(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	runnerSource := filepath.Join(canonicalAppTestTempDir(t), "delegate-run")
	writeRunnerFixture(t, runnerSource, "managed runner")
	delegate := filepath.Join(canonicalAppTestTempDir(t), "bin", "delegate")
	writeRunnerFixture(t, delegate, "delegate")
	client, daemon := writeSavedSupervisorPair(t)
	saved := savedSupervisorPairRef(t, client, daemon)
	saved.ConfigPath = pueue.PrivateConfigPath(root)

	got, err := prepareAdmissionExecutables(Arguments{savedSupervisor: &saved}, Dependencies{
		InitialSupervisorExecutable: delegate,
		RunnerExecutable:            runnerSource,
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	if !isStateRunnerPath(root, got) {
		t.Fatalf("managed runner path = %q, want state-root runner", got)
	}
	requireResolvedSupervisorPair(t, filepath.Join(root, ".supervisor"), "saved client", "saved daemon")
}

func TestPrepareAdmissionExecutablesUsesStateRootPairWhenInstallPairsAreUnavailable(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	client, daemon := writeSavedSupervisorPair(t)
	saved := savedSupervisorPairRef(t, client, daemon)
	saved.ConfigPath = pueue.PrivateConfigPath(root)
	if err := installStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(client); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(daemon); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", canonicalAppTestTempDir(t))

	runner := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, runner, "managed runner")
	missingDelegate := filepath.Join(canonicalAppTestTempDir(t), "missing", "delegate")
	got, err := prepareAdmissionExecutables(Arguments{savedSupervisor: &saved}, Dependencies{
		InitialSupervisorExecutable: missingDelegate,
		RunnerExecutable:            runner,
	}, root)
	if err != nil {
		t.Fatalf("admission could not reuse the verified state-root supervisor pair: %v", err)
	}
	if !isStateRunnerPath(root, got) {
		t.Fatalf("admission runner = %q, want managed state-root runner", got)
	}
	requireResolvedSupervisorPair(t, filepath.Join(root, ".supervisor"), "saved client", "saved daemon")
}

func TestResolvePrivateSupervisorExecutablesPrefersStateRootPairToPATH(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	client, daemon := writeSavedSupervisorPair(t)
	if err := installStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
		t.Fatal(err)
	}

	pathDir := canonicalAppTestTempDir(t)
	pathClient := filepath.Join(pathDir, "pueue")
	pathDaemon := filepath.Join(pathDir, "pueued")
	writeRunnerFixture(t, pathClient, "unrelated PATH client")
	writeRunnerFixture(t, pathDaemon, "unrelated PATH daemon")
	t.Setenv("PATH", pathDir)

	gotClient, gotDaemon, err := resolvePrivateSupervisorExecutables(root, Dependencies{})
	if err != nil {
		t.Fatalf("resolve private supervisor pair: %v", err)
	}
	wantClient, wantDaemon, found, err := resolveStateSupervisorPair(filepath.Join(root, ".supervisor"))
	if err != nil || !found {
		t.Fatalf("resolve verified state-root pair: found=%v err=%v", found, err)
	}
	if gotClient != wantClient || gotDaemon != wantDaemon {
		t.Fatalf("private pair = %q / %q, want state-root pair %q / %q", gotClient, gotDaemon, wantClient, wantDaemon)
	}
	if gotClient == pathClient || gotDaemon == pathDaemon {
		t.Fatal("private supervisor selected unrelated PATH executables")
	}
}

func TestBindInitialUsesStateRootPairWhenBundleIsUnavailable(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	sourceDirectory := canonicalAppTestTempDir(t)
	clientSource := filepath.Join(sourceDirectory, "pueue")
	daemonSource := filepath.Join(sourceDirectory, "pueued")
	writeRunnerFixture(t, clientSource, `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c|--config) config=$2; shift 2 ;;
    --version) printf '%s\n' 'pueue 99.7.3'; exit 0 ;;
    status)
      base=${config%/*}
      [ -f "$base/ready" ] || exit 1
      printf '%s\n' '{"tasks":{},"groups":{}}'
      exit 0 ;;
    *) shift ;;
  esac
done
exit 64
`)
	writeRunnerFixture(t, daemonSource, `#!/bin/sh
set -eu
config=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c|--config) config=$2; shift 2 ;;
    *) shift ;;
  esac
done
base=${config%/*}
: > "$base/ready"
`)
	if err := installStateSupervisorPair(context.Background(), root, clientSource, daemonSource); err != nil {
		t.Fatal(err)
	}
	clientPath, daemonPath, found, err := resolveStateSupervisorPair(filepath.Join(root, ".supervisor"))
	if err != nil || !found {
		t.Fatalf("state-root pair was not installed: found=%v err=%v", found, err)
	}
	if err = os.Remove(clientSource); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(daemonSource); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", canonicalAppTestTempDir(t))
	missingDelegate := filepath.Join(canonicalAppTestTempDir(t), "missing", "delegate")
	client, err := bindInitialWithOptions(Arguments{}, Dependencies{InitialSupervisorExecutable: missingDelegate}, root, pueue.Options{ObservationTimeout: time.Second})
	if err != nil {
		t.Fatalf("private supervisor did not bind from its verified state-root pair: %v", err)
	}
	if binding := client.Binding(); binding.ClientExecutable != clientPath || binding.DaemonExecutable != daemonPath {
		t.Fatalf("private binding used a different executable pair: got=(%q,%q) want=(%q,%q)", binding.ClientExecutable, binding.DaemonExecutable, clientPath, daemonPath)
	}
}

func TestInstallStateRunnerRejectsContentAddressCollision(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	source := filepath.Join(canonicalAppTestTempDir(t), "delegate-run")
	writeRunnerFixture(t, source, "trusted runner")
	digest, err := stateExecutableSourceDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, ".supervisor")
	if err = os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	collision := filepath.Join(base, stateRunnerName+"-"+digest)
	writeRunnerFixture(t, collision, "different runner")
	if _, err = installStateRunner(root, source); err == nil {
		t.Fatal("content-addressed path collision was overwritten")
	}
	if data, readErr := os.ReadFile(collision); readErr != nil || string(data) != "different runner" {
		t.Fatalf("collision target changed = %q, err=%v", data, readErr)
	}
}

func TestManagedRunnerExecutableSkipsExternalCustomRunner(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	custom := filepath.Join(canonicalAppTestTempDir(t), "custom-delegate-run")
	writeRunnerFixture(t, custom, "custom runner")
	if managedRunnerExecutable(root, custom, task.RunnerOwnershipCustom) {
		t.Fatal("external custom runner was classified as a managed Delegation Layer runner")
	}
}

func TestManagedRunnerExecutablePreservesAmbiguousRemovedLegacyRunner(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	removed := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	if managedRunnerExecutable(root, removed, "") {
		t.Fatal("ambiguous removed legacy runner was guessed to be managed")
	}
	if managedRunnerExecutable(root, removed, task.RunnerOwnershipCustom) {
		t.Fatal("explicitly custom runner was reclassified as managed")
	}
	custom := filepath.Join(canonicalAppTestTempDir(t), "custom-runner")
	if managedRunnerExecutable(root, custom, "") {
		t.Fatal("missing custom runner was classified as managed")
	}
	removedStateRunner := filepath.Join(root, ".supervisor", stateRunnerName+"-"+strings.Repeat("a", 64))
	if !managedRunnerExecutable(root, removedStateRunner, "") {
		t.Fatal("removed immutable state runner lost its durable managed identity")
	}
}

func TestManagedInspectionWorkerRequiresMatchingContentAddress(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	source := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, source, "managed runner bytes")
	managed := installStateRunnerFixture(t, root, source)
	digest, err := stateExecutableSourceDigest(managed)
	if err != nil {
		t.Fatal(err)
	}
	if !managedInspectionWorkerExecutable(root, inspection.Binding{
		WorkerExecutable: managed, WorkerSHA256: digest, RunnerOwnership: task.RunnerOwnershipManaged,
	}) {
		t.Fatal("verified explicitly managed state-root worker was not recognized")
	}
	if managedInspectionWorkerExecutable(root, inspection.Binding{
		WorkerExecutable: managed, WorkerSHA256: digest, RunnerOwnership: task.RunnerOwnershipCustom,
	}) {
		t.Fatal("explicit custom state-root worker was reclassified as managed")
	}
	if managedInspectionWorkerExecutable(root, inspection.Binding{WorkerExecutable: managed, WorkerSHA256: digest}) {
		t.Fatal("legacy worker with unknown ownership was guessed to be managed")
	}

	spoofed := filepath.Join(root, ".supervisor", stateRunnerName+"-"+strings.Repeat("a", 64))
	writeRunnerFixture(t, spoofed, "runner bytes")
	digest, err = stateExecutableSourceDigest(spoofed)
	if err != nil {
		t.Fatal(err)
	}
	if digest == strings.Repeat("a", 64) {
		t.Fatal("fixture unexpectedly matched the spoofed content address")
	}
	if managedInspectionWorkerExecutable(root, inspection.Binding{
		WorkerExecutable: spoofed, WorkerSHA256: digest, RunnerOwnership: task.RunnerOwnershipManaged,
	}) {
		t.Fatal("state-root worker with a mismatched content address was authorized as managed")
	}
}

func TestExplicitInspectionWorkerCannotBeAuthorizedForMigration(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	supervisor, _ := newBlockingInspectionSupervisor(t, store.RootID)
	source := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, source, "explicit custom runner")
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
	if _, eligible, prepareErr := prepareQueuedInspectionRunnerCommand(context.Background(), store, supervisor, job, request.TaskID); prepareErr != nil || eligible {
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
	root := canonicalAppTestTempDir(t)
	source := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, source, "runner v1")
	first, err := installStateRunner(root, source)
	if err != nil {
		t.Fatal(err)
	}
	writeRunnerFixture(t, source, "runner v2")
	second, err := installStateRunner(root, source)
	if err != nil {
		t.Fatal(err)
	}
	spoofed := filepath.Join(root, ".supervisor", stateRunnerName+"-"+strings.Repeat("b", 64))
	writeRunnerFixture(t, spoofed, "not the named digest")

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
	if _, eligible, err := prepareQueuedInspectionRunnerCommand(context.Background(), fixture.store, fixture.supervisor, wrongGroup, fixture.request.TaskID); !errors.Is(err, pueue.ErrBinding) || eligible {
		t.Fatalf("inspection worker in a different group was accepted: eligible=%v err=%v", eligible, err)
	}
}

func TestQueuedInspectionWorkerAcceptsAuthorizedUpgradeAfterOldBinaryRemoval(t *testing.T) {
	fixture := newQueuedInspectionUpgradeFixture(t)
	operation, err := inspection.LoadOperation(fixture.store, fixture.request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateInspectionWorkerExecutable(context.Background(), fixture.store.Root, operation, operation.Request().Binding, fixture.replacement.NewRunner); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unrecorded managed successor was accepted: %v", err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	if err = authorizeQueuedInspectionRunnerUpgrade(context.Background(), fixture.store, fixture.replacement); err != nil {
		t.Fatalf("managed queue migration was not durably authorized: %v", err)
	}
	if err = os.Remove(fixture.oldRunner); err != nil {
		t.Fatal(err)
	}
	operation, err = inspection.LoadOperation(fixture.store, fixture.request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = validateInspectionWorkerExecutable(context.Background(), fixture.store.Root, operation, operation.Request().Binding, fixture.replacement.NewRunner); err != nil {
		t.Fatalf("verified managed successor failed after old worker removal: %v", err)
	}
}

func TestAdmissionInspectionReplayAcceptsAuthorizedManagedSuccessor(t *testing.T) {
	fixture := newQueuedInspectionUpgradeFixture(t)
	operation, err := inspection.LoadOperation(fixture.store, fixture.request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	original := operation.Request()
	if _, err = operation.ClaimSubmissionContext(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}

	workerDigest, err := stateExecutableSourceDigest(fixture.replacement.NewRunner)
	if err != nil {
		t.Fatal(err)
	}
	binding := original.Binding
	binding.WorkerExecutable = fixture.replacement.NewRunner
	binding.WorkerSHA256 = workerDigest
	if _, err = openAdmissionInspectionOperation(fixture.store, fixture.request, binding, time.Now()); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unverified managed successor was accepted: %v", err)
	}

	if err = authorizeQueuedInspectionRunnerUpgrade(context.Background(), fixture.store, fixture.replacement); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(fixture.oldRunner); err != nil {
		t.Fatal(err)
	}
	operation, err = openAdmissionInspectionOperation(fixture.store, fixture.request, binding, time.Now())
	if err != nil {
		t.Fatalf("authorized inspection admission replay failed: %v", err)
	}
	defer func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	replayed := operation.Request()
	if replayed.Binding.WorkerExecutable != original.Binding.WorkerExecutable || replayed.Binding.WorkerSHA256 != original.Binding.WorkerSHA256 {
		t.Fatalf("replay changed immutable worker binding: before=%+v after=%+v", original.Binding, replayed.Binding)
	}
	if err = submitInspectionOnce(context.Background(), operation, fixture.supervisor, binding.WorkerExecutable, fixture.store.Root); err != nil {
		t.Fatalf("already-submitted inspection was not reconciled through its original journal: %v", err)
	}
}

type completedManagedInspectionReplayFixture struct {
	store      *taskdir.Store
	request    task.TaskRecord
	supervisor *pueue.Client
	logPath    string
	binding    inspection.Binding
	oldRunner  string
	oldDigest  string
	newRunner  string
	newDigest  string
}

func newCompletedManagedInspectionReplayFixture(t *testing.T) completedManagedInspectionReplayFixture {
	t.Helper()
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	supervisor, logPath := newBlockingInspectionSupervisor(t, store.RootID)

	oldSource := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, oldSource, "old inspection worker")
	oldRunner := installStateRunnerFixture(t, store.Root, oldSource)
	oldDigest, err := stateExecutableSourceDigest(oldRunner)
	if err != nil {
		t.Fatal(err)
	}
	openedAt := time.Now().UTC()
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: strings.Repeat("a", 64),
		HelperExecutable: "/usr/bin/true", HelperSHA256: strings.Repeat("a", 64),
		WorkerExecutable: oldRunner, WorkerSHA256: oldDigest,
		RunnerOwnership: task.RunnerOwnershipManaged,
		Supervisor:      supervisor.Binding(),
	}, openedAt)
	if err != nil {
		t.Fatal(err)
	}

	submission, err := operation.ClaimSubmissionContext(context.Background(), openedAt.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = submission.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	startedAt := openedAt.Add(2 * time.Millisecond)
	start, err := operation.ClaimStart(startedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	completedAt := openedAt.Add(3 * time.Millisecond)
	if err = operation.Complete(inspection.ResultEligible, []byte(`{"eligible":true}`), completedAt); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(17); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordWorkerSuccess(17); err != nil {
		t.Fatal(err)
	}
	binding := operation.Request().Binding
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}

	newSource := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, newSource, "new inspection worker")
	newRunner := installStateRunnerFixture(t, store.Root, newSource)
	newDigest, err := stateExecutableSourceDigest(newRunner)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(oldRunner); err != nil {
		t.Fatal(err)
	}
	return completedManagedInspectionReplayFixture{
		store: store, request: request, supervisor: supervisor, logPath: logPath,
		binding: binding, oldRunner: oldRunner, oldDigest: oldDigest,
		newRunner: newRunner, newDigest: newDigest,
	}
}

func TestAdmissionInspectionReplayAcceptsCompletedManagedSuccessor(t *testing.T) {
	fixture := newCompletedManagedInspectionReplayFixture(t)
	binding := fixture.binding
	binding.WorkerExecutable = fixture.newRunner
	binding.WorkerSHA256 = fixture.newDigest
	replayed, err := openAdmissionInspectionOperation(fixture.store, fixture.request, binding, time.Now().UTC())
	if err != nil {
		t.Fatalf("completed operation could not be reopened after worker upgrade: %v", err)
	}
	defer func() {
		if closeErr := replayed.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if replayed.Request().Binding.WorkerExecutable != fixture.oldRunner || replayed.Request().Binding.WorkerSHA256 != fixture.oldDigest {
		t.Fatal("replay changed the immutable worker binding")
	}
	if authorized, authErr := replayed.ManagedWorkerUpgradeAuthorizedContext(context.Background()); authErr != nil || authorized {
		t.Fatalf("completed replay unexpectedly depended on queue authorization: authorized=%v err=%v", authorized, authErr)
	}
	meta := &task.MetaRecord{
		RunnerExecutable: fixture.newRunner,
		RunnerOwnership:  task.RunnerOwnershipManaged,
		SupervisorConfig: fixture.binding.Supervisor,
	}
	td := &taskdir.TaskDir{Dir: filepath.Join(fixture.store.Root, "tasks", fixture.request.TaskID)}
	if err = validateInspectionSubmissionRunner(replayed, fixture.store, td, &fixture.request, meta, fixture.newRunner); err != nil {
		t.Fatalf("ordinary task replay rejected its completed managed inspection proof: %v", err)
	}

	before, err := os.ReadFile(fixture.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = submitInspectionOnce(context.Background(), replayed, fixture.supervisor, fixture.newRunner, fixture.store.Root); err != nil {
		t.Fatalf("completed submitted operation was not replayable: %v", err)
	}
	after, err := os.ReadFile(fixture.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("replay launched or inspected another worker: before=%q after=%q", before, after)
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
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	supervisor, _ := newBlockingInspectionSupervisor(t, store.RootID)
	oldSource := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, oldSource, "old inspection worker")
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
	replacement, eligible, err := prepareQueuedInspectionRunnerCommand(context.Background(), store, supervisor, job, request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !eligible {
		t.Fatal("managed inspection worker was not eligible for queue migration")
	}
	newSource := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, newSource, "new inspection worker")
	replacement.NewRunner = installStateRunnerFixture(t, store.Root, newSource)
	return queuedInspectionUpgradeFixture{store: store, supervisor: supervisor, request: request, replacement: replacement, oldRunner: oldRunner}
}

func TestTimeoutContinuationDoesNotPinManagedRunner(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	meta := &task.MetaRecord{
		RunnerExecutable: filepath.Join(root, ".supervisor", stateRunnerName+"-"+strings.Repeat("c", 64)),
		RunnerOwnership:  task.RunnerOwnershipManaged,
	}
	commandRunner := timeoutContinuationCommandRunner(meta, persistedRunnerOwnership(root, meta))
	if commandRunner != "" {
		t.Fatalf("managed continuation pinned runner %q", commandRunner)
	}
	runner := meta.RunnerExecutable
	response := newResponse("status")
	records := []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: "budget"}, Observation: &task.StopObservedRecord{Terminated: true}}}
	applyTimeoutContinuationWithRunnerStateAndCommandRunner(&response, root, &task.TaskRecord{TaskID: strings.Repeat("d", 32), Provider: "pi:json"}, records, NativeCatalog(), &task.ProviderRefRecord{Provider: "pi:json", ConversationID: "session"}, runner, commandRunner, nil, true, true)
	if response.Continuation == nil || !response.Continuation.Resumable || strings.Contains(response.Continuation.ContinueCommand, "--runner") {
		t.Fatalf("managed continuation command = %+v", response.Continuation)
	}
}

func TestLegacyManagedRunnerMigrationSurvivesRemovedInstallation(t *testing.T) {
	root := filepath.Join(canonicalAppTestTempDir(t), "state")
	oldRunner := filepath.Join(canonicalAppTestTempDir(t), "old-install", stateRunnerName)
	writeRunnerFixture(t, oldRunner, "old managed runner")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	td, req, meta := newLegacyRunnerTask(t, store, oldRunner)
	defer closeAppTestTask(t, store, td)

	runnerDigest := task.ComputeSHA256([]byte("old managed runner"))
	if err = td.RecordManagedRunnerMigrationContext(context.Background(), runnerDigest); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(oldRunner); err != nil {
		t.Fatal(err)
	}
	ownership, err := runnerOwnershipForTask(root, td, meta)
	if err != nil || ownership != task.RunnerOwnershipManaged {
		t.Fatalf("legacy runner ownership after installation removal = %q, err=%v", ownership, err)
	}
	requireLegacyRunnerContinuation(t, root, req)
	requireLegacyRunnerTimeoutContinuation(t, root, req, meta, oldRunner, ownership)
	requireLegacyRunnerRetry(t, root, store, td, meta)
}

func requireLegacyRunnerContinuation(t *testing.T, root string, req *task.TaskRecord) {
	t.Helper()
	loadedReq, loadedMeta, loadedBrief, err := loadContinuationInput(root, Arguments{TaskID: req.TaskID}, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	continuation, err := buildContinuationArguments(Arguments{}, root, loadedReq, loadedMeta, loadedBrief)
	if err != nil {
		t.Fatal(err)
	}
	if continuation.Runner != "" || requestedRunnerOwnership(continuation) != task.RunnerOwnershipManaged {
		t.Fatalf("continuation retained removed legacy runner: %+v", continuation)
	}
}

func requireLegacyRunnerTimeoutContinuation(t *testing.T, root string, req *task.TaskRecord, meta *task.MetaRecord, runner, ownership string) {
	t.Helper()
	commandRunner := timeoutContinuationCommandRunner(meta, ownership)
	response := newResponse("status")
	stopRecords := []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: "budget"}, Observation: &task.StopObservedRecord{Terminated: true}}}
	applyTimeoutContinuationWithRunnerStateAndCommandRunner(
		&response, root, &task.TaskRecord{TaskID: req.TaskID, Provider: "pi:json"}, stopRecords,
		NativeCatalog(), &task.ProviderRefRecord{Provider: "pi:json", ConversationID: "session"},
		runner, commandRunner, nil, true, true,
	)
	if response.Continuation == nil || !response.Continuation.Resumable || strings.Contains(response.Continuation.ContinueCommand, "--runner") {
		t.Fatalf("migrated legacy timeout continuation = %+v", response.Continuation)
	}
}

func requireLegacyRunnerRetry(t *testing.T, root string, store *taskdir.Store, td *taskdir.TaskDir, meta *task.MetaRecord) {
	t.Helper()
	currentRunner := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, currentRunner, "current managed runner")
	refreshed, allowUpgrade, err := refreshManagedRetryRunner(Arguments{}, Dependencies{RunnerExecutable: currentRunner}, root, store, td, meta)
	if err != nil || !allowUpgrade || refreshed.runnerOwnership != task.RunnerOwnershipManaged || !isStateRunnerPath(root, refreshed.Runner) {
		t.Fatalf("migrated legacy retry runner = %+v, allowUpgrade=%v, err=%v", refreshed, allowUpgrade, err)
	}
}

func TestLegacyManagedRetryRecordsOwnershipForContinuation(t *testing.T) {
	root := filepath.Join(canonicalAppTestTempDir(t), "state")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	oldRunner := filepath.Join(root, ".supervisor", stateRunnerName)
	writeRunnerFixture(t, oldRunner, "old managed runner")
	td, req, meta := newLegacyRunnerTask(t, store, oldRunner)
	defer closeAppTestTask(t, store, td)

	currentRunner := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, currentRunner, "current managed runner")
	refreshed, allowUpgrade, err := refreshManagedRetryRunner(Arguments{}, Dependencies{RunnerExecutable: currentRunner}, root, store, td, meta)
	if err != nil || !allowUpgrade || refreshed.runnerOwnership != task.RunnerOwnershipManaged || !currentManagedRunnerExecutable(root, refreshed.Runner) {
		t.Fatalf("legacy retry did not select the current managed runner: args=%+v allowUpgrade=%v err=%v", refreshed, allowUpgrade, err)
	}
	if recorded, recordErr := td.HasManagedRunnerMigration(); recordErr != nil || !recorded {
		t.Fatalf("retry did not persist managed ownership before runner replacement: recorded=%v err=%v", recorded, recordErr)
	}
	if err = os.Remove(oldRunner); err != nil {
		t.Fatal(err)
	}
	ownership, err := runnerOwnershipForTask(root, td, meta)
	if err != nil || ownership != task.RunnerOwnershipManaged {
		t.Fatalf("retry ownership after old installation removal = %q err=%v", ownership, err)
	}
	requireLegacyRunnerContinuation(t, root, req)
}

func TestLegacyInspectionRetryKeepsOriginalRunner(t *testing.T) {
	fixture := newAppInspectionProofFixtureWithOwnership(t, false, "")
	oldRunner := filepath.Join(fixture.store.Root, ".supervisor", stateRunnerName)
	writeRunnerFixture(t, oldRunner, "old managed inspection runner")
	meta := fixture.meta
	meta.RunnerExecutable = oldRunner
	td, err := fixture.store.CreateTask(fixture.req.TaskID, &fixture.req, []byte("inspection brief"), &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	currentRunner := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, currentRunner, "current managed inspection runner")

	refreshed, allowUpgrade, err := refreshManagedRetryRunner(Arguments{}, Dependencies{RunnerExecutable: currentRunner}, fixture.store.Root, fixture.store, td, &meta)
	if err != nil || allowUpgrade || refreshed.Runner != oldRunner || refreshed.runnerOwnership != "" {
		t.Fatalf("legacy inspection retry did not remain pinned: args=%+v allowUpgrade=%v err=%v", refreshed, allowUpgrade, err)
	}
	if recorded, recordErr := td.HasManagedRunnerMigration(); recordErr != nil || recorded {
		t.Fatalf("legacy inspection retry recorded ordinary runner ownership: recorded=%v err=%v", recorded, recordErr)
	}
}

func TestQueuedRunnerMigrationRecordsLegacyOwnership(t *testing.T) {
	root := filepath.Join(canonicalAppTestTempDir(t), "state")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(root, ".supervisor", stateRunnerName)
	writeRunnerFixture(t, runner, "managed state runner")
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
		RunnerExecutable: runner, Environment: []string{"HOME=" + canonicalAppTestTempDir(t)},
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

func TestAdmissionQueueRepairUsesManagedRunnerForCustomTask(t *testing.T) {
	custom := filepath.Join(canonicalAppTestTempDir(t), "custom-delegate-run")
	managed := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, custom, "custom runner")
	writeRunnerFixture(t, managed, "managed runner")

	got, err := admissionQueueRepairRunner(Arguments{Runner: custom}, Dependencies{RunnerExecutable: managed}, custom)
	if err != nil {
		t.Fatal(err)
	}
	if got != managed {
		t.Fatalf("queue repair runner = %q, want managed runner %q", got, managed)
	}

	got, err = admissionQueueRepairRunner(Arguments{Runner: custom}, Dependencies{RunnerExecutable: filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)}, custom)
	if err != nil || got != "" {
		t.Fatalf("missing managed queue repair runner = %q, err=%v, want skipped", got, err)
	}

	got, err = admissionQueueRepairRunner(Arguments{runnerOwnership: task.RunnerOwnershipManaged}, Dependencies{RunnerExecutable: custom}, managed)
	if err != nil || got != managed {
		t.Fatalf("managed task queue repair runner = %q, err=%v, want selected task runner %q", got, err, managed)
	}
}

func TestRefreshManagedRetryRunnerUsesCurrentStateRunner(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	source := filepath.Join(canonicalAppTestTempDir(t), stateRunnerName)
	writeRunnerFixture(t, source, "current managed runner")
	meta := &task.MetaRecord{
		RunnerExecutable: filepath.Join(canonicalAppTestTempDir(t), stateRunnerName),
		RunnerOwnership:  task.RunnerOwnershipManaged,
		SupervisorConfig: task.SupervisorRef{ConfigPath: pueue.PrivateConfigPath(root)},
	}

	got, allowUpgrade, err := refreshManagedRetryRunner(Arguments{}, Dependencies{RunnerExecutable: source}, root, nil, nil, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !allowUpgrade || got.runnerOwnership != task.RunnerOwnershipManaged || !isStateRunnerPath(root, got.Runner) {
		t.Fatalf("managed retry selection = %+v, allowUpgrade=%v", got, allowUpgrade)
	}
	if !currentManagedRunnerExecutable(root, got.Runner) {
		t.Fatalf("refreshed state runner %q did not verify as current managed executable", got.Runner)
	}

	meta.SupervisorConfig.ConfigPath = filepath.Join(canonicalAppTestTempDir(t), "external-pueue.yml")
	got, allowUpgrade, err = refreshManagedRetryRunner(Arguments{}, Dependencies{RunnerExecutable: source}, root, nil, nil, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !allowUpgrade || got.runnerOwnership != task.RunnerOwnershipManaged || !isStateRunnerPath(root, got.Runner) {
		t.Fatalf("external-supervisor retry selection = %+v, allowUpgrade=%v", got, allowUpgrade)
	}
	if !currentManagedRunnerExecutable(root, got.Runner) {
		t.Fatalf("external-supervisor retry runner %q did not verify as current managed executable", got.Runner)
	}

	custom := filepath.Join(canonicalAppTestTempDir(t), "custom-runner")
	writeRunnerFixture(t, custom, "custom runner")
	meta.RunnerExecutable = custom
	meta.RunnerOwnership = task.RunnerOwnershipCustom
	got, allowUpgrade, err = refreshManagedRetryRunner(Arguments{}, Dependencies{RunnerExecutable: source}, root, nil, nil, meta)
	if err != nil || allowUpgrade || got.Runner != "" {
		t.Fatalf("custom retry was rewritten: args=%+v allowUpgrade=%v err=%v", got, allowUpgrade, err)
	}
}

func TestManagedContinuationKeepsRunnerOwnership(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	managedRunner := filepath.Join(canonicalAppTestTempDir(t), "old-install", stateRunnerName)
	writeRunnerFixture(t, managedRunner, "old managed runner")
	meta := &task.MetaRecord{
		Environment:      []string{"HOME=/task"},
		RunnerExecutable: managedRunner,
		RunnerOwnership:  task.RunnerOwnershipManaged,
		SupervisorConfig: task.SupervisorRef{ConfigPath: filepath.Join(canonicalAppTestTempDir(t), "pueue.yml")},
	}
	arguments := Arguments{Runner: managedRunner}
	if err := validateContinuationMetadata(root, arguments, meta); err != nil {
		t.Fatal(err)
	}
	got, err := buildContinuationArguments(arguments, root, &task.TaskRecord{TaskID: "continuation"}, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner != "" || requestedRunnerOwnership(got) != task.RunnerOwnershipManaged {
		t.Fatalf("managed continuation lost runner ownership: args=%+v", got)
	}
}

func TestManagedRunnerBuildInfoUsesExecutablePath(t *testing.T) {
	if !managedRunnerBuildInfo(&debug.BuildInfo{Path: managedRunnerMain}) {
		t.Fatal("official delegate-run executable was not recognized")
	}
	if managedRunnerBuildInfo(&debug.BuildInfo{
		Path: "github.com/hishamkaram/delegation-layer",
		Main: debug.Module{Path: managedRunnerMain},
	}) {
		t.Fatal("module path was incorrectly treated as the executable package path")
	}
}

func TestInstallStateSupervisorPairPersistsCurrentExecutables(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	base := filepath.Join(root, ".supervisor")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLegacySupervisorPairFixture(t, base)
	client := filepath.Join(canonicalAppTestTempDir(t), "pueue")
	daemon := filepath.Join(canonicalAppTestTempDir(t), "pueued")
	writeRunnerFixture(t, client, "client v1")
	writeRunnerFixture(t, daemon, "daemon v1")
	firstClient, firstDaemon := installAndResolveSupervisorPairFixture(t, root, base, client, daemon, "client v1", "daemon v1")
	resolvedClient, resolvedDaemon, err := resolveBundledExecutables(Dependencies{InitialSupervisorExecutable: filepath.Join(base, "delegate-run")})
	if err != nil || resolvedClient != firstClient || resolvedDaemon != firstDaemon {
		t.Fatalf("state runner did not resolve the published pair: client=%q daemon=%q err=%v", resolvedClient, resolvedDaemon, err)
	}
	requireSupervisorPairRecordFixture(t, base)
	writeRunnerFixture(t, client, "client v2")
	writeRunnerFixture(t, daemon, "daemon v2")
	secondClient, secondDaemon := installAndResolveSupervisorPairFixture(t, root, base, client, daemon, "client v2", "daemon v2")
	requireSupervisorPairGenerationChanged(t, firstClient, firstDaemon, secondClient, secondDaemon)
	requireRunnerFixture(t, firstClient, "client v1")
	requireRunnerFixture(t, firstDaemon, "daemon v1")
	requireLegacySupervisorPairFixture(t, base)
}

func TestInstallStateSupervisorPairDoesNotWaitForTaskMaintenanceLease(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	lock, err := taskdir.OpenLockFile(filepath.Join(root, ".maintenance.lock"), taskdir.LockLevelMaintenance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = lock.LockSH(context.Background()); err != nil {
		t.Fatal(err)
	}

	client := filepath.Join(canonicalAppTestTempDir(t), "pueue")
	daemon := filepath.Join(canonicalAppTestTempDir(t), "pueued")
	writeRunnerFixture(t, client, "client while a task is active")
	writeRunnerFixture(t, daemon, "daemon while a task is active")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err = installStateSupervisorPair(ctx, root, client, daemon); err != nil {
		t.Fatalf("supervisor pair installation waited on the shared task lease: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, ".supervisor", stateSupervisorPairRecordName)); err != nil {
		t.Fatalf("supervisor pair was not published while task lease remained held: %v", err)
	}
}

func writeLegacySupervisorPairFixture(t *testing.T, base string) {
	t.Helper()
	for _, legacy := range []struct {
		name string
		data string
	}{{name: "pueue", data: "legacy client"}, {name: "pueued", data: "legacy daemon"}} {
		if err := os.WriteFile(filepath.Join(base, legacy.name), []byte(legacy.data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func installAndResolveSupervisorPairFixture(t *testing.T, root, base, client, daemon, wantClient, wantDaemon string) (string, string) {
	t.Helper()
	if err := installStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
		t.Fatal(err)
	}
	return requireResolvedSupervisorPair(t, base, wantClient, wantDaemon)
}

func requireSupervisorPairRecordFixture(t *testing.T, base string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(base, stateSupervisorPairRecordName)); err != nil {
		t.Fatalf("state-root pair record was not published: %v", err)
	}
}

func requireSupervisorPairGenerationChanged(t *testing.T, firstClient, firstDaemon, secondClient, secondDaemon string) {
	t.Helper()
	if firstClient == secondClient || firstDaemon == secondDaemon {
		t.Fatal("supervisor pair upgrade reused the old generation")
	}
}

func requireLegacySupervisorPairFixture(t *testing.T, base string) {
	t.Helper()
	for _, legacy := range []struct {
		name string
		want string
	}{{name: "pueue", want: "legacy client"}, {name: "pueued", want: "legacy daemon"}} {
		got, err := os.ReadFile(filepath.Join(base, legacy.name))
		if err != nil || string(got) != legacy.want {
			t.Fatalf("legacy state-root %s pair was changed: %q err=%v", legacy.name, got, err)
		}
	}
}

func requireResolvedSupervisorPair(t *testing.T, base, wantClient, wantDaemon string) (string, string) {
	t.Helper()
	client, daemon, found, err := resolveStateSupervisorPair(base)
	if err != nil || !found {
		t.Fatalf("state-root pair resolution found=%t err=%v", found, err)
	}
	for _, executable := range []struct {
		path string
		want string
	}{{path: client, want: wantClient}, {path: daemon, want: wantDaemon}} {
		got, readErr := os.ReadFile(executable.path)
		if readErr != nil || !strings.HasSuffix(string(got), executable.want) {
			t.Fatalf("resolved state-root executable %q = %q, err=%v", executable.path, got, readErr)
		}
	}
	return client, daemon
}

func TestInstallStateSupervisorPairPublishesCoherentGenerations(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	clientA, daemonA := filepath.Join(canonicalAppTestTempDir(t), "pueue-a"), filepath.Join(canonicalAppTestTempDir(t), "pueued-a")
	clientB, daemonB := filepath.Join(canonicalAppTestTempDir(t), "pueue-b"), filepath.Join(canonicalAppTestTempDir(t), "pueued-b")
	writeRunnerFixture(t, clientA, strings.Repeat("client-a", 16*1024))
	writeRunnerFixture(t, daemonA, strings.Repeat("daemon-a", 16*1024))
	writeRunnerFixture(t, clientB, strings.Repeat("client-b", 16*1024))
	writeRunnerFixture(t, daemonB, strings.Repeat("daemon-b", 16*1024))
	if err := installStateSupervisorPair(context.Background(), root, clientA, daemonA); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, ".supervisor")
	start := make(chan struct{})
	done := make(chan struct{})
	errorsFound := make(chan error, 9)
	var writers sync.WaitGroup
	for index := 0; index < 8; index++ {
		writers.Add(1)
		go writeSupervisorPairGeneration(start, &writers, root, index, clientA, daemonA, clientB, daemonB, errorsFound)
	}
	var reader sync.WaitGroup
	reader.Add(1)
	go readSupervisorPairGenerations(start, done, &reader, base, errorsFound)
	close(start)
	writers.Wait()
	close(done)
	reader.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
}

func writeSupervisorPairGeneration(start <-chan struct{}, writers *sync.WaitGroup, root string, index int, clientA, daemonA, clientB, daemonB string, errorsFound chan<- error) {
	defer writers.Done()
	<-start
	client, daemon := clientA, daemonA
	if index%2 != 0 {
		client, daemon = clientB, daemonB
	}
	for iteration := 0; iteration < 3; iteration++ {
		if err := installStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
			errorsFound <- err
			return
		}
	}
}

func readSupervisorPairGenerations(start, done <-chan struct{}, reader *sync.WaitGroup, base string, errorsFound chan<- error) {
	defer reader.Done()
	<-start
	for {
		select {
		case <-done:
			return
		default:
		}
		if err := inspectSupervisorPairGeneration(base); err != nil {
			errorsFound <- err
			return
		}
	}
}

func inspectSupervisorPairGeneration(base string) error {
	client, daemon, found, err := resolveStateSupervisorPair(base)
	if err != nil {
		return fmt.Errorf("resolve pair during concurrent install: %w", err)
	}
	if !found {
		return errors.New("state supervisor pair was not published during concurrent install")
	}
	clientData, clientErr := os.ReadFile(client)
	daemonData, daemonErr := os.ReadFile(daemon)
	clientAFound := strings.HasSuffix(string(clientData), strings.Repeat("client-a", 16*1024))
	daemonAFound := strings.HasSuffix(string(daemonData), strings.Repeat("daemon-a", 16*1024))
	clientBFound := strings.HasSuffix(string(clientData), strings.Repeat("client-b", 16*1024))
	daemonBFound := strings.HasSuffix(string(daemonData), strings.Repeat("daemon-b", 16*1024))
	completePair := (clientAFound && daemonAFound) || (clientBFound && daemonBFound)
	if clientErr != nil || daemonErr != nil || !completePair {
		return fmt.Errorf("observed mixed or incomplete supervisor pair: client=%t/%t daemon=%t/%t", clientAFound, clientBFound, daemonAFound, daemonBFound)
	}
	return nil
}

func TestInstallStateSupervisorPairForRecoveryUsesCurrentBundle(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	bundle := canonicalAppTestTempDir(t)
	delegate := filepath.Join(bundle, "delegate")
	client := filepath.Join(bundle, "pueue")
	daemon := filepath.Join(bundle, "pueued")
	writeRunnerFixture(t, delegate, "delegate")
	writeRunnerFixture(t, client, "current client")
	writeRunnerFixture(t, daemon, "current daemon")

	if err := installStateSupervisorPairForRecovery(context.Background(), root, delegate, task.SupervisorRef{}); err != nil {
		t.Fatal(err)
	}
	requireResolvedSupervisorPair(t, filepath.Join(root, ".supervisor"), "current client", "current daemon")
}

func TestInstallStateSupervisorPairForRecoveryUsesOnlyVerifiedSavedFallback(t *testing.T) {
	t.Run("saved pair matches", func(t *testing.T) {
		root := canonicalAppTestTempDir(t)
		client, daemon := writeSavedSupervisorPair(t)
		binding := savedSupervisorPairRef(t, client, daemon)
		missingDelegate := filepath.Join(canonicalAppTestTempDir(t), "delegate")
		if err := installStateSupervisorPairForRecovery(context.Background(), root, missingDelegate, binding); err != nil {
			t.Fatal(err)
		}
		requireResolvedSupervisorPair(t, filepath.Join(root, ".supervisor"), "saved client", "saved daemon")
	})
	t.Run("saved pair digest differs", func(t *testing.T) {
		root := canonicalAppTestTempDir(t)
		client, daemon := writeSavedSupervisorPair(t)
		binding := savedSupervisorPairRef(t, client, daemon)
		binding.DaemonSHA256 = strings.Repeat("a", 64)
		missingDelegate := filepath.Join(canonicalAppTestTempDir(t), "delegate")
		if err := installStateSupervisorPairForRecovery(context.Background(), root, missingDelegate, binding); !errors.Is(err, pueue.ErrBinding) {
			t.Fatalf("mismatched saved pair error = %v, want binding error", err)
		}
		if _, err := os.Lstat(filepath.Join(root, ".supervisor", stateSupervisorPairRecordName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unverified saved pair was published: %v", err)
		}
	})
}

func TestPrivateRecoveryUsesStateRootPairWhenInstallPairsAreUnavailable(t *testing.T) {
	root := filepath.Join(canonicalAppTestTempDir(t), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	client, daemon := writeSavedSupervisorPair(t)
	saved := savedSupervisorPairRef(t, client, daemon)
	saved.ConfigPath = pueue.PrivateConfigPath(root)
	saved.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("resolved config"))
	saved.Endpoint = "unix:" + filepath.Join(root, ".supervisor", "runtime", "pueue.sock")
	saved.ConfigDigest = task.ComputeSHA256([]byte("config"))
	saved.ObservedVersion = "pueue 4.0.4"
	if err := installStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(client); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(daemon); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", canonicalAppTestTempDir(t))

	missingDelegate := filepath.Join(canonicalAppTestTempDir(t), "missing", "delegate")
	_, err := newSupervisorClient(context.Background(), root, saved, pueue.Options{}, true, missingDelegate)
	if err == nil || !strings.Contains(err.Error(), "saved private supervisor config is unavailable") {
		t.Fatalf("recovery did not reach the saved private config check using the state-root pair: %v", err)
	}
}

func TestPrivateRecoveryRejectsTamperedStateRootSupervisorPair(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, path, external string)
	}{
		{
			name: "changed executable bytes",
			tamper: func(t *testing.T, path, _ string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("replaced client"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlinked executable",
			tamper: func(t *testing.T, path, external string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := canonicalAppTestTempDir(t)
			clientSource, daemonSource := writeSavedSupervisorPair(t)
			if err := installStateSupervisorPair(context.Background(), root, clientSource, daemonSource); err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(root, ".supervisor")
			client, daemon, found, err := resolveStateSupervisorPair(base)
			if err != nil || !found {
				t.Fatalf("resolve installed state-root pair: found=%t err=%v", found, err)
			}
			saved := savedSupervisorPairRef(t, client, daemon)
			saved.ConfigPath = pueue.PrivateConfigPath(root)
			tc.tamper(t, client, clientSource)

			if _, err = newSupervisorClient(context.Background(), root, saved, pueue.Options{}, true, filepath.Join(canonicalAppTestTempDir(t), "delegate")); !errors.Is(err, pueue.ErrBinding) {
				t.Fatalf("tampered state-root supervisor pair error = %v, want binding error", err)
			}
		})
	}
}

func TestValidateSavedStateSupervisorPairKeepsExternalExecutablesUnchanged(t *testing.T) {
	client, daemon := writeSavedSupervisorPair(t)
	saved := savedSupervisorPairRef(t, client, daemon)
	if err := validateSavedStateSupervisorPair(canonicalAppTestTempDir(t), saved); err != nil {
		t.Fatalf("external saved supervisor pair was subjected to state-root validation: %v", err)
	}
}

func TestValidateSavedStateSupervisorPairAcceptsHistoricalGeneration(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	client, daemon := writeSavedSupervisorPair(t)
	if err := installStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
		t.Fatal(err)
	}
	historicalClient, historicalDaemon, found, err := resolveStateSupervisorPair(filepath.Join(root, ".supervisor"))
	if err != nil || !found {
		t.Fatalf("resolve historical state-root pair: found=%t err=%v", found, err)
	}
	saved := savedSupervisorPairRef(t, historicalClient, historicalDaemon)

	currentClient := filepath.Join(canonicalAppTestTempDir(t), "pueue")
	currentDaemon := filepath.Join(canonicalAppTestTempDir(t), "pueued")
	writeRunnerFixture(t, currentClient, "current client")
	writeRunnerFixture(t, currentDaemon, "current daemon")
	if err = installStateSupervisorPair(context.Background(), root, currentClient, currentDaemon); err != nil {
		t.Fatal(err)
	}
	if err = validateSavedStateSupervisorPair(root, saved); err != nil {
		t.Fatalf("intact historical state-root pair failed validation: %v", err)
	}
}

func TestInstallStateSupervisorPairForRecoveryUsesVerifiedStatePair(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	client, daemon := writeSavedSupervisorPair(t)
	binding := savedSupervisorPairRef(t, client, daemon)
	if err := installStateSupervisorPair(context.Background(), root, client, daemon); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, ".supervisor")
	stateClient, stateDaemon, found, err := resolveStateSupervisorPair(base)
	if err != nil || !found {
		t.Fatalf("resolve installed state-root pair: found=%t err=%v", found, err)
	}
	recordPath := filepath.Join(base, stateSupervisorPairRecordName)
	recordBefore, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(client); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(daemon); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", canonicalAppTestTempDir(t))
	missingDelegate := filepath.Join(canonicalAppTestTempDir(t), "missing", "delegate")
	if err = installStateSupervisorPairForRecovery(context.Background(), root, missingDelegate, binding); err != nil {
		t.Fatalf("recovery rejected the verified state-root pair: %v", err)
	}
	resolvedClient, resolvedDaemon, found, err := resolveStateSupervisorPair(base)
	if err != nil || !found || resolvedClient != stateClient || resolvedDaemon != stateDaemon {
		t.Fatalf("recovery changed the state-root pair: client=%q daemon=%q found=%t err=%v", resolvedClient, resolvedDaemon, found, err)
	}
	recordAfter, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recordAfter, recordBefore) {
		t.Fatal("recovery rewrote the verified state-root pair record")
	}
}

func writeSavedSupervisorPair(t *testing.T) (string, string) {
	t.Helper()
	client := filepath.Join(canonicalAppTestTempDir(t), "pueue")
	daemon := filepath.Join(canonicalAppTestTempDir(t), "pueued")
	writeRunnerFixture(t, client, "saved client")
	writeRunnerFixture(t, daemon, "saved daemon")
	return client, daemon
}

func savedSupervisorPairRef(t *testing.T, client, daemon string) task.SupervisorRef {
	t.Helper()
	clientDigest, err := stateExecutableSourceDigest(client)
	if err != nil {
		t.Fatal(err)
	}
	daemonDigest, err := stateExecutableSourceDigest(daemon)
	if err != nil {
		t.Fatal(err)
	}
	return task.SupervisorRef{
		ClientExecutable: client, ClientSHA256: clientDigest,
		DaemonExecutable: daemon, DaemonSHA256: daemonDigest,
	}
}

func TestBuildContinuationArgumentsDoesNotGuessRemovedLegacyRunnerIsManaged(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	missing := filepath.Join(root, "removed-install", "delegate-run")
	meta := &task.MetaRecord{RunnerExecutable: missing}
	if _, err := buildContinuationArguments(Arguments{}, root, &task.TaskRecord{}, meta, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambiguous removed legacy runner error = %v, want not-exist", err)
	}
	if ownership := persistedRunnerOwnership(root, meta); ownership != task.RunnerOwnershipCustom {
		t.Fatalf("removed legacy runner ownership = %q, want fail-closed custom", ownership)
	}

	writeRunnerFixture(t, missing, "available runner")
	got, err := buildContinuationArguments(Arguments{}, root, &task.TaskRecord{}, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner != missing {
		t.Fatalf("continuation runner = %q, want saved runner %q", got.Runner, missing)
	}
	if requestedRunnerOwnership(got) != task.RunnerOwnershipCustom {
		t.Fatalf("available legacy custom runner ownership = %q, want custom", requestedRunnerOwnership(got))
	}
}

func TestBuildContinuationArgumentsFailsWhenCustomRunnerWasRemoved(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	missing := filepath.Join(root, "removed-custom-runner", "delegate-run")
	meta := &task.MetaRecord{RunnerExecutable: missing, RunnerOwnership: task.RunnerOwnershipCustom}
	if _, err := buildContinuationArguments(Arguments{}, root, &task.TaskRecord{}, meta, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed custom runner error = %v, want not-exist", err)
	}

	writeRunnerFixture(t, missing, "custom runner")
	got, err := buildContinuationArguments(Arguments{}, root, &task.TaskRecord{}, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner != missing {
		t.Fatalf("continuation runner = %q, want saved custom runner %q", got.Runner, missing)
	}
	if requestedRunnerOwnership(got) != task.RunnerOwnershipCustom {
		t.Fatalf("continuation ownership = %q, want custom", requestedRunnerOwnership(got))
	}
}

func TestBuildContinuationArgumentsRefreshesStateRootRunner(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	runner := filepath.Join(root, ".supervisor", stateRunnerName)
	writeRunnerFixture(t, runner, "old state runner")
	meta := &task.MetaRecord{RunnerExecutable: runner}
	got, err := buildContinuationArguments(Arguments{}, root, &task.TaskRecord{}, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner != "" {
		t.Fatalf("continuation pinned old state-root runner %q", got.Runner)
	}
	if requestedRunnerOwnership(got) != task.RunnerOwnershipManaged {
		t.Fatalf("state-root continuation ownership = %q, want managed", requestedRunnerOwnership(got))
	}
}

func TestBuildContinuationArgumentsRefreshesContentAddressedRunner(t *testing.T) {
	root := canonicalAppTestTempDir(t)
	source := filepath.Join(canonicalAppTestTempDir(t), "delegate-run")
	writeRunnerFixture(t, source, "saved runner")
	runner, err := installStateRunner(root, source)
	if err != nil {
		t.Fatal(err)
	}
	meta := &task.MetaRecord{RunnerExecutable: runner}
	got, err := buildContinuationArguments(Arguments{}, root, &task.TaskRecord{}, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner != "" {
		t.Fatalf("continuation pinned old content-addressed runner %q", got.Runner)
	}
	if requestedRunnerOwnership(got) != task.RunnerOwnershipManaged {
		t.Fatalf("content-addressed continuation ownership = %q, want managed", requestedRunnerOwnership(got))
	}
}

func writeRunnerFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
