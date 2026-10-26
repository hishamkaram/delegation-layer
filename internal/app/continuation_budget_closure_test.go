package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// These public-command tests replay a provider turn whose guardian closed the
// native lifetime because the budget elapsed: a budget empty-scope
// termination, a native worker Wait, a budget stop request without its derived
// terminated observation, a released runner lease, and no seal or outcome.
// The validated termination cause owns the timeout classification; the stop
// annotation and any earlier status poll are not required.

const budgetClosureStopID = "budget"

func budgetClosureSetup() rebootSetup {
	return rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedBudget, launchEnvironment: true}
}

// newBudgetClosureFixture records the request-only budget stop and proves the
// predecessor carries exactly the reported records before any command runs.
func newBudgetClosureFixture(t *testing.T, setup rebootSetup) *rebootFixture {
	t.Helper()
	fixture := newRebootFixture(t, setup)
	recordRebootStopRequest(t, fixture, budgetClosureStopID, "budget")
	requireBudgetClosureLifetime(t, fixture.td, task.LifetimeTerminationBudget)
	requireBudgetStopUnobserved(t, fixture)
	inspection, err := fixture.td.Inspect()
	if err != nil || inspection.SealExists || inspection.OutcomeExists || inspection.Publication != task.PublicationUnknown {
		t.Fatalf("inspection = %+v err=%v, want no seal or outcome", inspection, err)
	}
	return fixture
}

// requireBudgetClosureLifetime resolves the canonical native closure from the
// production stores and requires the named termination cause.
func requireBudgetClosureLifetime(t *testing.T, td *taskdir.TaskDir, cause string) {
	t.Helper()
	evidence, err := taskdir.LoadLifetimeEvidenceContext(t.Context(), td)
	if err != nil {
		t.Fatalf("canonical lifetime evidence did not load: %v", err)
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil || closure.Kind != task.LifetimeClosureRegisteredNative {
		t.Fatalf("lifetime closure = %+v err=%v, want registered-native", closure, err)
	}
	if evidence.Termination == nil || evidence.Termination.Cause != cause || !evidence.Termination.ScopeEmpty {
		t.Fatalf("termination = %+v, want %s with empty scope", evidence.Termination, cause)
	}
	if wait := evidence.WorkerWait; wait == nil || wait.Result != task.LifetimeWorkerWaitExitError {
		t.Fatalf("worker completion = %+v, want native exit-error Wait", wait)
	}
}

// requireBudgetStopUnobserved proves no derived terminated annotation exists.
func requireBudgetStopUnobserved(t *testing.T, fixture *rebootFixture) {
	t.Helper()
	stops, err := fixture.td.ReadStopRecords()
	if err != nil || len(stops) != 1 || stops[0].Request == nil || stops[0].Request.Cause != "budget" || stops[0].Observation != nil {
		t.Fatalf("stop records = %+v err=%v, want one unobserved budget request", stops, err)
	}
	if tasklifecycle.HasTerminatedBudgetStop(stops) {
		t.Fatal("fixture already carries a terminated budget annotation")
	}
}

// requireBudgetTimedOut checks the timeout envelope: no outcome, payload, or
// failure, and resumable guidance from the observed provider identity.
func requireBudgetTimedOut(t *testing.T, fixture *rebootFixture, result commandResult, wantCode int) {
	t.Helper()
	response := result.response
	if result.code != wantCode || response.Status != "timed_out" || response.Publication != task.PublicationUnknown.String() || response.Outcome != nil || response.Payload != nil || response.Failure != nil {
		t.Fatalf("%s = code %d err=%v %+v, want exit %d timed_out", response.Command, result.code, result.err, response, wantCode)
	}
	if response.Command == "collect" && (response.Pending == nil || response.Pending.Kind != string(tasklifecycle.PendingPublication)) {
		t.Fatalf("collect pending = %+v, want publication", response.Pending)
	}
	requireInterruptedContinuation(t, fixture, response, "observed", true)
}

func budgetClosureTerminationDigest(t *testing.T, td *taskdir.TaskDir) string {
	t.Helper()
	termination, err := td.ReadLifetimeTerminationContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(termination)
	if err != nil {
		t.Fatal(err)
	}
	return task.ComputeSHA256(data)
}

func TestCollectFirstReportsBudgetClosureTimedOut(t *testing.T) {
	fixture := newBudgetClosureFixture(t, budgetClosureSetup())
	before := snapshotRebootTaskFiles(t, fixture.td.Dir)
	resetRebootCommandLog(t, fixture)

	requireBudgetTimedOut(t, fixture, rebootCollect(t, fixture), 3)
	// Collect without a watch observes local records only: no supervisor
	// command, no derived annotation, and no handoff.
	if commands := readRebootCommandLog(t, fixture); commands != "" {
		t.Fatalf("collect without watch ran supervisor commands %q", commands)
	}
	requireBudgetStopUnobserved(t, fixture)
	requireRebootTaskFilesUnchanged(t, before)
	requireRebootSuccessorBusy(t, fixture)
}

// The classification reads only lifetime records, so every provider catalog
// sees the same cause; the catalog decides resume guidance alone.
func TestBudgetClosureClassificationIsProviderIndependent(t *testing.T) {
	fixture := newBudgetClosureFixture(t, budgetClosureSetup())
	for _, tc := range []struct {
		name      string
		catalog   commonprovider.Catalog
		mode      commonprovider.ContinuationMode
		resumable bool
	}{
		{name: "native continuation provider", catalog: rebootDependencies(t, fixture).Catalog, mode: commonprovider.ContinuationNative, resumable: true},
		{name: "unregistered provider", catalog: commonprovider.Catalog{}, mode: commonprovider.ContinuationUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offer, err := tasklifecycle.ObserveTimeoutContinuation(fixture.root, fixture.td, &fixture.req, &fixture.meta, tc.catalog)
			if err != nil || offer == nil || offer.Cause != tasklifecycle.ContinuationTimedOut || offer.Mode != string(tc.mode) || offer.Resumable != tc.resumable {
				t.Fatalf("offer = %+v err=%v, want timed_out mode=%s resumable=%t", offer, err, tc.mode, tc.resumable)
			}
		})
	}
	requireBudgetStopUnobserved(t, fixture)
}

