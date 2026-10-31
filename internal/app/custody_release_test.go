package app

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// custodyLifetimeOpen leaves the recorded lifetime without a termination.
const custodyLifetimeOpen = ""

// custodyLifetimeRecords is the task or inspection journal a test records a
// lifetime in, under the family's own owner lease.
type custodyLifetimeRecords interface {
	guardian.Records
	AcquireLifetimeOwner(context.Context) (*taskdir.LifetimeOwnerLease, error)
	ReadLifetimeWorkerContext(context.Context) (*task.LifetimeWorkerRecord, error)
}

// recordCustodyLifetime records a registered lifetime with placement, closed by
// a native termination with cause unless cause is custodyLifetimeOpen.
func recordCustodyLifetime(t *testing.T, td custodyLifetimeRecords, placement *task.LifetimeCustodyPlacement, cause string) *task.LifetimeRegistrationRecord {
	t.Helper()
	return recordCustodyLifetimeUntil(t, td, placement, cause, time.Now().UTC().Add(time.Hour))
}

// recordCustodyLifetimeUntil records with the journal's own deadline, which an
// inspection journal requires.
func recordCustodyLifetimeUntil(t *testing.T, td custodyLifetimeRecords, placement *task.LifetimeCustodyPlacement, cause string, deadline time.Time) *task.LifetimeRegistrationRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	owner, err := td.AcquireLifetimeOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	if _, err = td.RecordLifetimeIntentContext(ctx, taskdir.LifetimeIntentFacts{ScopeKind: task.LifetimeScopeCgroupV2, Assurance: task.LifetimeAssuranceExact, ScopePlan: "custody-plan", BootID: "custody-boot", Deadline: deadline}); err != nil {
		t.Fatal(err)
	}
	registration, err := td.RecordLifetimeRegistrationContext(ctx, taskdir.LifetimeRegistrationFacts{ScopeKind: task.LifetimeScopeCgroupV2, Assurance: task.LifetimeAssuranceExact, ScopeID: "custody-scope", BootID: "custody-boot", Deadline: deadline, CustodyPlacement: placement})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = td.RecordLifetimeWorkerContext(ctx, taskdir.LifetimeWorkerFacts{PID: 4242, ProcessStart: "custody-worker-start", BootID: "custody-boot"}); err != nil {
		t.Fatal(err)
	}
	if cause == custodyLifetimeOpen {
		return registration
	}
	if _, err = td.RecordLifetimeTerminationContext(ctx, cause); err != nil {
		t.Fatal(err)
	}
	worker, err := td.ReadLifetimeWorkerContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := task.MarshalCanonical(worker)
	if err != nil {
		t.Fatal(err)
	}
	workerDigest := task.ComputeSHA256(encoded)
	exitCode := 1
	if _, err = td.RecordLifetimeWorkerWaitContext(ctx, taskdir.LifetimeWorkerWaitFacts{WorkerSHA256: workerDigest, ChildPID: 4242, Result: task.LifetimeWorkerWaitExitError, ExitCode: &exitCode}); err != nil {
		t.Fatal(err)
	}
	return registration
}

type custodyObservationCommand func(*taskdir.Store, string) commandResult

func custodyObservationCommands() map[string]custodyObservationCommand {
	return map[string]custodyObservationCommand{
		"status": func(store *taskdir.Store, taskID string) commandResult {
			return status(Arguments{Root: store.Root, TaskID: taskID}, Dependencies{ResourceChecker: fixtureResources})
		},
		"collect": func(store *taskdir.Store, taskID string) commandResult {
			return collect(Arguments{Root: store.Root, TaskID: taskID}, storeDependencies{})
		},
	}
}

