package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

type schedulerPublicationFault struct {
	taskdir.BaseFaultInjector
	phase   string
	target  string
	failure error
	calls   int
}

func (f *schedulerPublicationFault) OnLink(_, destination string) error {
	if destination == f.target && f.phase == "absent" {
		f.calls++
		return f.failure
	}
	return nil
}

func (f *schedulerPublicationFault) OnAfterLinkDirBarrier(destination string) error {
	if destination != f.target || f.phase == "absent" {
		return nil
	}
	f.calls++
	if f.phase == "unreadable" {
		return errors.Join(f.failure, os.WriteFile(destination, []byte("corrupt winner"), 0o600))
	}
	return f.failure
}

// The fault is installed on the production Store after real Status, at the
// adapter's next boundary. Collection, publication and the post-read all use
// that same Store; no synthetic collection result substitutes for its verdict.
func TestSchedulerClosureCollectionFaultRequiresValidatedReread(t *testing.T) {
	for _, phase := range []string{"absent", "matching", "unreadable"} {
		t.Run(phase, func(t *testing.T) {
			store, td, _ := newAppTestTask(t, true)
			t.Cleanup(func() { closeAppTestTask(t, store, td) })
			recordCustodyLifetime(t, td, nil, task.LifetimeTerminationCompleted)
			tasks := schedulerObservationFixture(store)
			observed, err := tasklifecycle.Status(store, td, tasklifecycle.SupervisorOptions{}, "", tasks.dependencies.Catalog)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected outcome publication failure")
			fault := &schedulerPublicationFault{phase: phase, target: filepath.Join(td.Dir, "outcome.json"), failure: injected}
			store.SetFaultInjector(fault)
			state, err := tasks.observeStatus(td, observed, nil)
			if !errors.Is(err, injected) || fault.calls != 1 {
				t.Fatalf("collection was lost or retried: calls=%d state=%+v err=%v", fault.calls, state, err)
			}
			assertSchedulerCollectionFaultState(t, td, phase, state.Observation, state.Release)
		})
	}
}

func assertSchedulerCollectionFaultState(t *testing.T, td *taskdir.TaskDir, phase string, observation contract.Observation, release *contract.Release) {
	t.Helper()
	if phase == "unreadable" {
		if release != nil || !observation.ObservedAt.IsZero() {
			t.Fatalf("failed post-read published partial facts: observation=%+v release=%+v", observation, release)
		}
		return
	}
	if release == nil || release.Kind != contract.ReleaseClosed || observation.FaultCode != "collection_failed" {
		t.Fatalf("collection diagnostic erased valid closure/observation: observation=%+v release=%+v", observation, release)
	}
	want := task.PublicationPending
	if phase == "matching" {
		want = task.PublicationCommitted
		ref, refErr := td.OutcomeReference()
		if refErr != nil || ref != observation.OutcomeSHA256 {
			t.Fatalf("durable winner not retained: %q %+v %v", ref, observation, refErr)
		}
	}
	if observation.Publication != want {
		t.Fatalf("publication=%v want %v", observation.Publication, want)
	}
}

