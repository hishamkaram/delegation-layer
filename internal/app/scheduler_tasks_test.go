package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	schedulerruntime "github.com/hishamkaram/delegation-layer/internal/scheduler/runtime"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func schedulerObservationFixture(store *taskdir.Store) schedulerTasks {
	deps := Dependencies{PredicateRegistry: predicate.Default, Environment: []string{}}.normalized()
	deps.SupervisorOptions.WaitForPendingClose = true
	return schedulerTasks{root: store.Root, rootID: store.RootID, dependencies: deps, routing: deps.infrastructureRouting()}
}

func TestSchedulerObservationCleanupRetainsRoutingStore(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	other, err := taskdir.OpenStore(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := errors.Join(store.Close(), other.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	cleanup := &infrastructureRoutingCleanupError{resources: &infrastructureRoutingResources{target: store}, cause: os.ErrPermission}
	resultErr := errors.Join(errors.New("earlier independent cause"), cleanup)
	state := schedulerruntime.TaskState{Observation: contract.Observation{ObservedAt: time.Unix(1, 0).UTC()}}
	closeSchedulerObservation(&state, &resultErr, store)
	if _, pinErr := store.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("scheduler closed routed cleanup's borrowed Store: %v", pinErr)
	}
	if state.Observation.ObservedAt.IsZero() || !errors.Is(resultErr, os.ErrPermission) {
		t.Fatal("scheduler lost the observation or cleanup cause")
	}
	closeSchedulerObservation(&state, &resultErr, other)
	if _, pinErr := other.PinRootContext(t.Context()); !errors.Is(pinErr, os.ErrClosed) {
		t.Fatalf("scheduler retained an unrelated handle for the same root: %v", pinErr)
	}
	// No native actor is involved: this control represents a settled error view.
	cleanup.resources = nil
	closeSchedulerObservation(&state, &resultErr, store)
	if _, pinErr := store.PinRootContext(t.Context()); !errors.Is(pinErr, os.ErrClosed) {
		t.Fatalf("scheduler retained a settled cleanup view: %v", pinErr)
	}
}

func TestSchedulerObservationReleasesCancelledSubmittedNeverLaunchedTask(t *testing.T) {
	store, td, tasks := schedulerSubmittedFixture(t)
	id := strings.Repeat("d", 32)
	request, err := td.PrepareStop(id, "user", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(request.Consume(), request.Release()); err != nil {
		t.Fatal(err)
	}
	assertSchedulerMissingQueueUnlaunched(t, store, td, tasks)
	state, err := tasks.Observe(td.TaskID)
	if !tasklifecycle.IsPureSupervisorUncertaintyError(err) || state.Release == nil || state.Release.Kind != contract.ReleaseClosed {
		t.Fatalf("cancelled submitted task remained reserved: %+v, %v", state, err)
	}
	stop, err := td.ReadStopRequest(id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(stop)
	if err != nil || state.Release.Ref != task.ComputeSHA256(data) {
		t.Fatalf("release does not reference validated refusal: %+v, %v", state, err)
	}
	if state.Observation.Publication != task.PublicationUnknown || state.Observation.OutcomeSHA256 != "" {
		t.Fatalf("never-started task fabricated publication: %+v", state)
	}
	if _, err = guardian.ReadClosureProof(t.Context(), td); !errors.Is(err, guardian.ErrProofPending) {
		t.Fatalf("refusal fabricated native termination: %v", err)
	}
}

func assertSchedulerMissingQueueUnlaunched(t *testing.T, store *taskdir.Store, td *taskdir.TaskDir, tasks schedulerTasks) {
	t.Helper()
	observed, statusErr := tasklifecycle.Status(store, td, observationSupervisorOptions(tasks.dependencies.supervisorControlScope(), tasks.dependencies.supervisorLookup(), tasks.dependencies.infrastructureRouting()), "", tasks.dependencies.Catalog)
	if statusErr == nil || !tasklifecycle.IsPureSupervisorUncertaintyError(statusErr) || !observed.ControlObservation || observed.Admission != task.AdmissionUnknown || observed.Pending != "" {
		t.Fatalf("missing-row Status classification: %+v, %v", observed, statusErr)
	}
	proven, err := td.ProveUnlaunchedTaskContext(t.Context())
	if err != nil || !proven {
		t.Fatalf("fixture lacks existing complete never-launched proof: %t, %v", proven, err)
	}
}

// This finite supervisor fixture can only report version and an empty queue.
// Production Status therefore encounters a genuinely missing numeric row;
// there is no daemon, Add, enqueue, worker or provider behind the fixture.
func schedulerSubmittedFixture(t *testing.T) (*taskdir.Store, *taskdir.TaskDir, schedulerTasks) {
	t.Helper()
	store, template, original := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, template) })
	base := supervisorfixture.CanonicalTempDir(t)
	executable, configPath := filepath.Join(base, "pueue"), filepath.Join(base, "pueue.json")
	supervisorfixture.WriteExecutable(t, executable, `#!/bin/sh
set -eu
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status) printf '%s\n' '{"tasks":{},"groups":{}}' ;;
  *) exit 64 ;;
esac
`)
	shared := map[string]string{}
	for _, name := range []string{"pueue_directory", "runtime_directory", "unix_socket_path", "alias_file", "pid_path", "shared_secret_path", "daemon_cert", "daemon_key"} {
		shared[name] = filepath.Join(base, name)
	}
	data, err := json.Marshal(struct {
		Shared map[string]string `json:"shared"`
	}{shared})
	if err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, configPath, data)
	client, err := pueue.Bind(t.Context(), executable, configPath, pueue.Options{Environment: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	_, meta, err := template.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	req := *original
	req.TaskID = strings.Repeat("a", 32)
	meta.TaskID, meta.SupervisorConfig = req.TaskID, client.Binding()
	td, err := store.CreateTask(req.TaskID, &req, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	submit, err := td.PrepareSubmission(meta.SupervisorConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err = errors.Join(submit.Consume(), submit.Release()); err != nil {
		t.Fatal(err)
	}
	recordSchedulerFixtureReceipt(t, td)
	return store, td, schedulerObservationFixture(store)
}

func recordSchedulerFixtureReceipt(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	req, meta, spec, metaHash, err := td.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := (pueue.Observation{
		Matched: true, NumericTaskID: 7, Binding: meta.SupervisorConfig,
		Identity: tasklifecycle.TaskIdentity(req, meta, spec, metaHash, nil),
	}).Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err = td.RecordSupervisorReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
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

func TestSchedulerObservationCollectsSealedResultAtProvenClosure(t *testing.T) {
	store, td, request := newAppTestTask(t, true)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	recordCustodyLifetime(t, td, nil, task.LifetimeTerminationCompleted)
	state, err := schedulerObservationFixture(store).Observe(request.TaskID)
	if err != nil || state.Release == nil || state.Observation.Publication != task.PublicationCommitted || state.Observation.OutcomeSHA256 == "" {
		t.Fatalf("closed sealed task was not collected: %+v, %v", state, err)
	}
	outcome, err := td.OutcomeReference()
	if err != nil || state.Observation.OutcomeSHA256 != outcome {
		t.Fatalf("observation does not match published winner: %+v, %q, %v", state, outcome, err)
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
