package supervisor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func runtimeTestBinding(store *taskdir.Store) task.SupervisorRef {
	return testsupport.ContinuationSupervisorRef(store.Root, task.ComputeSHA256([]byte("runtime-quiescence-fixture")), "unix:"+filepath.Join(store.Root, "runtime.sock"))
}

func TestRuntimeQuiescenceFindsRowlessTaskAndRetainsLiveOwner(t *testing.T) {
	store, td, _ := lifecyclefixture.NewAppTestTask(t, false)
	lifecyclefixture.PrepareAppBudgetRecords(t, td)
	recordMigrationClosure(t, td, task.LifetimeTerminationCompleted, true)
	binding := runtimeTestBinding(store)
	before := testsupport.SnapshotStore(t, store.Root)
	if err := supervisor.CheckRuntimeQuiescence(t.Context(), store, binding, nil); err != nil {
		t.Fatalf("canonically closed rowless task blocked restart: %v", err)
	}
	if after := testsupport.SnapshotStore(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("quiescence scan changed task history")
	}
	owner, err := td.TryAcquireLifetimeOwnerContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := owner.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = supervisor.CheckRuntimeQuiescence(t.Context(), store, binding, nil); !errors.Is(err, supervisor.ErrRuntimeQuiescenceBusy) {
		t.Fatalf("live task owner returned %v", err)
	}
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
	if err = supervisor.CheckRuntimeQuiescence(t.Context(), store, binding, nil); err != nil {
		t.Fatalf("released task owner stayed busy: %v", err)
	}
}

func TestRuntimeQuiescenceDoesNotUsePublicationAsClosure(t *testing.T) {
	store, td, _ := lifecyclefixture.NewAppTestTask(t, true)
	outcome, cleanupErr, commitErr := td.PublishCandidateForTest("committed", "result.txt", []byte("answer\n"), task.FixturePredicateRef())
	if err := errors.Join(cleanupErr, commitErr); err != nil || outcome == nil {
		t.Fatalf("publication fixture = (%+v, %v)", outcome, err)
	}
	err := supervisor.CheckRuntimeQuiescence(t.Context(), store, runtimeTestBinding(store), nil)
	if !errors.Is(err, supervisor.ErrRuntimeQuiescenceBusy) {
		t.Fatalf("terminal publication without closure returned %v", err)
	}
}

func TestFencedRuntimePreparationIsNeitherAdmissionNorClosure(t *testing.T) {
	store, td, _ := lifecyclefixture.NewAppTestTask(t, false)
	binding := runtimeTestBinding(store)
	before := testsupport.SnapshotStore(t, store.Root)
	if err := supervisor.CheckRuntimeQuiescence(t.Context(), store, binding, nil); !errors.Is(err, supervisor.ErrRuntimeQuiescenceBusy) {
		t.Fatalf("initial prepared task returned %v", err)
	}
	if err := supervisor.CheckFencedRuntimeQuiescence(t.Context(), store, binding, nil); err != nil {
		t.Fatalf("fenced preparation-only task returned %v", err)
	}
	if after := testsupport.SnapshotStore(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("fenced observation fabricated task evidence or lock inodes")
	}
	observation, err := store.ReadTaskExecutionClosureContext(t.Context(), td.TaskID)
	if err != nil || observation.Proof != nil {
		t.Fatalf("preparation was promoted to closure = (%+v, %v)", observation, err)
	}
}

func TestRuntimeQuiescenceRefusesPartialCandidatesWithoutCreatingLocks(t *testing.T) {
	for _, family := range []string{"tasks", "inspections", "workspace-operations"} {
		t.Run(family, func(t *testing.T) {
			store := testsupport.NewInspectionStore(t)
			path := filepath.Join(store.Root, family, strings.Repeat("8", 32))
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			before := testsupport.SnapshotStore(t, store.Root)
			for _, check := range []func(context.Context, *taskdir.Store, task.SupervisorRef, []pueue.Job) error{supervisor.CheckRuntimeQuiescence, supervisor.CheckFencedRuntimeQuiescence} {
				if err := check(t.Context(), store, runtimeTestBinding(store), nil); err == nil {
					t.Fatal("partial rowless operation was ignored")
				}
			}
			if after := testsupport.SnapshotStore(t, store.Root); !reflect.DeepEqual(before, after) {
				t.Fatal("partial observation repaired or created storage")
			}
		})
	}
}

func TestRuntimeQuiescenceQueueRowsCannotHideActiveOrAmbiguousWork(t *testing.T) {
	store := testsupport.NewInspectionStore(t)
	binding := runtimeTestBinding(store)
	for _, state := range []pueue.State{pueue.StateUnknown, pueue.StateQueued, pueue.StateRunning, pueue.StateStashed} {
		t.Run(string(state), func(t *testing.T) {
			err := supervisor.CheckRuntimeQuiescence(t.Context(), store, binding, []pueue.Job{{ID: 1, State: state}})
			if !errors.Is(err, supervisor.ErrRuntimeQuiescenceBusy) {
				t.Fatalf("unmanaged %s row returned %v", state, err)
			}
		})
	}
	label := "delegate:" + store.RootID + ":" + strings.Repeat("a", 32)
	for _, jobs := range [][]pueue.Job{
		{{ID: 1, State: pueue.StateEnded, Label: &label}},
		{{ID: 1, State: pueue.StateEnded}, {ID: 1, State: pueue.StateEnded}},
		{{ID: 1, State: pueue.State("future-state")}},
	} {
		if err := supervisor.CheckRuntimeQuiescence(t.Context(), store, binding, jobs); err == nil || errors.Is(err, supervisor.ErrRuntimeQuiescenceBusy) {
			t.Fatalf("ambiguous queue rows returned %v", err)
		}
	}
	if err := supervisor.CheckRuntimeQuiescence(t.Context(), store, binding, []pueue.Job{{ID: 1, State: pueue.StateEnded}}); err != nil {
		t.Fatalf("unmanaged ended history row blocked empty runtime: %v", err)
	}
}