// custodyObservedResult is the observable command result a custody fault must
// never change.
func custodyObservedResult(t *testing.T, result commandResult) string {
	t.Helper()
	response, err := json.Marshal(result.response)
	if err != nil {
		t.Fatal(err)
	}
	errText := ""
	if result.err != nil {
		errText = result.err.Error()
	}
	encoded, err := json.Marshal(struct {
		Code     int
		Err      string
		Response json.RawMessage
	}{result.code, errText, response})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestStatusAndCollectReleaseCustodyOnEveryClosedPath covers closures that
// Interrupted() does not report: a budget closure (timed out), a requested
// closure and a completed native closure. A missing pool retains its views and
// never changes the command result; the next observation releases the exact
// recorded pool.
func TestStatusAndCollectReleaseCustodyOnEveryClosedPath(t *testing.T) {
	causes := []string{task.LifetimeTerminationBudget, task.LifetimeTerminationRequested, task.LifetimeTerminationCompleted}
	for name, run := range custodyObservationCommands() {
		for _, cause := range causes {
			t.Run(name+"/"+cause, func(t *testing.T) {
				store, td, request := newAppTestTask(t, false)
				defer closeAppTestTask(t, store, td)
				placement := newCustodyTestPlacement(t)
				registration := recordCustodyLifetime(t, td, placement, cause)
				bindCustodyTestView(t, registration)

				restore := moveCustodyTestPool(t, placement)
				faulted := custodyObservedResult(t, run(store, request.TaskID))
				restore()
				if !custodyTestPending(t, registration) {
					t.Fatal("a missing pool was treated as released")
				}
				healthy := custodyObservedResult(t, run(store, request.TaskID))
				if faulted != healthy {
					t.Fatalf("custody fault changed the result:\nfaulted %s\nhealthy %s", faulted, healthy)
				}
				if custodyTestPending(t, registration) {
					t.Fatalf("%s did not release custody after a %s closure", name, cause)
				}
			})
		}
	}
}

func TestStatusAndCollectKeepCustodyWhileClosurePending(t *testing.T) {
	for name, run := range custodyObservationCommands() {
		t.Run(name, func(t *testing.T) {
			store, td, request := newAppTestTask(t, false)
			defer closeAppTestTask(t, store, td)
			registration := recordCustodyLifetime(t, td, newCustodyTestPlacement(t), custodyLifetimeOpen)
			bindCustodyTestView(t, registration)
			run(store, request.TaskID)
			if !custodyTestPending(t, registration) {
				t.Fatalf("%s released custody before canonical closure", name)
			}
		})
	}
}

func TestCollectReleasesCustodyBesideTerminalOutcome(t *testing.T) {
	store, td, req := newAppTestTaskWithPrior(t, false, testPriorSession())
	defer closeAppTestTask(t, store, td)
	if err := td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatal(err)
	}
	sealRejectedTurn(t, td)
	registration := recordCustodyLifetime(t, td, newCustodyTestPlacement(t), task.LifetimeTerminationBudget)
	bindCustodyTestView(t, registration)
	collected := collect(Arguments{Root: store.Root, TaskID: req.TaskID}, storeDependencies{})
	if collected.response.Publication != task.PublicationRejected.String() || collected.code != 4 {
		t.Fatalf("collect = code %d err=%v %+v, want the rejected outcome", collected.code, collected.err, collected.response)
	}
	if custodyTestPending(t, registration) {
		t.Fatal("collect of a terminal task did not release custody")
	}
}

func TestLifetimeCustodyResolverIgnoresUnplacedRegistrations(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	if err := releaseLifetimeCustody(t.Context(), td); err != nil {
		t.Fatalf("release without registration: %v", err)
	}
	recordCustodyLifetime(t, td, nil, task.LifetimeTerminationBudget)
	if err := releaseLifetimeCustody(t.Context(), td); err != nil {
		t.Fatalf("release of a legacy registration: %v", err)
	}
	if pending, err := lifetimeCustodyPending(t.Context(), td); err != nil || pending {
		t.Fatalf("legacy registration pending = %t, %v; want nothing to release", pending, err)
	}
}

