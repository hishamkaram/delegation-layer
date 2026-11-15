package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestMaintenanceGenerationMappingRetainsHistoricalTaskAcrossTwoDrains(t *testing.T) {
	root, bindings := newPrivateSuccessorTestState(t)
	for index, step := range []struct {
		edges   [][2]string
		current string
	}{
		{[][2]string{{"A", "B"}}, "B"},
		{[][2]string{{"A", "B"}, {"B", "C"}}, "C"},
	} {
		if index > 0 {
			requireMaintenanceProbeTest(t, os.Remove(privateBootstrapRecordPath(root)))
		}
		seedPrivateSuccessorTestHistory(t, root, bindings, step.edges, step.current)
		pin, err := readControlRootPin(t.Context(), root)
		requireMaintenanceProbeTest(t, err)
		err = WithExistingBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
			if _, beginErr := BeginPrivateUpgradeWithLock(ctx, pin, UpgradeOperationRequired, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}}); beginErr != nil {
				return beginErr
			}
			matched, readErr := PrivateSupervisorMaintenanceGenerationMatchesWithLock(ctx, root, bindings["A"], bindings[step.current])
			if readErr != nil || !matched {
				t.Fatalf("historical task lost mapping during drain %d: %t %v", index+1, matched, readErr)
			}
			assertMaintenanceMappingPurposesUnchanged(t, ctx, root, bindings["A"])
			if matched, readErr = PrivateSupervisorMaintenanceGenerationMatchesWithLock(ctx, root, bindings["X"], bindings[step.current]); readErr == nil && matched {
				t.Fatal("unconnected generation admitted")
			}
			if matched, readErr = PrivateSupervisorMaintenanceGenerationMatchesWithLock(ctx, root, bindings["A"], bindings["X"]); readErr == nil && matched {
				t.Fatal("unpublished target admitted")
			}
			return nil
		})
		requireMaintenanceProbeTest(t, err)
	}
}

func TestMaintenanceProbeRequiresActualExistingEXAndMappingLease(t *testing.T) {
	root, bindings := newPrivateSuccessorTestState(t)
	seedPrivateSuccessorTestHistory(t, root, bindings, [][2]string{{"A", "B"}}, "B")
	if _, err := ProbeSavedPrivateMaintenanceWithLock(t.Context(), root, bindings["A"], Options{}, nil); err == nil {
		t.Fatal("probe accepted missing EX")
	}
	if _, err := PrivateSupervisorMaintenanceGenerationMatchesWithLock(t.Context(), root, bindings["A"], bindings["B"]); err == nil {
		t.Fatal("mapping accepted missing generation lease")
	}
	var escaped context.Context
	err := WithSupervisorLeaseContext(t.Context(), root, func(ctx context.Context) error {
		escaped = ctx
		matched, readErr := PrivateSupervisorMaintenanceGenerationMatchesWithLock(ctx, root, bindings["A"], bindings["B"])
		if readErr != nil || !matched {
			t.Fatalf("valid shared lease refused: %t %v", matched, readErr)
		}
		return nil
	})
	requireMaintenanceProbeTest(t, err)
	if _, err = PrivateSupervisorMaintenanceGenerationMatchesWithLock(escaped, root, bindings["A"], bindings["B"]); !errors.Is(err, ErrBinding) {
		t.Fatalf("escaped lease: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err = WithExistingBootstrapLockContext(t.Context(), missing, func(ctx context.Context) error {
		_, probeErr := ProbeSavedPrivateMaintenanceWithLock(ctx, missing, task.SupervisorRef{}, Options{}, nil)
		return probeErr
	}); err == nil {
		t.Fatal("missing root accepted")
	}
	if _, err = os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("probe provisioned root")
	}
}