type runtimeAuxiliary interface {
	Close() error
	SealLifetimeUnlaunchedContext(context.Context) (*task.LifetimeUnlaunchedRecord, error)
	TryAcquireLifetimeOwner(context.Context) (*taskdir.LifetimeOwnerLease, error)
}

func runtimeAuxiliaryFixture(t *testing.T, family string, deadline time.Time) (*taskdir.Store, runtimeAuxiliary) {
	t.Helper()
	store := testsupport.NewInspectionStore(t)
	binding := runtimeTestBinding(store)
	var operation runtimeAuxiliary
	var err error
	switch family {
	case "inspection":
		request := testsupport.NewInspectionRequest(t, store)
		digest := task.ComputeSHA256([]byte("inspection-definition"))
		operation, err = inspection.OpenOperation(store, request, inspection.Binding{
			DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
			HelperExecutable: "/fixture/helper", HelperSHA256: digest,
			WorkerExecutable: "/fixture/worker", WorkerSHA256: digest,
			Supervisor: binding, RunnerOwnership: task.RunnerOwnershipManaged, WorkerExitProof: inspection.WorkerExitProofGuardianWaitV1,
		}, deadline.Add(-inspection.AdmissionTimeout))
	case "workspace":
		operation, err = workspace.OpenOperation(t.Context(), store, workspace.OperationRequest{
			SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: strings.Repeat("b", 32),
			Kind: workspace.OperationCheckpoint, Subject: workspace.Subject{Kind: workspace.SubjectTask, ID: strings.Repeat("c", 32), SHA256: task.ComputeSHA256([]byte("subject"))},
			Deadline: deadline.UTC().Format(time.RFC3339Nano), WorkerExecutable: "/fixture/worker", WorkerSHA256: task.ComputeSHA256([]byte("fixture-worker")), Supervisor: binding,
			Checkpoint: &workspace.CheckpointSelection{Kind: workspace.CheckpointOrdinary, Cwd: filepath.Join(store.Root, "project"), CwdIdentity: &workspace.DirectoryIdentity{Device: 1, Inode: 2}},
		})
	default:
		t.Fatalf("unknown auxiliary fixture %q", family)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store, operation
}

func TestRuntimeQuiescenceIndependentlyScansAuxiliaryFamilies(t *testing.T) {
	for _, family := range []string{"inspection", "workspace"} {
		t.Run(family, func(t *testing.T) {
			store, operation := runtimeAuxiliaryFixture(t, family, time.Now().Add(-time.Minute))
			if _, err := operation.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			binding := runtimeTestBinding(store)
			before := testsupport.SnapshotStore(t, store.Root)
			if err := supervisor.CheckRuntimeQuiescence(t.Context(), store, binding, nil); err != nil {
				t.Fatalf("rowless auxiliary closure blocked restart: %v", err)
			}
			if after := testsupport.SnapshotStore(t, store.Root); !reflect.DeepEqual(before, after) {
				t.Fatal("auxiliary observation changed lifetime history")
			}
			owner, err := operation.TryAcquireLifetimeOwner(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if closeErr := owner.Release(); closeErr != nil {
					t.Error(closeErr)
				}
			}()
			if err = supervisor.CheckRuntimeQuiescence(t.Context(), store, binding, nil); !errors.Is(err, supervisor.ErrRuntimeQuiescenceBusy) {
				t.Fatalf("owned auxiliary lifetime returned %v", err)
			}
		})
	}
}

func TestFencedRuntimeAuxiliaryPreparationDoesNotPublishClosure(t *testing.T) {
	for _, family := range []string{"inspection", "workspace"} {
		t.Run(family, func(t *testing.T) {
			store, _ := runtimeAuxiliaryFixture(t, family, time.Now().Add(time.Minute))
			binding := runtimeTestBinding(store)
			before := testsupport.SnapshotStore(t, store.Root)
			if err := supervisor.CheckRuntimeQuiescence(t.Context(), store, binding, nil); !errors.Is(err, supervisor.ErrRuntimeQuiescenceBusy) {
				t.Fatalf("initial auxiliary preparation returned %v", err)
			}
			if err := supervisor.CheckFencedRuntimeQuiescence(t.Context(), store, binding, nil); err != nil {
				t.Fatalf("fenced auxiliary preparation returned %v", err)
			}
			if after := testsupport.SnapshotStore(t, store.Root); !reflect.DeepEqual(before, after) {
				t.Fatal("fenced auxiliary observation created a closure or owner inode")
			}
		})
	}
}
