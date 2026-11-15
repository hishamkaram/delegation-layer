package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// These authored finite control roles model live rows, not native task/daemon
// lifetime. The real store and busy authorizer publish the recovery alias.
type upgradeAliasReplayFixture struct {
	stored        generationGuardFixture
	pin           taskdir.RootPin
	accepted      PrivateUpgradeObservation
	options       Options
	currentClient string
	currentDaemon string
	jobCounts     []int
	starts        atomic.Int32
	mutations     atomic.Int32
}

func newUpgradeAliasReplayFixture(t *testing.T, customDaemon bool) *upgradeAliasReplayFixture {
	t.Helper()
	f := &upgradeAliasReplayFixture{stored: newGenerationGuardFixture(t)}
	if customDaemon {
		custom := filepath.Join(filepath.Dir(f.stored.client.Binding().DaemonExecutable), "custom-recorded-daemon")
		if err := os.Rename(f.stored.client.Binding().DaemonExecutable, custom); err != nil {
			t.Fatal(err)
		}
		if err := attachPrivateDaemon(f.stored.client, custom); err != nil {
			t.Fatal(err)
		}
	}
	f.currentClient, f.currentDaemon = installPrivateSupervisorPair(t, filepath.Join(filepath.Dir(f.stored.root), "current-install"))
	f.options = f.stored.client.options
	f.options.RuntimeClientExecutable = f.currentClient
	f.options.ObservationTimeout = privateTestObservationTimeout
	f.options.DaemonStartGuard = daemonStartGuardFunc(func(context.Context, string, func() error) error {
		f.starts.Add(1)
		return ErrInfrastructureOwnerRequired
	})
	f.options.Observer = f.observeCommand
	f.options.PrivateMigrationAuthorizer = func(ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) error {
		return f.authorizeLiveRows(t, ctx, root, binding, jobs)
	}
	var err error
	f.pin, err = readControlRootPin(t.Context(), f.stored.root)
	if err != nil {
		t.Fatal(err)
	}
	err = WithExistingBootstrapLockContext(t.Context(), f.stored.root, func(ctx context.Context) error {
		binding := f.stored.client.Binding()
		if publishErr := publishPrivateSupervisorBindingIfAbsentContext(ctx, f.stored.root, binding, f.options.PrivateGenerationGuard); publishErr != nil {
			return publishErr
		}
		if identityErr := publishPrivateDaemonIdentityContext(ctx, f.stored.base, binding, f.options.PrivateGenerationGuard); identityErr != nil {
			return identityErr
		}
		f.accepted, err = BeginPrivateUpgradeWithLock(ctx, f.pin, UpgradeReactive, f.options)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	writePrivateSuccessorTestBytes(t, filepath.Join(f.stored.base, "ready"), nil)
	writeUpgradeAliasReplayRows(t, f.stored.base, 3)
	return f
}

func (f *upgradeAliasReplayFixture) observeCommand(event CommandEvent) {
	if event.Stage != "started" {
		return
	}
	for _, command := range []string{"shutdown", "kill", "remove", "clean", "add", "enqueue"} {
		if slices.Contains(event.Argv, command) {
			f.mutations.Add(1)
		}
	}
}

func (f *upgradeAliasReplayFixture) authorizeLiveRows(t *testing.T, ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) error {
	t.Helper()
	if root != f.stored.root || binding != f.stored.client.Binding() {
		t.Fatal("alias observation replaced the immutable predecessor")
	}
	if err := requireRuntimeRestartBootstrapEX(ctx, root); err != nil {
		return err
	}
	f.jobCounts = append(f.jobCounts, len(jobs))
	if len(jobs) != 0 {
		return ErrPrivateSupervisorMigrationBusy
	}
	return nil
}

func writeUpgradeAliasReplayRows(t *testing.T, base string, count int) {
	t.Helper()
	rows := make(map[string]any, count)
	for id := 1; id <= count; id++ {
		rows[strconv.Itoa(id)] = statusTestJob(int64(id), "active-"+strconv.Itoa(id), map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}})
	}
	data := statusTestPayload(t, rows, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	writePrivateSuccessorTestBytes(t, filepath.Join(base, "test-status.json"), data)
}

func (f *upgradeAliasReplayFixture) advance(t *testing.T) (PrivateUpgradeObservation, error) {
	t.Helper()
	var observed PrivateUpgradeObservation
	err := WithExistingBootstrapLockContext(t.Context(), f.stored.root, func(ctx context.Context) error {
		var err error
		observed, err = AdvancePrivateUpgradeDrainWithLock(ctx, f.pin, f.accepted.OperationID, f.options)
		return err
	})
	return observed, err
}

func (f *upgradeAliasReplayFixture) assertFirstBusyPublishesAlias(t *testing.T) {
	t.Helper()
	observed, err := f.advance(t)
	if !errors.Is(err, ErrPrivateSupervisorMigrationBusy) || observed != f.accepted || !slices.Equal(f.jobCounts, []int{3}) {
		t.Fatalf("first live drain did not retain its winner: observed=%+v rows=%v err=%v", observed, f.jobCounts, err)
	}
	alias, found, err := readValidatedPrivateRecoveryAlias(f.stored.root)
	if err != nil || !found || !alias.matches(f.stored.client.Binding()) || alias.record.Supervisor != f.stored.client.Binding() {
		t.Fatalf("first busy authorization did not publish the exact alias: found=%t err=%v", found, err)
	}
}

func (f *upgradeAliasReplayFixture) assertNoStartOrStop(t *testing.T) {
	t.Helper()
	if f.starts.Load() != 0 || f.mutations.Load() != 0 {
		t.Fatalf("drain observation gained mutation authority: starts=%d mutations=%d", f.starts.Load(), f.mutations.Load())
	}
	assertPrivateTestPathMissing(t, filepath.Join(f.stored.base, "daemon-started"))
	assertPrivateTestPathMissing(t, privateMigrationRecordPath(f.stored.base))
}

func TestUpgradeRepeatedBusyDrainReopensAliasAfterInstallationRemoval(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "removed-pair", true: "available-custom-recorded-daemon"}[custom], func(t *testing.T) {
			testUpgradeAliasReplayAfterRemoval(t, custom)
		})
	}
}

