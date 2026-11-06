package app

import (
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// These tests drive the public collect --watch consumer over a sealed,
// unpublished, budget-terminated task. Its predicate port is reachable, so a
// scripted interpreter marks the pass boundaries: every Evaluate call is one
// collection pass, and a durable user stop written inside the second call is
// committed after the first pass derived its offer.

const (
	watchStopTaskID  = "d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1"
	watchStopRunner  = "/usr/bin/delegate-run"
	watchStopSession = "watch-session"
)

var errInjectedWatchFault = errors.New("injected collection fault")

// scriptedInterpreter returns the fixture interpretation, which is always
// valid, joined with the scripted error of its call. Calls are synchronous with
// the collecting goroutine, so they need no locking.
type scriptedInterpreter struct {
	inner  predicate.Interpreter
	script func(call int) error
	calls  int
}

func (i *scriptedInterpreter) Reference() task.PredicateRef { return i.inner.Reference() }

func (i *scriptedInterpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	i.calls++
	interpretation, err := i.inner.Evaluate(input, raw, out)
	return interpretation, errors.Join(err, i.script(i.calls))
}

// watchStopFixture is a sealed task whose provider turn ended by a terminated
// budget stop, with the identity, runner, and launch environment a successor
// needs. Nothing is published yet.
type watchStopFixture struct {
	root    string
	deps    Dependencies
	storeID string
}

func newWatchStopFixture(t *testing.T, script func(call int) error) (*watchStopFixture, *scriptedInterpreter) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("watch stop")
	req := task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: watchStopTaskID, Provider: "fixture:test", Mode: "read-only", CanonicalCwd: workspace, RequestedConfig: task.TaskConfig{Permission: "read-only", Budget: "1m0s"}, BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
	digest := task.ComputeSHA256([]byte("watch-stop"))
	supervisor := task.SupervisorRef{ClientExecutable: "/tmp/app-pueue", ClientSHA256: digest, ResolvedConfigSHA256: digest, ConfigPath: "/tmp/app-pueue.yml", ConfigDigest: digest, Endpoint: "unix:/tmp/app-pueue.sock", ObservedVersion: pueue.FixtureVersion}
	meta := tasklifecycle.NewMeta(req, appTestExecutionProfile(&req, "/usr/bin/true", nil), supervisor, watchStopRunner, task.RunnerOwnershipCustom, "watch-stop-test")
	meta.Environment, meta.EnvironmentRecorded = []string{"LANG=C"}, true
	td, err := store.CreateTask(req.TaskID, &req, brief, &meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	sealWatchStopTask(t, td, req.Provider)
	prepareAppPublishedTimeoutEvidence(t, td)

	interpreter := &scriptedInterpreter{inner: predicate.FixtureV1(), script: script}
	registry, err := predicate.New(interpreter)
	if err != nil {
		t.Fatal(err)
	}
	deps := fixtureContinuationDependencies(t, nil, "")
	deps.PredicateRegistry = func() predicate.Registry { return registry }
	fixture := &watchStopFixture{root: root, deps: deps, storeID: req.TaskID}
	fixture.requireResumableOffer(t, td)
	return fixture, interpreter
}

func sealWatchStopTask(t *testing.T, td *taskdir.TaskDir, provider string) {
	t.Helper()
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordProviderIdentity(task.SessionIdentity{Provider: provider, ConversationID: watchStopSession}); err != nil {
		t.Fatal(err)
	}
	if err = td.WriteRawFiles("answer\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
}

// requireResumableOffer proves the canonical observer offers a continuation
// before any user stop exists, so a later non-resumable result is a change.
func (f *watchStopFixture) requireResumableOffer(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	req, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	offer, err := tasklifecycle.ObserveTimeoutContinuation(f.root, td, req, meta, f.deps.Catalog)
	if err != nil || offer == nil || offer.Cause != tasklifecycle.ContinuationTimedOut || !offer.Resumable || offer.SessionSource != tasklifecycle.ContinuationSessionObserved {
		t.Fatalf("initial offer = %+v err=%v, want resumable observed timed_out", offer, err)
	}
}

// commitUserStop writes the durable user stop through a separately opened
// production store, as another process running stop would.
func (f *watchStopFixture) commitUserStop() (resultErr error) {
	store, err := taskdir.OpenStoreWithPredicates(f.root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	td, err := store.OpenTask(f.storeID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	permit, err := td.PrepareStop(strings.Repeat("c", 32), "user", time.Time{})
	if err != nil {
		return err
	}
	return errors.Join(permit.Consume(), permit.Release())
}

func (f *watchStopFixture) requireUserStopDurable(t *testing.T) {
	t.Helper()
	store, err := taskdir.OpenStoreWithPredicates(f.root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	td, err := store.OpenTask(f.storeID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if stopped, stopErr := td.UserStopRequestedContext(t.Context()); stopErr != nil || !stopped {
		t.Fatalf("durable user stop = %t err=%v, want recorded", stopped, stopErr)
	}
}

// collectWatch runs the public command with a finite watch. Fake time makes
// the polling waits instantaneous; the interpreter script owns ordering.
func (f *watchStopFixture) collectWatch(t *testing.T) (int, Response) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := Run([]string{"--root", f.root, "collect", f.storeID, "--watch", "2s", "--json"}, &stdout, &stderr, f.deps)
	var response Response
	if err := json.Unmarshal([]byte(stdout.String()), &response); err != nil {
		t.Fatalf("collect output %q: %v (stderr %q)", stdout.String(), err, stderr.String())
	}
	return code, response
}

// stopAfterFirstPass lets pass one derive its offer, commits the user stop in
// pass two, and keeps publication pending until the script's release call.
func stopAfterFirstPass(t *testing.T, fixture func() *watchStopFixture, publishAt int) func(call int) error {
	return func(call int) error {
		if call == 2 {
			if err := fixture().commitUserStop(); err != nil {
				t.Errorf("commit user stop: %v", err)
			}
		}
		if publishAt > 0 && call >= publishAt {
			return nil
		}
		return task.ErrLockBusy
	}
}

// requireCancelledPendingCollect checks the wire after a user stop landed
// mid-watch: the stale resumable offer is gone and the task is still pending.
func requireCancelledPendingCollect(t *testing.T, code int, response Response) {
	t.Helper()
	continuation := response.Continuation
	if continuation == nil || continuation.Resumable || continuation.ContinueCommand != "" || continuation.Reason != "task was cancelled by the user" {
		t.Fatalf("stale offer survived the user stop: %+v", continuation)
	}
	if code != 3 || response.Status != "timed_out" || response.Pending == nil || response.Pending.Kind != "publication" || response.Outcome != nil || response.Error != "" {
		t.Fatalf("collect = code %d %+v, want pending publication timed_out", code, response)
	}
	failure := response.Failure
	if failure == nil || failure.Code != "task_cancelled" || failure.Stage != "collect" || failure.NextAction != "stop_and_report" {
		t.Fatalf("failure = %+v, want task_cancelled/stop_and_report", failure)
	}
}

func TestCollectWatchReevaluatesOfferAfterUserStopInSameWatch(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var fixture *watchStopFixture
		fixture, interpreter := newWatchStopFixture(t, stopAfterFirstPass(t, func() *watchStopFixture { return fixture }, 0))
		code, response := fixture.collectWatch(t)

		if interpreter.calls < 2 {
			t.Fatalf("interpreter calls = %d, want at least two collection passes", interpreter.calls)
		}
		fixture.requireUserStopDurable(t)
		requireCancelledPendingCollect(t, code, response)
	})
}

func TestCollectWatchConcreteFaultAfterOfferCarriesNoContinuation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, interpreter := newWatchStopFixture(t, func(call int) error {
			if call == 2 {
				return errors.Join(task.ErrLockBusy, errInjectedWatchFault)
			}
			return task.ErrLockBusy
		})
		code, response := fixture.collectWatch(t)

		if interpreter.calls != 2 {
			t.Fatalf("interpreter calls = %d, want the concrete fault to end the second pass", interpreter.calls)
		}
		if code != 1 || response.Error == "" || response.Continuation != nil || response.Status == "timed_out" || response.Outcome != nil {
			t.Fatalf("collect = code %d %+v, want operational error without an earlier pass's offer", code, response)
		}
		if response.Failure != nil && response.Failure.NextAction == "continue_task" {
			t.Fatalf("operational error advertised continue_task: %+v", response.Failure)
		}
	})
}

func TestCollectWatchPublicationAfterUserStopHasNoCancellationFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fixture *watchStopFixture
		fixture, interpreter := newWatchStopFixture(t, stopAfterFirstPass(t, func() *watchStopFixture { return fixture }, 3))
		code, response := fixture.collectWatch(t)

		if interpreter.calls != 3 {
			t.Fatalf("interpreter calls = %d, want publication on the third pass", interpreter.calls)
		}
		fixture.requireUserStopDurable(t)
		if code != 0 || response.Publication != task.PublicationCommitted.String() || response.Outcome == nil || response.Pending != nil {
			t.Fatalf("collect = code %d %+v, want committed outcome", code, response)
		}
		if response.Failure != nil {
			t.Fatalf("earlier pass's cancellation failure survived publication: %+v", response.Failure)
		}
		if response.Continuation != nil && response.Continuation.Resumable {
			t.Fatalf("cancelled task advertised resumption: %+v", response.Continuation)
		}
	})
}