func TestLifetimeCustodyResolverRetainsForeignOrMissingPool(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	placement := newCustodyTestPlacement(t)
	registration := recordCustodyLifetime(t, td, placement, task.LifetimeTerminationCompleted)
	bindCustodyTestView(t, registration)

	foreign := newCustodyTestPlacement(t)
	recorded := *registration
	recorded.CustodyPlacement = &task.LifetimeCustodyPlacement{Version: placement.Version, PoolRoot: foreign.PoolRoot, PoolID: placement.PoolID}
	if custody, err := recordedLifetimeCustody(&recorded); err != nil {
		t.Fatal(err)
	} else if err = custody.pool.ReleaseOwner(t.Context(), custody.namespace); err == nil {
		t.Fatal("a pool with a different identity accepted the release")
	}

	restore := moveCustodyTestPool(t, placement)
	if err := releaseLifetimeCustody(t.Context(), td); err == nil {
		t.Fatal("release of a missing pool reported success")
	}
	if _, err := lifetimeCustodyPending(t.Context(), td); err == nil {
		t.Fatal("pending check of a missing pool reported an answer")
	}
	restore()
	if pending, err := lifetimeCustodyPending(t.Context(), td); err != nil || !pending {
		t.Fatalf("restored pool pending = %t, %v; want retained views", pending, err)
	}
	if err := releaseLifetimeCustody(t.Context(), td); err != nil {
		t.Fatal(err)
	}
	if pending, err := lifetimeCustodyPending(t.Context(), td); err != nil || pending {
		t.Fatalf("released pool pending = %t, %v", pending, err)
	}
	if err := releaseLifetimeCustody(t.Context(), td); err != nil {
		t.Fatalf("a second release of the same owner: %v", err)
	}
}

// newCustodyRebootFixture is the canonical budget-closure command fixture with
// the lifetime recorded here instead, so its registration carries placement.
func newCustodyRebootFixture(t *testing.T, holdRunnerLease bool, cause string) (*rebootFixture, *task.LifetimeRegistrationRecord) {
	t.Helper()
	setup := budgetClosureSetup()
	setup.stage, setup.holdRunnerLease = rebootLifetimeNone, holdRunnerLease
	fixture := newRebootFixture(t, setup)
	registration := recordCustodyLifetime(t, fixture.td, newCustodyTestPlacement(t), cause)
	recordRebootStopRequest(t, fixture, budgetClosureStopID, "budget")
	bindCustodyTestView(t, registration)
	return fixture, registration
}

// TestStatusAndCollectRequestCustodyReleaseWhenOfferDerivationFails: a
// malformed provider identity makes the timed-out offer's session resolution
// fail after closure is proven. The offer is absent and the session fault is
// reported. The private public path releases nothing: a pool fault is only a
// warning, a provable remaining release persists one full wake, and the
// elected owner's custody phase then releases the exact recorded pool without
// changing that result.
func TestStatusAndCollectRequestCustodyReleaseWhenOfferDerivationFails(t *testing.T) {
	commands := map[string]func(*testing.T, *rebootFixture) commandResult{"status": rebootStatus, "collect": rebootCollect}
	for name, run := range commands {
		t.Run(name, func(t *testing.T) {
			fixture, registration := newCustodyRebootFixture(t, false, task.LifetimeTerminationBudget)
			requireBudgetClosureLifetime(t, fixture.td, task.LifetimeTerminationBudget)
			writeRebootTaskFile(t, fixture, "provider.ref.json", []byte("{"))

			restore := moveCustodyTestPool(t, registration.CustodyPlacement)
			before := readTaskMaintenanceWake(t, fixture.root)
			faultedResult := run(t, fixture)
			restore()
			if !custodyTestPending(t, registration) {
				t.Fatal("a missing pool was treated as released")
			}
			requireTaskMaintenanceWakeUnchanged(t, fixture.root, before, "a pool fault")
			healthyResult := run(t, fixture)
			requirePublicCustodyRequest(t, fixture.root, registration, before)
			runTaskCustodyOwner(t, fixture.root)
			for _, result := range []commandResult{faultedResult, healthyResult} {
				if result.response.Continuation != nil || result.err == nil || !strings.Contains(result.err.Error(), "predecessor session is unavailable") {
					t.Fatalf("%s with failed offer derivation = code %d err=%v continuation=%+v", name, result.code, result.err, result.response.Continuation)
				}
			}
			if faulted, healthy := custodyObservedResult(t, faultedResult), custodyObservedResult(t, healthyResult); faulted != healthy {
				t.Fatalf("custody fault changed the result:\nfaulted %s\nhealthy %s", faulted, healthy)
			}
			if custodyTestPending(t, registration) {
				t.Fatalf("the owner did not release custody after %s requested it", name)
			}
			settled := readTaskMaintenanceWake(t, fixture.root)
			run(t, fixture)
			requireTaskMaintenanceWakeUnchanged(t, fixture.root, settled, "a settled "+name+" poll")
		})
	}
}