func requireMaintenanceProbeTest(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertMaintenanceMappingPurposesUnchanged(t *testing.T, ctx context.Context, root string, saved task.SupervisorRef) {
	t.Helper()
	for _, purpose := range []privateGenerationPurpose{privateGenerationGeneral, privateGenerationTaskRead} {
		if _, found, resolveErr := resolvePrivateSupervisorSuccessorWithLock(ctx, root, saved, purpose); found || resolveErr == nil {
			t.Fatalf("generic/task read policy broadened: purpose=%v found=%t err=%v", purpose, found, resolveErr)
		}
	}
}

func TestMaintenanceHistoricalDifferentConfigDrainStopRefusesReusedNumericWrongLabel(t *testing.T) {
	fixture := newMaintenancePrivateFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	current, err := BindPrivate(ctx, fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}})
	supervisorLeaseTestSettleClient(t, current, err)
	requireMaintenanceProbeTest(t, err)
	t.Cleanup(func() { requireMaintenanceProbeTest(t, current.Close()) })
	saved := current.Binding()
	saved.ConfigDigest = task.ComputeSHA256([]byte("retired logical config"))
	saved.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("retired resolved config"))
	saved.Endpoint = "unix:" + filepath.Join(fixture.stateRoot, "retired.sock")
	if task.SameSupervisorIdentity(saved, current.Binding()) {
		t.Fatal("fixture did not change logical config identity")
	}
	writePrivateSuccessorTestRecord(t, privateMigrationHistoryPath(filepath.Dir(saved.ConfigPath)), privateMigrationHistory{SchemaVersion: task.SchemaVersion, Migrations: []privateMigrationRecord{{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(saved), Successor: privateDaemonIdentityForBinding(current.Binding()), LegacyPID: 101, LegacyStopped: true}}})
	target := maintenanceTaskInRoot(t, fixture.stateRoot, saved)
	submit := createSubmission(t, target, saved)
	id := int64(7)
	identity := identityFromRecord(submit)
	identity.NumericTaskID = &id
	observed := Observation{Matched: true, State: StateQueued, Identity: identity, Binding: saved, NumericTaskID: id}
	receipt, err := observed.Receipt()
	requireMaintenanceProbeTest(t, err)
	requireMaintenanceProbeTest(t, target.taskDir.RecordSupervisorReceipt(*receipt))
	other := strings.Repeat("9", 32)
	if other == submit.TaskID {
		other = strings.Repeat("8", 32)
	}
	writePrivateSuccessorTestBytes(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-status.json"), statusTestPayload(t, map[string]any{"7": statusTestJob(7, "delegate:"+submit.RootID+":"+other, map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}})}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}}))
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	writePrivateSuccessorTestBytes(t, filepath.Join(base, "test-record-kill"), nil)
	pin, err := readControlRootPin(ctx, fixture.stateRoot)
	requireMaintenanceProbeTest(t, err)
	var mapped *Client
	err = WithExistingBootstrapLockContext(ctx, fixture.stateRoot, func(locked context.Context) error {
		if _, beginErr := BeginPrivateUpgradeWithLock(locked, pin, UpgradeOperationRequired, current.options); beginErr != nil {
			return beginErr
		}
		var probeErr error
		mapped, probeErr = ProbeSavedPrivateMaintenanceWithLock(locked, fixture.stateRoot, saved, current.options, func(context.Context) (string, string, error) { return fixture.clientPath, fixture.daemonPath, nil })
		return probeErr
	})
	requireMaintenanceProbeTest(t, err)
	if mapped == nil {
		t.Fatal("historical Drain probe returned no task control handle")
	}
	t.Cleanup(func() { requireMaintenanceProbeTest(t, mapped.Close()) })
	if mapped.Binding() != saved || mapped.RuntimeBinding() != current.Binding() {
		t.Fatal("control did not preserve saved task and exact current generation")
	}
	stop, err := target.taskDir.PrepareStop(strings.Repeat("7", 32), "user", time.Time{})
	requireMaintenanceProbeTest(t, err)
	t.Cleanup(func() { requireMaintenanceProbeTest(t, stop.Release()) })
	result, err := mapped.Stop(ctx, stop)
	if !errors.Is(err, ErrUnknown) || result.Matched || result.Attempted {
		t.Fatalf("different-identity retired task mutated numeric collision: %+v %v", result, err)
	}
	if _, err = os.Stat(filepath.Join(base, "test-kill-task-id")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove/kill side effect recorded: %v", err)
	}
}

func maintenanceTaskInRoot(t *testing.T, root string, saved task.SupervisorRef) *taskFixture {
	t.Helper()
	source := newTaskFixture(t, saved)
	request, meta, err := source.taskDir.PreparedRecords()
	requireMaintenanceProbeTest(t, err)
	brief, err := source.taskDir.ReadBrief()
	requireMaintenanceProbeTest(t, err)
	store, err := taskdir.InitStore(root)
	requireMaintenanceProbeTest(t, err)
	t.Cleanup(func() { requireMaintenanceProbeTest(t, store.Close()) })
	request.RootID = store.RootID
	meta.RootID = store.RootID
	meta.SpecSHA256 = ""
	td, err := store.CreateTask(request.TaskID, request, brief, meta)
	requireMaintenanceProbeTest(t, err)
	t.Cleanup(func() { requireMaintenanceProbeTest(t, td.Close()) })
	return &taskFixture{store: store, taskDir: td}
}

func newMaintenancePrivateFixture(t *testing.T) privateSupervisorTestFixture {
	t.Helper()
	base := canonicalTemp(t)
	store, err := taskdir.InitStore(filepath.Join(base, "state"))
	requireMaintenanceProbeTest(t, err)
	t.Cleanup(func() { requireMaintenanceProbeTest(t, store.Close()) })
	fixture := privateSupervisorTestFixture{stateRoot: store.Root, clientPath: filepath.Join(base, "pueue"), daemonPath: filepath.Join(base, "pueued")}
	writeExecutable(t, fixture.clientPath, privateClientFixture)
	writeExecutable(t, fixture.daemonPath, privateDaemonFixture)
	return fixture
}
