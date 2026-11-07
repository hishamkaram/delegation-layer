package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// These public-command tests observe the reboot fixture's production stores:
// a closed native lifetime with no seal, outcome, or provider stop.

func rebootDependencies(t *testing.T, fixture *rebootFixture) Dependencies {
	t.Helper()
	deps := fixtureContinuationDependencies(t, nil, fixture.clientA)
	deps.SupervisorOptions, deps.RunnerExecutable = fixture.dependencies().SupervisorOptions, fixture.runner
	return deps
}

func rebootStatus(t *testing.T, fixture *rebootFixture) commandResult {
	t.Helper()
	return status(Arguments{Root: fixture.root, TaskID: fixture.req.TaskID}, rebootDependencies(t, fixture))
}

func rebootCollect(t *testing.T, fixture *rebootFixture) commandResult {
	t.Helper()
	return collect(Arguments{Root: fixture.root, TaskID: fixture.req.TaskID}, rebootDependencies(t, fixture).storeDependencies())
}

// requireInterruptedEnvelope checks the honest interruption contract: no
// outcome, payload, or publication promise, and exact continuation guidance.
func requireInterruptedEnvelope(t *testing.T, fixture *rebootFixture, result commandResult, source, nextAction string) {
	t.Helper()
	response := result.response
	if response.Status != "interrupted" || response.Publication != task.PublicationUnknown.String() || response.Outcome != nil || response.Payload != nil || response.Pending != nil {
		t.Fatalf("%s interruption envelope = %+v", response.Command, response)
	}
	requireInterruptedContinuation(t, fixture, response, source, nextAction == "continue_task")
	failure := response.Failure
	if failure == nil || failure.Code != "task_interrupted" || failure.Stage != response.Command || failure.NextAction != nextAction {
		t.Fatalf("%s failure = %+v, want task_interrupted/%s", response.Command, failure, nextAction)
	}
}

func requireInterruptedContinuation(t *testing.T, fixture *rebootFixture, response Response, source string, resumable bool) {
	t.Helper()
	continuation := response.Continuation
	if continuation == nil || continuation.Resumable != resumable || continuation.PredecessorID != fixture.req.TaskID || continuation.SessionSource != source || (continuation.ContinueCommand != "") != resumable {
		t.Fatalf("%s continuation = %+v, want resumable=%t source=%s", response.Command, continuation, resumable, source)
	}
	if resumable && !strings.Contains(continuation.ContinueCommand, "continue --task "+fixture.req.TaskID) {
		t.Fatalf("%s continue command = %q", response.Command, continuation.ContinueCommand)
	}
}

func requireInterruptedExitCodes(t *testing.T, fixture *rebootFixture, source, nextAction string) {
	t.Helper()
	observed := rebootStatus(t, fixture)
	if observed.code != 0 || observed.err != nil {
		t.Fatalf("status exit=%d err=%v, want 0", observed.code, observed.err)
	}
	requireInterruptedEnvelope(t, fixture, observed, source, nextAction)
	collected := rebootCollect(t, fixture)
	if collected.code != 1 || collected.err == nil {
		t.Fatalf("collect exit=%d err=%v, want operational interruption 1", collected.code, collected.err)
	}
	requireInterruptedEnvelope(t, fixture, collected, source, nextAction)
}

func TestStatusAndCollectReportRebootInterruption(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		identity bool
		source   string
	}{
		{name: "observed identity", identity: true, source: "observed"},
		{name: "recorded input", source: "recorded_input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: tc.identity, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
			requireInterruptedExitCodes(t, fixture, tc.source, "continue_task")
			// Observation never publishes or hands off the session.
			requireRebootNoProviderPublication(t, fixture)
			requireRebootSuccessorBusy(t, fixture)
		})
	}
}

func TestStatusAndCollectKeepPendingWithoutClosedReleasedLifetime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup rebootSetup
	}{
		{name: "open lifetime", setup: rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeOpen}},
		{name: "held runner lease", setup: rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, holdRunnerLease: true}},
		{name: "held runner lease after budget closure", setup: rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedBudget, holdRunnerLease: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup.launchEnvironment = true
			fixture := newRebootFixture(t, tc.setup)
			// An open lifetime with an unprovable boot keeps its existing
			// recovery error; neither control may report an interruption.
			observed := rebootStatus(t, fixture)
			if observed.response.Status != "" || observed.response.Continuation != nil || observed.response.Failure != nil {
				t.Fatalf("status = code %d %+v, want no interruption", observed.code, observed.response)
			}
			collected := rebootCollect(t, fixture)
			if collected.code != 3 || collected.response.Pending == nil || collected.response.Pending.Kind != "publication" || collected.response.Failure != nil {
				t.Fatalf("collect = code %d %+v, want pending publication", collected.code, collected.response)
			}
		})
	}
}