// TestStatusRequestsClosedCustodyReleaseWhileRunnerLeaseIsHeld: the runner
// lease (not the lifetime-owner lease) is still held, so no resumable
// continuation is offered, but canonical closure alone makes release work
// remain. The public path persists a wake and the owner releases; an open
// lifetime keeps its views and requests nothing. Each command runs alone on a
// fresh fixture, so one command's request can never mask another's.
func TestStatusRequestsClosedCustodyReleaseWhileRunnerLeaseIsHeld(t *testing.T) {
	// Only status derives the held-lease offer; collect alone reports none.
	commands := map[string]struct {
		run           func(*testing.T, *rebootFixture) commandResult
		reportsLeased bool
	}{"status": {rebootStatus, true}, "collect": {rebootCollect, false}}
	for name, command := range commands {
		for _, tc := range []struct {
			cause       string
			wantPending bool
		}{
			{cause: task.LifetimeTerminationBudget, wantPending: false},
			{cause: custodyLifetimeOpen, wantPending: true},
		} {
			t.Run(name+"/closed="+strconv.FormatBool(!tc.wantPending), func(t *testing.T) {
				fixture, registration := newCustodyRebootFixture(t, true, tc.cause)
				requireRunnerLeaseHeld(t, fixture)
				before := readTaskMaintenanceWake(t, fixture.root)
				requireRunnerLeaseHeldOffer(t, command.run(t, fixture), command.reportsLeased && !tc.wantPending)
				if tc.wantPending {
					requireTaskMaintenanceWakeUnchanged(t, fixture.root, before, "an open lifetime")
				} else {
					requirePublicCustodyRequest(t, fixture.root, registration, before)
				}
				runTaskCustodyOwner(t, fixture.root)
				if got := custodyTestPending(t, registration); got != tc.wantPending {
					t.Fatalf("%s left custody pending = %t, want %t", name, got, tc.wantPending)
				}
			})
		}
	}
}

// requireRunnerLeaseHeld observes the fixture's runner lease through a second
// task handle, as an independent observer would.
func requireRunnerLeaseHeld(t *testing.T, fixture *rebootFixture) {
	t.Helper()
	observer, err := fixture.store.OpenTask(fixture.req.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	released, err := observer.RunnerLeaseReleased()
	if closeErr := observer.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || released {
		t.Fatalf("runner lease released=%t err=%v, want held", released, err)
	}
}

// requireRunnerLeaseHeldOffer requires that no observation offers a resumable
// continuation while the runner lease is held and, when reported is set, that
// the offer names the held lease.
func requireRunnerLeaseHeldOffer(t *testing.T, result commandResult, reported bool) {
	t.Helper()
	offer := result.response.Continuation
	if offer != nil && offer.Resumable {
		t.Fatalf("%s offered a resumable continuation while the runner lease is held: %+v", result.response.Command, offer)
	}
	if reported && (offer == nil || offer.Reason != "runner lease is still active") {
		t.Fatalf("%s continuation %+v, want the held runner lease reported", result.response.Command, offer)
	}
}

// TestStatusCustodyReadPathLatency records, without asserting a bound, the
// wall time of repeated status polls for a legacy unplaced registration and
// for placed open and proven-closed registrations on the same fixture shape.
func TestStatusCustodyReadPathLatency(t *testing.T) {
	const polls = 10
	for _, tc := range []struct {
		name      string
		placement bool
		cause     string
	}{
		{name: "legacy-unplaced-closed", cause: task.LifetimeTerminationBudget},
		{name: "placed-open", placement: true, cause: custodyLifetimeOpen},
		{name: "placed-closed", placement: true, cause: task.LifetimeTerminationBudget},
	} {
		store, td, request := newAppTestTask(t, false)
		var placement *task.LifetimeCustodyPlacement
		if tc.placement {
			placement = newCustodyTestPlacement(t)
		}
		registration := recordCustodyLifetime(t, td, placement, tc.cause)
		if tc.placement {
			bindCustodyTestView(t, registration)
		}
		started := time.Now()
		for range polls {
			status(Arguments{Root: store.Root, TaskID: request.TaskID}, Dependencies{ResourceChecker: fixtureResources})
		}
		elapsed := time.Since(started)
		t.Logf("custody-latency %s polls=%d total=%s per-poll=%s", tc.name, polls, elapsed, elapsed/polls)
		closeAppTestTask(t, store, td)
	}
}