func TestBudgetClosureClassificationIsOrderIndependent(t *testing.T) {
	t.Run("status first", func(t *testing.T) {
		fixture := newBudgetClosureFixture(t, budgetClosureSetup())
		status := rebootStatus(t, fixture)
		requireBudgetTimedOut(t, fixture, status, 0)
		collected := rebootCollect(t, fixture)
		requireBudgetTimedOut(t, fixture, collected, 3)
		if *collected.response.Continuation != *status.response.Continuation {
			t.Fatalf("collect continuation = %+v, want status %+v", collected.response.Continuation, status.response.Continuation)
		}
	})
	t.Run("collect first then explicit continue", func(t *testing.T) {
		fixture := newBudgetClosureFixture(t, budgetClosureSetup())
		collected := rebootCollect(t, fixture)
		requireBudgetTimedOut(t, fixture, collected, 3)
		if err := tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)); err != nil {
			t.Fatalf("explicit continue after collect: %v", err)
		}
		// Without an observed stop the closure termination is the witness,
		// even though the displayed cause is a timeout.
		release, err := os.ReadFile(rebootSessionReleasePath(fixture))
		if err != nil {
			t.Fatal(err)
		}
		requireBudgetStopUnobserved(t, fixture)

		// A later derived annotation changes neither the classification nor
		// the stored handoff.
		recordRebootBudgetObservation(t, fixture, budgetClosureStopID)
		requireBudgetTimedOut(t, fixture, rebootStatus(t, fixture), 0)
		requireBudgetTimedOut(t, fixture, rebootCollect(t, fixture), 3)
		if err = tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)); err != nil {
			t.Fatalf("explicit continue after later annotation: %v", err)
		}
		if after, readErr := os.ReadFile(rebootSessionReleasePath(fixture)); readErr != nil || !bytes.Equal(after, release) {
			t.Fatalf("later annotation rewrote the closure handoff: err=%v", readErr)
		}
		requireRebootReleaseDigest(t, fixture, budgetClosureTerminationDigest(t, fixture.td))
	})
}

func TestBudgetClosureKeepsCancellationAndCausePrecedence(t *testing.T) {
	t.Run("budget closure with user stop", func(t *testing.T) {
		fixture := newBudgetClosureFixture(t, budgetClosureSetup())
		recordRebootStopRequest(t, fixture, strings.Repeat("c", 32), "user")
		for _, result := range []commandResult{rebootStatus(t, fixture), rebootCollect(t, fixture)} {
			response := result.response
			continuation, failure := response.Continuation, response.Failure
			wantCode := map[string]int{"status": 0, "collect": 3}[response.Command]
			if result.code != wantCode || response.Status != "timed_out" || continuation == nil || continuation.Resumable || continuation.ContinueCommand != "" || continuation.Reason != "task was cancelled by the user" {
				t.Fatalf("%s cancelled budget closure = code %d %+v", response.Command, result.code, response)
			}
			if failure == nil || failure.Code != "task_cancelled" || failure.NextAction != "stop_and_report" {
				t.Fatalf("%s failure = %+v, want task_cancelled/stop_and_report", response.Command, failure)
			}
		}
	})
	t.Run("user stop won requested cause", func(t *testing.T) {
		fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedRequested, launchEnvironment: true})
		requireBudgetClosureLifetime(t, fixture.td, task.LifetimeTerminationRequested)
		recordRebootStopRequest(t, fixture, strings.Repeat("c", 32), "user")
		requireInterruptedExitCodes(t, fixture, "observed", "stop_and_report")
	})
	t.Run("non-budget native closure", func(t *testing.T) {
		fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedRequested, launchEnvironment: true})
		requireBudgetClosureLifetime(t, fixture.td, task.LifetimeTerminationRequested)
		requireInterruptedExitCodes(t, fixture, "observed", "continue_task")
	})
}