func TestUpgradeAliasDrainRejectsInvalidOrUnavailableDaemon(t *testing.T) {
	for _, kind := range []string{"invalid-recorded", "missing-selected"} {
		t.Run(kind, func(t *testing.T) {
			f := newUpgradeAliasReplayFixture(t, false)
			f.assertFirstBusyPublishesAlias(t)
			removeUpgradeAliasFixtureFile(t, f.stored.client.Binding().ClientExecutable)
			removeUpgradeAliasFixtureFile(t, f.stored.client.Binding().DaemonExecutable)
			if kind == "invalid-recorded" {
				makePrivateTestDirectory(t, f.stored.client.Binding().DaemonExecutable)
			} else {
				removeUpgradeAliasFixtureFile(t, f.currentDaemon)
			}
			before := snapshotPrivateSuccessorTestState(t, f.stored.root)
			observed, err := f.advance(t)
			if !errors.Is(err, ErrBinding) || !errors.Is(err, ErrConfiguration) || observed != f.accepted || !slices.Equal(f.jobCounts, []int{3}) {
				t.Fatalf("invalid runtime daemon crossed live-row authorization: observed=%+v rows=%v err=%v", observed, f.jobCounts, err)
			}
			if (kind == "missing-selected") != errors.Is(err, os.ErrNotExist) {
				t.Fatalf("daemon selection lost missing/invalid cause: %v", err)
			}
			assertPrivateSuccessorTestStateUnchanged(t, f.stored.root, before)
			f.assertNoStartOrStop(t)
		})
	}
}

func testUpgradeAliasReplayAfterRemoval(t *testing.T, custom bool) {
	t.Helper()
	f := newUpgradeAliasReplayFixture(t, custom)
	f.assertFirstBusyPublishesAlias(t)
	removeUpgradeAliasFixtureFile(t, f.stored.client.Binding().ClientExecutable)
	wantDaemon := f.currentDaemon
	if custom {
		// Ordinary embedders may retain a custom recorded daemon while using a
		// different client. They need no invented sibling role for that read.
		removeUpgradeAliasFixtureFile(t, f.currentDaemon)
		wantDaemon = f.stored.client.Binding().DaemonExecutable
	} else {
		removeUpgradeAliasFixtureFile(t, f.stored.client.Binding().DaemonExecutable)
	}
	writeUpgradeAliasReplayRows(t, f.stored.base, 1)
	before := snapshotPrivateSuccessorTestState(t, f.stored.root)
	observed, err := f.advance(t)
	if !errors.Is(err, ErrPrivateSupervisorMigrationBusy) || observed != f.accepted || !slices.Equal(f.jobCounts, []int{3, 1}) {
		t.Fatalf("same OP could not observe the retained live alias: observed=%+v rows=%v err=%v", observed, f.jobCounts, err)
	}
	assertPrivateSuccessorTestStateUnchanged(t, f.stored.root, before)
	assertUpgradeAliasRuntimePair(t, f, wantDaemon)
	paths := []string{privateSupervisorBindingPath(f.stored.root), privateDaemonIdentityPath(f.stored.base), privateRecoveryRecordPath(f.stored.root), privateRecoveryConfigPath(f.stored.root)}
	provenance := snapshotPrivateTestFiles(t, paths)
	writeUpgradeAliasReplayRows(t, f.stored.base, 0)
	observed, err = f.advance(t)
	if err != nil || observed.OperationID != f.accepted.OperationID || observed.Phase != UpgradeAuthorized || observed.Predecessor != f.accepted.Predecessor || !slices.Equal(f.jobCounts, []int{3, 1, 0}) {
		t.Fatalf("closed rows could not resume the exact drain: observed=%+v rows=%v err=%v", observed, f.jobCounts, err)
	}
	for _, path := range paths {
		data, found := provenance[path]
		assertPrivateTestFileUnchanged(t, path, data, found)
	}
	f.assertNoStartOrStop(t)
}

func removeUpgradeAliasFixtureFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func assertUpgradeAliasRuntimePair(t *testing.T, f *upgradeAliasReplayFixture, daemon string) {
	t.Helper()
	err := WithExistingBootstrapLockContext(t.Context(), f.stored.root, func(ctx context.Context) error {
		drain, found, err := readPrivateUpgrade(f.pin, f.accepted.OperationID)
		if err != nil || !found {
			return errors.Join(ErrBinding, err)
		}
		client, err := preparePrivateUpgradePredecessor(ctx, f.pin, drain, f.options)
		if err != nil {
			return err
		}
		if client.Binding() != f.accepted.Predecessor || client.RuntimeBinding().ClientExecutable != f.currentClient || client.RuntimeBinding().DaemonExecutable != daemon || client.RuntimeBinding().ConfigPath != privateRecoveryConfigPath(f.stored.root) {
			t.Error("runtime alias pair replaced logical identity or ignored installed selection")
		}
		return client.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}