func TestSchedulerClosureCollectionBusyRetainsValidatedRelease(t *testing.T) {
	store, td, _ := newAppTestTask(t, true)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	recordCustodyLifetime(t, td, nil, task.LifetimeTerminationCompleted)
	lock, err := taskdir.OpenLockFile(filepath.Join(td.Dir, ".run.lock"), taskdir.LockLevelRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = lock.LockEX(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, err := schedulerObservationFixture(store).Observe(td.TaskID)
	if err != nil || state.Release == nil || state.Observation.Publication != task.PublicationPending || state.Observation.FaultCode != "collection_pending" {
		t.Fatalf("busy collection lost independently valid closure: %+v %v", state, err)
	}
	if _, err = os.Stat(filepath.Join(td.Dir, "outcome.json")); !os.IsNotExist(err) {
		t.Fatalf("busy collection fabricated a winner: %v", err)
	}
}

func TestSchedulerObservationRetainsCancelledTaskOnJoinedLocalFault(t *testing.T) {
	store, td, tasks := schedulerSubmittedFixture(t)
	permit, err := td.PrepareStop("dddddddddddddddddddddddddddddddd", "user", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(permit.Consume(), permit.Release()); err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, filepath.Join(td.Dir, "stop", "dddddddddddddddddddddddddddddddd.request.json"), []byte("{}"))
	observed, statusErr := tasklifecycle.Status(store, td, observationSupervisorOptions(tasks.dependencies.supervisorControlScope(), tasks.dependencies.supervisorLookup()), "", tasks.dependencies.Catalog)
	if !observed.ControlObservation || !errors.Is(statusErr, pueue.ErrUnknown) || tasklifecycle.IsPureSupervisorUncertaintyError(statusErr) {
		t.Fatalf("fixture did not join its local fault to queue uncertainty: %+v %v", observed, statusErr)
	}
	state, err := tasks.Observe(td.TaskID)
	if err == nil || state.Release != nil || !state.Observation.ObservedAt.IsZero() {
		t.Fatalf("joined local fault was suppressed: %+v %v", state, err)
	}
}

func TestSchedulerObservationDoesNotFenceSubmittedTaskFromMissingQueue(t *testing.T) {
	_, td, tasks := schedulerSubmittedFixture(t)
	state, err := tasks.Observe(td.TaskID)
	if !tasklifecycle.IsPureSupervisorUncertaintyError(err) || state.Release != nil || !state.Observation.ObservedAt.IsZero() {
		t.Fatalf("missing queue row released or replaced valid prior facts: %+v %v", state, err)
	}
	refused, err := td.TaskLaunchRefusedContext(t.Context())
	if err != nil || refused {
		t.Fatalf("uncertainty created an automatic refusal: %t %v", refused, err)
	}
}

func TestSchedulerObservationSeparatesClosureFromPublication(t *testing.T) {
	for _, evidence := range []string{"open lifetime", "invalid closure", "closed interruption"} {
		t.Run(evidence, func(t *testing.T) {
			sealed := evidence != "closed interruption"
			store, td, _ := newAppTestTask(t, sealed)
			t.Cleanup(func() { closeAppTestTask(t, store, td) })
			cause := task.LifetimeTerminationCompleted
			if evidence == "open lifetime" {
				cause = custodyLifetimeOpen
			}
			recordCustodyLifetime(t, td, nil, cause)
			if evidence == "invalid closure" {
				writeAppTestFile(t, filepath.Join(td.Dir, "lifetime", "terminated.json"), []byte("{}"))
			}
			state, err := schedulerObservationFixture(store).Observe(td.TaskID)
			assertSchedulerClosurePublication(t, evidence, state.Observation, state.Release, err)
			if _, err = os.Lstat(filepath.Join(td.Dir, "outcome.json")); !os.IsNotExist(err) {
				t.Fatalf("unexpected result publication: %v", err)
			}
		})
	}
}

func assertSchedulerClosurePublication(t *testing.T, evidence string, observation contract.Observation, release *contract.Release, err error) {
	t.Helper()
	switch evidence {
	case "invalid closure":
		if err == nil || release != nil || !observation.ObservedAt.IsZero() {
			t.Fatalf("invalid closure released or collected: observation=%+v release=%+v %v", observation, release, err)
		}
	case "open lifetime":
		if err != nil || release != nil || observation.Publication != task.PublicationPending {
			t.Fatalf("open lifetime was treated as closed: observation=%+v release=%+v %v", observation, release, err)
		}
	case "closed interruption":
		if err != nil || release == nil || observation.Publication != task.PublicationUnknown {
			t.Fatalf("interruption fabricated publication: observation=%+v release=%+v %v", observation, release, err)
		}
	}
}

func TestSchedulerClosureDoesNotCollectExistingWinnerAgain(t *testing.T) {
	store, td, _ := newAppTestTask(t, true)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	recordCustodyLifetime(t, td, nil, task.LifetimeTerminationCompleted)
	if _, cleanupErr, err := td.Collect(task.FixturePredicateRef()); errors.Join(cleanupErr, err) != nil {
		t.Fatal(errors.Join(cleanupErr, err))
	}
	tasks := schedulerObservationFixture(store)
	observed, err := tasklifecycle.Status(store, td, tasklifecycle.SupervisorOptions{}, "", tasks.dependencies.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	fault := &schedulerPublicationFault{phase: "matching", target: filepath.Join(td.Dir, "outcome.json"), failure: errors.New("must not recollect winner")}
	store.SetFaultInjector(fault)
	state, err := tasks.observeStatus(td, observed, nil)
	if err != nil || fault.calls != 0 || state.Release == nil || state.Observation.Publication != task.PublicationCommitted {
		t.Fatalf("existing winner recollected or lost: %+v calls=%d err=%v", state, fault.calls, err)
	}
}