func TestBudgetClosureWithoutWorkerWaitStaysPending(t *testing.T) {
	fixture := newBudgetClosureFixture(t, budgetClosureSetup())
	if err := os.Remove(filepath.Join(fixture.td.Dir, "lifetime", "worker-wait.json")); err != nil {
		t.Fatal(err)
	}
	observed := rebootStatus(t, fixture)
	if observed.response.Status != "" || observed.response.Continuation != nil || observed.response.Failure != nil {
		t.Fatalf("status without worker Wait = code %d %+v, want no offer", observed.code, observed.response)
	}
	collected := rebootCollect(t, fixture)
	if collected.code != 3 || collected.response.Status != "" || collected.response.Continuation != nil || collected.response.Pending == nil || collected.response.Pending.Kind != "publication" || collected.response.Failure != nil {
		t.Fatalf("collect without worker Wait = code %d %+v, want pending publication", collected.code, collected.response)
	}
}

func TestMalformedBudgetClosureIsRefused(t *testing.T) {
	fixture := newBudgetClosureFixture(t, budgetClosureSetup())
	writeRebootTaskFile(t, fixture, filepath.Join("lifetime", "terminated.json"), []byte("{"))
	for _, result := range []commandResult{rebootStatus(t, fixture), rebootCollect(t, fixture)} {
		response := result.response
		if result.code != 1 || result.err == nil || response.Status == "timed_out" || response.Status == "interrupted" || (response.Continuation != nil && response.Continuation.Resumable) {
			t.Fatalf("%s with malformed termination = code %d err=%v %+v", response.Command, result.code, result.err, response)
		}
	}
}

func TestSealedBudgetClosureIsCollectedNotTimedOut(t *testing.T) {
	setup := budgetClosureSetup()
	setup.seal = true
	fixture := newRebootFixture(t, setup)
	requireBudgetClosureLifetime(t, fixture.td, task.LifetimeTerminationBudget)
	observed := rebootStatus(t, fixture)
	if observed.response.Status != "" || observed.response.Continuation != nil {
		t.Fatalf("status over sealed budget closure = code %d %+v, want no offer", observed.code, observed.response)
	}
	outcome := collectSealedRebootOutcome(t, fixture)
	requireTerminalRebootContinuation(t, fixture)
	requireRebootReleaseDigest(t, fixture, outcome.EvidenceSHA256)
}

// A rejected outcome is as authoritative as a committed one: the budget
// closure beside it never reports a timeout.
func TestRejectedOutcomeWinsOverBudgetClosure(t *testing.T) {
	store, td, req := newAppTestTaskWithPrior(t, false, testPriorSession())
	defer closeAppTestTask(t, store, td)
	if err := td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatal(err)
	}
	sealRejectedTurn(t, td)
	recordRebootLifetime(t, td, rebootLifetimeClosedBudget)
	collected := collect(Arguments{Root: store.Root, TaskID: req.TaskID}, storeDependencies{})
	if collected.response.Publication != task.PublicationRejected.String() || collected.response.Outcome == nil || collected.response.Status != "" || collected.response.Continuation != nil {
		t.Fatalf("collect with rejected outcome and budget closure = code %d err=%v %+v", collected.code, collected.err, collected.response)
	}
	observed := status(Arguments{Root: store.Root, TaskID: req.TaskID}, Dependencies{ResourceChecker: fixtureResources})
	if observed.code != 0 || observed.response.Publication != task.PublicationRejected.String() || observed.response.Status != "" || observed.response.Continuation != nil {
		t.Fatalf("status with rejected outcome and budget closure = code %d %+v", observed.code, observed.response)
	}
}

// sealRejectedTurn seals an empty answer with a failed exit under a consumed
// start permit, which the fixture predicate rejects at collection.
func sealRejectedTurn(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err = td.WriteRawFiles("", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = td.Seal(task.InvocationStarted, 1, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
}
