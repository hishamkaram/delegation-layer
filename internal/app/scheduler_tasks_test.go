package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func schedulerObservationFixture(store *taskdir.Store) schedulerTasks {
	deps := Dependencies{PredicateRegistry: predicate.Default, Environment: []string{}}.normalized()
	deps.SupervisorOptions.WaitForPendingClose = true
	return schedulerTasks{root: store.Root, rootID: store.RootID, dependencies: deps}
}

func TestSchedulerObservationDistinguishesWholeAbsenceFromPartialTask(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	tasks := schedulerObservationFixture(store)
	id := strings.Repeat("e", 32)
	state, err := tasks.Observe(id)
	if err != nil || state.Release == nil || state.Release.Kind != contract.ReleaseNotCreated || state.Release.Ref != "task_absent" {
		t.Fatalf("whole absence was not proven: %+v, %v", state, err)
	}
	if mkdirErr := os.Mkdir(filepath.Join(store.Root, "tasks", id), 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	state, err = tasks.Observe(id)
	if err == nil || state.Release != nil {
		t.Fatalf("partial task was released: %+v, %v", state, err)
	}
	if !state.Observation.ObservedAt.IsZero() {
		t.Fatal("failed evidence read may overwrite the last validated observation")
	}
}

func TestSchedulerObservationFencesPreparedNeverStartedTask(t *testing.T) {
	store, td, request := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	tasks := schedulerObservationFixture(store)
	state, err := tasks.Observe(request.TaskID)
	if err != nil || state.Release == nil || state.Release.Kind != contract.ReleaseClosed {
		t.Fatalf("prepared unsubmitted task did not close admission: %+v, %v", state, err)
	}
	if validationErr := contract.ValidateRelease(*state.Release); validationErr != nil {
		t.Fatal(validationErr)
	}
	refused, err := td.TaskLaunchRefusedContext(t.Context())
	if err != nil || !refused {
		t.Fatalf("later launch is not durably refused: %t, %v", refused, err)
	}
	if _, proofErr := guardian.ReadClosureProof(t.Context(), td); !errors.Is(proofErr, guardian.ErrProofPending) {
		t.Fatalf("never-started refusal fabricated native closure: %v", proofErr)
	}
	again, err := tasks.Observe(request.TaskID)
	if err != nil || again.Release == nil || again.Release.Ref != state.Release.Ref {
		t.Fatalf("refusal reference changed: %+v, %v", again, err)
	}
}

func TestSchedulerObservationDoesNotReleaseForPublicationAlone(t *testing.T) {
	store, td, request := newAppTestTask(t, true)
	defer closeAppTestTask(t, store, td)
	if _, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef()); collectErr != nil || cleanupErr != nil {
		t.Fatal(errors.Join(collectErr, cleanupErr))
	}
	state, err := schedulerObservationFixture(store).Observe(request.TaskID)
	if err != nil || state.Release != nil || state.Observation.Publication != task.PublicationCommitted {
		t.Fatalf("publication was confused with native closure: %+v, %v", state, err)
	}
	data, err := os.ReadFile(filepath.Join(td.Dir, "outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Observation.OutcomeSHA256 != task.ComputeSHA256(data) {
		t.Fatal("observation does not reference the actual validated outcome file")
	}
}

func TestSchedulerObservationRejectsChangedRootIdentity(t *testing.T) {
	store, td, request := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	tasks := schedulerObservationFixture(store)
	tasks.rootID = strings.Repeat("0", 32)
	state, err := tasks.Observe(request.TaskID)
	if !errors.Is(err, task.ErrIdentityMismatch) || state.Release != nil {
		t.Fatalf("scheduler accepted another state root: %+v, %v", state, err)
	}
}
