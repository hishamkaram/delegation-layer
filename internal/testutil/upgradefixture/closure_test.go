package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

// These are policy records, not native Start/Wait proof. Native acceptance
// produces the actual future task through its installed provider/guardian.
func recordClosedFixtureTask(t *testing.T, td *taskdir.TaskDir) *task.LifetimeWorkerWaitRecord {
	t.Helper()
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	deadline := time.Now().Add(time.Minute)
	_, err = td.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "fixture-scope", BootID: "fixture-boot", Deadline: deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = td.RecordLifetimeRegistrationContext(t.Context(), taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative,
		ScopeID: "fixture-scope", BootID: "fixture-boot", Deadline: deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := td.RecordLifetimeWorkerContext(t.Context(), taskdir.LifetimeWorkerFacts{PID: 4242, ProcessStart: "fixture-worker", BootID: "fixture-boot"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(worker)
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	waited, err := td.RecordLifetimeWorkerWaitContext(t.Context(), taskdir.LifetimeWorkerWaitFacts{
		WorkerSHA256: task.ComputeSHA256(data), ChildPID: worker.PID, Result: task.LifetimeWorkerWaitSuccess, ExitCode: &exitCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = td.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationCompleted); err != nil {
		t.Fatal(err)
	}
	return waited
}

func TestTaskClosureObserverExcludesUnrelatedInspectionAndRespectsOwner(t *testing.T) {
	store, td, request := lifecyclefixture.NewAppTestTask(t, false)
	waited := recordClosedFixtureTask(t, td)
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	binding := meta.SupervisorConfig
	binding.ConfigPath = pueue.PrivateConfigPath(store.Root)
	op, err := openFixtureInspection(t.Context(), store, request.CanonicalCwd, strings.Repeat("d", 32), "/fixture/helper", "/fixture/sleep", binding)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := op.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	observed, err := observeTaskClosure(t.Context(), store.Root, td.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(waited)
	if err != nil {
		t.Fatal(err)
	}
	if !observed.Closed || observed.Kind != taskdir.ConnectorClosureRegisteredNative || observed.ReferenceSHA256 != task.ComputeSHA256(data) {
		t.Fatalf("exact task canonical closure missing: %+v", observed)
	}
	if _, err = os.Stat(filepath.Join(store.Root, "inspections", op.Request().IdentityID(), "request.json")); err != nil {
		t.Fatal(err)
	}
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	observed, observeErr := observeTaskClosure(t.Context(), store.Root, td.TaskID)
	releaseErr := owner.Release()
	if err = errors.Join(observeErr, releaseErr); err != nil {
		t.Fatal(err)
	}
	if observed.Closed || observed.PendingReason != taskdir.ConnectorClosureOwnerBusy {
		t.Fatalf("observer bypassed exact task owner: %+v", observed)
	}
}

func TestTaskClosureObserverNeverInitializesMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	if _, err := observeTaskClosure(t.Context(), root, strings.Repeat("a", 32)); err == nil {
		t.Fatal("missing root accepted")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("observation created root: %v", err)
	}
}