func TestUserCancellationSuppressesAutomaticRebootContinuation(t *testing.T) {
	t.Parallel()
	t.Run("interrupted", func(t *testing.T) {
		fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
		recordRebootStop(t, fixture, strings.Repeat("c", 32), "user")
		requireInterruptedExitCodes(t, fixture, "observed", "stop_and_report")
		if reason := rebootStatus(t, fixture).response.Continuation.Reason; reason != "task was cancelled by the user" {
			t.Fatalf("cancellation reason = %q", reason)
		}
		// Suppressed guidance never blocks an explicit continue.
		if err := tasklifecycle.PredecessorTerminated(fixture.root, fixture.td, &fixture.req, rebootSupervisorOptions(fixture)); err != nil {
			t.Fatalf("explicit continue after cancellation was refused: %v", err)
		}
		if err := fixture.successor.ClaimSession(fixture.req.Provider, fixture.req.PriorSession.ConversationID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUnreadableCancellationIsReportedNotHidden(t *testing.T) {
	t.Parallel()
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
	id := strings.Repeat("c", 32)
	recordRebootStop(t, fixture, id, "user")
	if err := os.WriteFile(filepath.Join(fixture.td.Dir, "stop", id+".request.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, result := range []commandResult{rebootStatus(t, fixture), rebootCollect(t, fixture)} {
		if result.code != 1 || result.err == nil || result.response.Failure != nil || (result.response.Continuation != nil && result.response.Continuation.Resumable) {
			t.Fatalf("%s with unreadable cancellation = code %d err=%v %+v", result.response.Command, result.code, result.err, result.response)
		}
	}
}

func TestCommittedOutcomeWinsOverClosedLifetime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		stage rebootLifetimeStage
	}{
		{name: "previous boot", stage: rebootLifetimeClosedPreviousBoot},
		{name: "budget", stage: rebootLifetimeClosedBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, td, req := newAppTestTaskWithPrior(t, true, testPriorSession())
			defer closeAppTestTask(t, store, td)
			if err := td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
				t.Fatal(err)
			}
			recordRebootLifetime(t, td, tc.stage)
			collected := collect(Arguments{Root: store.Root, TaskID: req.TaskID}, storeDependencies{})
			if collected.code != 0 || collected.response.Publication != task.PublicationCommitted.String() || collected.response.Outcome == nil || collected.response.Status != "" || collected.response.Failure != nil {
				t.Fatalf("collect with outcome and closed lifetime = code %d %+v", collected.code, collected.response)
			}
			observed := status(Arguments{Root: store.Root, TaskID: req.TaskID}, Dependencies{ResourceChecker: fixtureResources})
			if observed.code != 0 || observed.response.Publication != task.PublicationCommitted.String() || observed.response.Status != "" || observed.response.Failure != nil {
				t.Fatalf("status with outcome and closed lifetime = code %d %+v", observed.code, observed.response)
			}
		})
	}
}

// A fresh predecessor without provider identity fails at the session stage
// with the continuation family code, and creates no successor.
func TestContinueReportsMissingSessionAsContinuationUnavailable(t *testing.T) {
	t.Parallel()
	fixture := newRebootFixture(t, rebootSetup{stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
	result := continueTask(Arguments{Root: fixture.root, TaskID: fixture.req.TaskID}, rebootDependencies(t, fixture))
	if result.code != 2 || !errors.Is(result.err, os.ErrNotExist) {
		t.Fatalf("continue without session = code %d err=%v", result.code, result.err)
	}
	var output bytes.Buffer
	if err := writeJSONResponse(&output, "continue", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TaskRecord != "not_created" || decoded.TaskID != "" || decoded.Failure == nil || decoded.Failure.Stage != "load-continuation-session" || decoded.Failure.Code != "continuation_unavailable" || decoded.Failure.NextAction != "stop_and_report" {
		t.Fatalf("continue session failure envelope = %+v", decoded)
	}
}

func recordRebootStop(t *testing.T, fixture *rebootFixture, id, cause string) {
	t.Helper()
	recordRebootStopRequest(t, fixture, id, cause)
	if cause == "budget" {
		recordRebootBudgetObservation(t, fixture, id)
	}
}

// recordRebootStopRequest records only the consumed stop request. A budget
// request carries its deadline but no supervisor reply or observation.
func recordRebootStopRequest(t *testing.T, fixture *rebootFixture, id, cause string) {
	t.Helper()
	deadline := time.Time{}
	if cause == "budget" {
		deadline = time.Now().Add(time.Minute)
	}
	permit, err := fixture.td.PrepareStop(id, cause, deadline)
	if err != nil {
		t.Fatalf("prepare %s stop: %v", cause, err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatalf("consume %s stop: %v", cause, err)
	}
	if err = permit.Release(); err != nil {
		t.Fatalf("release %s stop: %v", cause, err)
	}
}

// recordRebootBudgetObservation adds the derived terminated-stop annotation a
// supervisor status pass writes for an existing budget request.
func recordRebootBudgetObservation(t *testing.T, fixture *rebootFixture, id string) {
	t.Helper()
	receipt, err := fixture.td.ReadSupervisorReceipt()
	if err != nil {
		t.Fatal(err)
	}
	numeric := receipt.NumericTaskID
	if err = fixture.td.RecordStopReply(id, task.StopReplyFacts{NumericTaskID: numeric, Action: "kill", Acknowledged: true}); err != nil {
		t.Fatalf("record budget stop reply: %v", err)
	}
	if err = fixture.td.RecordStopObservation(id, task.StopObservationFacts{NumericTaskID: numeric, State: "ended", Terminated: true}); err != nil {
		t.Fatalf("record budget stop observation: %v", err)
	}
}
