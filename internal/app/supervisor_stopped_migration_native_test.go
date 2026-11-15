package app

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This component invokes the actual native migration callback before its wire
// projection. Preflight birth is represented only by its relevant effect: no
// initial Ensure runs after the real daemon is stopped. The fixture guard is
// not a production resident birth or managed-image eligibility proof.
func TestStoppedCurrentNativeMigrationWithCompletedUpgradeAndStaleInspectionHead(t *testing.T) {
	clientPath, daemonPath := requireRuntimeRestartCLINativeContext(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	owner, store, current, options := stoppedMigrationNativeFixture(t, ctx, clientPath, daemonPath)
	before, err := pueue.ObservePrivateExecutionTarget(ctx, store.Root, options)
	if err != nil || before.State() != pueue.ExecutionTargetRunning {
		t.Fatalf("current fixture daemon was not natively verified: state=%v err=%v", before.State(), err)
	}
	predecessor := seedStoppedMigrationUpgradeHistory(t, ctx, store, current.Binding(), before)
	writeSupervisorGenerationTestGroup(t, store, predecessor)
	stopCurrentMigrationNativeDaemon(t, ctx, owner, before)
	base := filepath.Dir(current.Binding().ConfigPath)
	witness := supervisorfixture.CapturePairFaultWitness(t, base)
	probe := stoppedMigrationInvocation{owner: owner, store: store, current: current, options: options, lookup: installation.ExecutableLookup{InitialExecutable: clientPath, Environment: []string{}, AcceptedBootstrapPair: installation.BootstrapPair{Client: clientPath, Daemon: daemonPath}}}
	checkpoint := stoppedMigrationCheckpoint{legacy: predecessor, published: current.Binding(), history: string(readSupervisorProbeTestFile(t, filepath.Join(base, "migration-history.json"))), starts: len(owner.daemonWaits())}
	report, disposition, migrationErr := probe.migrate(t, ctx)
	if migrationErr != nil {
		logStoppedMigrationErrorGraph(t, migrationErr)
		logStoppedMigrationEffects(t, store, base, witness, checkpoint.starts, owner)
		t.Fatalf("stopped current native migration callback failed before wire: report=%+v disposition=%+v", report, disposition)
	}
	if report.State != infrastructure.ManualMigrationMigrated || disposition.Kind != infrastructure.DispositionStarted {
		t.Fatalf("native migration callback did not complete its ordinary restart: report=%+v disposition=%+v", report, disposition)
	}
	winner := checkpoint.verify(t, ctx, probe)
	if winner.PID() == before.PID() && winner.StartToken() == before.StartToken() {
		t.Fatal("stopped migration reused the predecessor native incarnation")
	}
	journal := stoppedMigrationInspectionRecords(t, store.Root)
	replayed, replayDisposition, replayErr := probe.migrate(t, ctx)
	if replayErr != nil || replayed.State != infrastructure.ManualMigrationMigrated || replayDisposition.Kind != infrastructure.DispositionWarmReuse {
		t.Fatalf("healthy repeated migration failed: report=%+v disposition=%+v err=%v", replayed, replayDisposition, replayErr)
	}
	repeated := checkpoint.verify(t, ctx, probe)
	if repeated.PID() != winner.PID() || repeated.StartToken() != winner.StartToken() || !maps.Equal(journal, stoppedMigrationInspectionRecords(t, store.Root)) {
		t.Fatal("healthy repeated migration created another native generation or inspection edge")
	}
}

type stoppedMigrationInvocation struct {
	owner   *componentSupervisorFixture
	store   *taskdir.Store
	current *pueue.Client
	options pueue.Options
	lookup  installation.ExecutableLookup
}

func (p stoppedMigrationInvocation) migrate(t *testing.T, ctx context.Context) (infrastructure.ManualMigrationReport, infrastructure.Disposition, error) {
	t.Helper()
	selected := infrastructure.LaunchContext{Protocol: infrastructure.RecordProtocolVersion, Root: p.store.Root, RootID: p.store.RootID, OS: runtime.GOOS, SupervisorExecutable: p.current.Binding().ClientExecutable, ControlEnvironment: pueue.DefaultEnvironment(), MaintenanceEnvironment: []string{}}
	digest, err := selected.Digest()
	if err != nil {
		t.Fatal(err)
	}
	observation, err := infrastructure.NewPreparationObservation(p.owner, infrastructure.LaunchContextReference{SHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	options := p.options
	options.DaemonStartGuard = observation
	starts := len(p.owner.daemonWaits())
	report, migrationErr := (nativeInfrastructureMigration{}).MigrateOwn(ctx, p.store, p.lookup, options, observation)
	p.owner.ownClient(nil, migrationErr)
	p.owner.ownDaemonClients(p.current, starts)
	return report, observation.Disposition(), migrationErr
}

type stoppedMigrationCheckpoint struct {
	legacy, published task.SupervisorRef
	history           string
	starts            int
}

func (c stoppedMigrationCheckpoint) verify(t *testing.T, ctx context.Context, p stoppedMigrationInvocation) pueue.ExecutionTarget {
	t.Helper()
	head, migrated, err := inspection.ReadGroupHead(ctx, p.store)
	if err != nil || !migrated || head.Supervisor != c.published {
		t.Fatalf("migration did not advance the exact inspection edge: migrated=%t head=%+v err=%v", migrated, head, err)
	}
	complete, err := inspection.GroupMigrationComplete(ctx, p.store, supervisor.InspectionGroup(p.store.RootID, c.legacy), supervisor.InspectionGroup(p.store.RootID, c.published))
	if err != nil || !complete {
		t.Fatalf("inspection edge is not canonically complete: complete=%t err=%v", complete, err)
	}
	if len(p.owner.daemonWaits()) != c.starts+1 || string(readSupervisorProbeTestFile(t, filepath.Join(p.store.Root, ".supervisor", "migration-history.json"))) != c.history {
		t.Fatal("ordinary recovery duplicated Start or changed completed private history")
	}
	published, found, err := pueue.ReadPrivateSupervisorBinding(p.store.Root)
	if err != nil || !found || published != c.published {
		t.Fatalf("ordinary migration changed its published private target: found=%t err=%v", found, err)
	}
	target, err := pueue.ObservePrivateExecutionTarget(ctx, p.store.Root, p.options)
	if err != nil || target.State() != pueue.ExecutionTargetRunning {
		t.Fatalf("migration did not verify the native daemon: state=%v err=%v", target.State(), err)
	}
	return target
}

func stoppedMigrationInspectionRecords(t *testing.T, root string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]string{}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "inspection-group") {
			continue
		}
		err = filepath.WalkDir(filepath.Join(root, entry.Name()), func(path string, item fs.DirEntry, walkErr error) error {
			if walkErr != nil || item.IsDir() {
				return walkErr
			}
			data, readErr := os.ReadFile(path)
			if readErr == nil {
				records[path] = string(data)
			}
			return readErr
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return records
}

func stoppedMigrationNativeFixture(t *testing.T, ctx context.Context, clientPath, daemonPath string) (*componentSupervisorFixture, *taskdir.Store, *pueue.Client, pueue.Options) {
	t.Helper()
	owner := newComponentSupervisorFixture(t, componentPersistentDaemon)
	store, err := taskdir.InitStore(owner.root)
	owner.ownStore(t, store)
	if err != nil {
		t.Fatal(err)
	}
	runner := canonicalAppTestExecutable(t)
	options := owner.setupOptions(nativeInfrastructureCallbacks().ApplyTo(pueue.Options{ObservationTimeout: 10 * time.Second, RuntimeClientExecutable: clientPath, ControlOwnerExecutable: runner, PrivateTerminalEventRunner: runner, Environment: pueue.DefaultEnvironment()}))
	current, err := owner.bindPrivate(ctx, clientPath, daemonPath, options)
	if err != nil {
		t.Fatal(err)
	}
	err = pueue.WithExistingBootstrapLockContext(ctx, store.Root, func(locked context.Context) error {
		return supervisor.EnsureOrdinarySchedulingWithBootstrapLock(locked, store.Root, current)
	})
	if err != nil {
		t.Fatal(err)
	}
	return owner, store, current, options
}

func stopCurrentMigrationNativeDaemon(t *testing.T, ctx context.Context, owner *componentSupervisorFixture, before pueue.ExecutionTarget) {
	t.Helper()
	waits := owner.daemonWaits()
	if len(waits) != 1 {
		t.Fatalf("fixture expected exactly one actual daemon Start/Wait: %d", len(waits))
	}
	if err := owner.stopDaemons(ctx, waits); err != nil {
		t.Fatal(err)
	}
	if err := awaitComponentDaemons(ctx, waits); err != nil {
		t.Fatal(err)
	}
	process, err := lifetime.NewProcessRef(before.PID(), before.StartToken())
	if err != nil {
		t.Fatal(err)
	}
	if err = lifetime.WaitProcessAbsent(ctx, process); err != nil {
		t.Fatal(err)
	}
}

// Completed historical records are immutable DATA fixtures, not observations
// that a fabricated predecessor ran or closed. Only the actual current daemon
// is started, observed, stopped and joined by this test's lifetime owner.
func seedStoppedMigrationUpgradeHistory(t *testing.T, ctx context.Context, store *taskdir.Store, current task.SupervisorRef, running pueue.ExecutionTarget) task.SupervisorRef {
	t.Helper()
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	legacy := current
	legacy.ClientExecutable = filepath.Join(filepath.Dir(store.Root), "removed-installation", "pueue")
	legacy.DaemonExecutable = filepath.Join(filepath.Dir(legacy.ClientExecutable), "pueued")
	legacy.ConfigDigest = task.ComputeSHA256([]byte("historical installed callback config fixture"))
	legacy.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("historical resolved callback config fixture"))
	if err = task.ValidateFreshSupervisorRef(legacy); err != nil {
		t.Fatal(err)
	}
	root := map[string]any{"path": pin.Root(), "id": pin.RootID(), "device": pin.Device(), "inode": pin.Inode()}
	record := func(predecessor task.SupervisorRef, op, mode string, oldPID, newPID int, oldStart, newStart string) map[string]any {
		return map[string]any{"schema_version": task.SchemaVersion, "legacy": stoppedMigrationDaemonIdentity(predecessor), "successor": stoppedMigrationDaemonIdentity(current), "legacy_pid": oldPID, "legacy_process_start": oldStart, "legacy_shutdown_requested": true, "legacy_shutdown_dispatched": true, "legacy_stopped": true, "upgrade_generation": map[string]any{"operation_id": op, "root": root, "predecessor": predecessor, "mode": mode, "target": current, "legacy_boot_id": boot, "accepted_start": true, "successor_boot_id": boot, "successor_pid": newPID, "successor_process_start": newStart, "completed": true}}
	}
	op1, op2 := "upgrade-v1:"+strings.Repeat("1", 32), "upgrade-v1:"+strings.Repeat("2", 32)
	history := map[string]any{"schema_version": task.SchemaVersion, "migrations": []any{record(legacy, op1, "reactive", 123, 124, "fixture-historical-legacy", "fixture-historical-successor"), record(current, op2, "operation_required", 124, running.PID(), "fixture-historical-successor", running.StartToken())}}
	data, err := task.MarshalCanonical(history)
	if err != nil {
		t.Fatal(err)
	}
	err = pueue.WithExistingBootstrapLockContext(ctx, store.Root, func(locked context.Context) error {
		if err := os.WriteFile(filepath.Join(store.Root, ".supervisor", "migration-history.json"), data, 0o600); err != nil {
			return err
		}
		active, found, resolveErr := pueue.ResolvePrivateSupervisorSuccessorWithLock(locked, store.Root, legacy)
		if resolveErr != nil || !found || active != current {
			return errors.Join(pueue.ErrBinding, resolveErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("completed historical fixture failed canonical validation: %v", err)
	}
	return legacy
}

func stoppedMigrationDaemonIdentity(binding task.SupervisorRef) map[string]any {
	return map[string]any{"executable": binding.DaemonExecutable, "executable_identity": binding.ExecutableIdentity, "config_path": binding.ConfigPath, "config_digest": binding.ConfigDigest, "resolved_config_sha256": binding.ResolvedConfigSHA256, "endpoint": binding.Endpoint}
}

func logStoppedMigrationEffects(t *testing.T, store *taskdir.Store, base string, before map[string]supervisorfixture.PairFaultFile, starts int, owner *componentSupervisorFixture) {
	t.Helper()
	after := supervisorfixture.CapturePairFaultWitness(t, base)
	for name, value := range before {
		if after[name] != value {
			t.Logf("migration changed canonical record %s", name)
		}
	}
	head, migrated, err := inspection.ReadGroupHead(t.Context(), store)
	t.Logf("post-callback group migrated=%t read_error=%v config_digest=%s; native_starts_before=%d after=%d", migrated, err, head.Supervisor.ConfigDigest, starts, len(owner.daemonWaits()))
}

func logStoppedMigrationErrorGraph(t *testing.T, cause error) {
	t.Helper()
	count := 0
	task.AnyErrorNode(cause, func(node error) bool {
		if count >= 32 {
			return true
		}
		t.Logf("direct migration error node[%d] type=%T cause=%.2048s", count, node, node.Error())
		count++
		return false
	})
}

func TestNativeInspectionMigrationKeepsCompletedCanonicalPairOverUnacceptedProposal(t *testing.T) {
	clientPath, daemonPath := requireRuntimeRestartCLINativeContext(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	owner, store, current, options := stoppedMigrationNativeFixture(t, ctx, clientPath, daemonPath)
	running, err := pueue.ObservePrivateExecutionTarget(ctx, store.Root, options)
	if err != nil || running.State() != pueue.ExecutionTargetRunning {
		t.Fatalf("canonical current daemon unavailable: state=%v err=%v", running.State(), err)
	}
	legacy := seedStoppedMigrationUpgradeHistory(t, ctx, store, current.Binding(), running)
	writeSupervisorGenerationTestGroup(t, store, legacy)
	proposalOwner := stoppedMigrationInvocation{owner: owner, store: store, current: current}
	proposalOwner.prepareOtherPair(t, ctx, legacy)
	lookup := installation.ExecutableLookup{InitialExecutable: clientPath, Environment: []string{}, AcceptedBootstrapPair: installation.BootstrapPair{Client: clientPath, Daemon: daemonPath}}
	var selected *pueue.Client
	err = pueue.WithBootstrapLockContext(ctx, store.Root, func(locked context.Context) error {
		var prepareErr error
		selected, prepareErr = prepareInspectionMigrationSupervisorWithLock(locked, store, lookup, options)
		owner.ownClient(selected, prepareErr)
		return prepareErr
	})
	if err != nil || selected == nil {
		t.Fatalf("app migration selector did not prepare canonical D: %v", err)
	}
	if selected.Binding() != current.Binding() {
		t.Fatalf("app migration selector replaced completed canonical D with unaccepted C': selected=%+v", selected.Binding())
	}
	history := string(readSupervisorProbeTestFile(t, filepath.Join(store.Root, ".supervisor", "migration-history.json")))
	probe := stoppedMigrationInvocation{owner: owner, store: store, current: current, options: options, lookup: lookup}
	report, disposition, migrationErr := probe.migrate(t, ctx)
	if migrationErr != nil || report.State != infrastructure.ManualMigrationMigrated || disposition.Kind != infrastructure.DispositionWarmReuse {
		t.Fatalf("outer app coordinator did not complete canonical D: report=%+v disposition=%+v err=%v", report, disposition, migrationErr)
	}
	head, migrated, readErr := inspection.ReadGroupHead(ctx, store)
	if readErr != nil || !migrated || head.Supervisor != current.Binding() || len(owner.daemonWaits()) != 1 {
		t.Fatalf("outer app coordinator selected another edge/native generation: migrated=%t starts=%d err=%v", migrated, len(owner.daemonWaits()), readErr)
	}
	if history != string(readSupervisorProbeTestFile(t, filepath.Join(store.Root, ".supervisor", "migration-history.json"))) {
		t.Fatal("outer app inspection completion duplicated private migration")
	}
}

func (p stoppedMigrationInvocation) prepareOtherPair(t *testing.T, ctx context.Context, legacy task.SupervisorRef) {
	t.Helper()
	proposal := p.current.Binding()
	proposal.ClientExecutable = filepath.Join(p.owner.base, "unaccepted-pair", "pueue")
	proposal.DaemonExecutable = filepath.Join(filepath.Dir(proposal.ClientExecutable), "pueued")
	// Independent finite fixture programs supply another installed proposal;
	// no installed native executable is copied, read or hashed.
	supervisorfixture.WriteExecutable(t, proposal.ClientExecutable, "#!/bin/sh\nprintf 'pueue 4.0.4\\n'\n")
	supervisorfixture.WriteExecutable(t, proposal.DaemonExecutable, "#!/bin/sh\nexit 0\n")
	if !task.SameSupervisorIdentity(proposal, p.current.Binding()) || proposal == p.current.Binding() {
		t.Fatal("proposal does not exercise same-logical different-pair selection")
	}
	if err := inspection.PrepareGroupMigration(ctx, p.store, supervisor.InspectionGroup(p.store.RootID, legacy), supervisor.InspectionGroup(p.store.RootID, proposal)); err != nil {
		t.Fatal(err)
	}
}
