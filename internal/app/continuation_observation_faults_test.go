package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// A concrete evidence or control fault is the authoritative collect error. The
// automatic continuation offer is optional observation and must never ride
// along with it.
func TestCollectConcreteFaultCarriesNoAutomaticContinuation(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
	// Paired control: the unfaulted interruption still offers the continuation.
	requireInterruptedEnvelope(t, fixture, rebootCollect(t, fixture), "observed", "continue_task")

	stdout := filepath.Join(fixture.td.Dir, "raw", "stdout")
	if err := os.Remove(stdout); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stdout, 0o700); err != nil {
		t.Fatal(err)
	}
	collected := rebootCollect(t, fixture)
	if collected.code == 0 || !errors.Is(collected.err, task.ErrEvidenceFault) {
		t.Fatalf("collect with faulted raw evidence = code %d err=%v, want %v", collected.code, collected.err, task.ErrEvidenceFault)
	}
	response := collected.response
	if response.Continuation != nil || response.Status == "interrupted" || (response.Failure != nil && response.Failure.NextAction == "continue_task") {
		t.Fatalf("faulted collect advertised continuation: status=%q continuation=%+v failure=%+v", response.Status, response.Continuation, response.Failure)
	}
}

// runRebootStatusJSON runs the public `status --json` command with the
// production lifecycle observer over the reboot fixture.
func runRebootStatusJSON(t *testing.T, fixture *rebootFixture) (int, Response) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"status", fixture.req.TaskID, "--root", fixture.root, "--json"}, &stdout, &stderr, rebootDependencies(t, fixture))
	return code, decodeHistoricalResponse(t, stdout.Bytes(), stderr.Bytes(), code)
}

// A concrete fault found after the supervisor was prepared is status's
// authoritative result, exactly like collect's. The saved supervisor receipt is
// read only after preparation, so a malformed one is a concrete final-status
// fault. The optional continuation offer, and the guidance derived from it,
// must not ride along with the failed exit.
func TestStatusConcreteFaultCarriesNoAutomaticContinuation(t *testing.T) {
	t.Parallel()
	malformed := []byte("{")
	var decodeProbe struct{}
	wantDiagnostic := json.Unmarshal(malformed, &decodeProbe)
	if wantDiagnostic == nil {
		t.Fatal("fixture bytes are not malformed JSON")
	}
	for _, tc := range []struct {
		name       string
		prepare    func(*testing.T, *rebootFixture)
		wantStatus string
	}{
		{name: "interrupted", wantStatus: "interrupted", prepare: func(*testing.T, *rebootFixture) {}},
		{name: "budget timeout", wantStatus: "timed_out", prepare: func(t *testing.T, fixture *rebootFixture) {
			recordRebootStop(t, fixture, "budget", "budget")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
			tc.prepare(t, fixture)
			// Paired control: the unfaulted observation offers the continuation.
			code, control := runRebootStatusJSON(t, fixture)
			if code != 0 || control.Status != tc.wantStatus || control.Continuation == nil || !control.Continuation.Resumable {
				t.Fatalf("unfaulted status = code %d %+v, want exit 0 %s with a resumable offer", code, control, tc.wantStatus)
			}

			writeRebootTaskFile(t, fixture, "supervisor.ref.json", malformed)
			if err := os.WriteFile(fixture.commandLog, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			code, response := runRebootStatusJSON(t, fixture)
			commands, err := os.ReadFile(fixture.commandLog)
			if err != nil || !strings.Contains(string(commands), "status") {
				t.Fatalf("supervisor preparation did not run before the fault: commands=%q err=%v", commands, err)
			}
			requireFaultedStatusResponse(t, code, response, wantDiagnostic.Error())
		})
	}
}

// requireFaultedStatusResponse checks a concrete status fault: exit 1 with its
// diagnostics retained, and no automatic guidance, outcome, or payload.
func requireFaultedStatusResponse(t *testing.T, code int, response Response, diagnostics ...string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if code != 1 || !strings.Contains(response.Error, diagnostic) {
			t.Fatalf("faulted status = code %d error=%q, want exit 1 retaining %q", code, response.Error, diagnostic)
		}
	}
	failure := response.Failure
	if response.Continuation != nil || response.Status == "interrupted" || response.Status == "timed_out" || response.Outcome != nil || response.Payload != nil || (failure != nil && (failure.Code == "task_interrupted" || failure.NextAction == "continue_task")) {
		t.Fatalf("faulted status advertised automatic guidance or evidence: status=%q continuation=%+v failure=%+v outcome=%+v payload=%+v", response.Status, response.Continuation, failure, response.Outcome, response.Payload)
	}
}

// lateStatusFault records every supervisor command and, once armed, corrupts
// the queue status payload at the entry of the next `status --json`, before the
// real command starts. The fault therefore lands after supervisor preparation.
type lateStatusFault struct {
	path    string
	payload []byte

	mu      sync.Mutex
	armed   bool
	entries [][]string
	exits   []int
	hookErr error
}

func (f *lateStatusFault) observe(event pueue.CommandEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch event.Stage {
	case "entry":
		f.entries = append(f.entries, slices.Clone(event.Argv))
		if f.armed && historicalCommandSequence(event.Argv, "status", "--json") {
			f.hookErr = errors.Join(f.hookErr, os.WriteFile(f.path, f.payload, 0o600))
			f.armed = false
		}
	case "completed":
		f.exits = append(f.exits, event.ExitCode)
	}
}

func (f *lateStatusFault) reset(armed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.entries, f.exits = armed, nil, nil
}

func (f *lateStatusFault) snapshot() (entries [][]string, exits []int, hookErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.entries), slices.Clone(f.exits), f.hookErr
}

func countStatusJSON(entries [][]string) int {
	count := 0
	for _, argv := range entries {
		if historicalCommandSequence(argv, "status", "--json") {
			count++
		}
	}
	return count
}

// The queue status read itself fails after preparation succeeded: the real
// status command exits zero with a malformed payload. Preparation never reads
// status here, so this is the actual final-status fault. It joins
// ErrStatusOperational with ErrUnknown, so it is not pure uncertainty.
func TestStatusMalformedFinalStatusCarriesNoAutomaticContinuation(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
	requireRebootClosedPreconditions(t, fixture)
	listenSupervisorProbeTestEndpoint(t, fixture.predecessor.Endpoint)
	healthy, err := os.ReadFile(fixture.statusPath)
	if err != nil {
		t.Fatal(err)
	}
	fault := &lateStatusFault{path: fixture.statusPath, payload: []byte("{")}
	fixture.options.Observer = fault.observe
	t.Cleanup(func() {
		if restoreErr := os.WriteFile(fixture.statusPath, healthy, 0o600); restoreErr != nil {
			t.Error(restoreErr)
		}
	})

	// Paired control: the healthy queue read offers the continuation.
	code, control := runRebootStatusJSON(t, fixture)
	entries, _, hookErr := fault.snapshot()
	if code != 0 || control.Status != "interrupted" || control.Continuation == nil || !control.Continuation.Resumable || countStatusJSON(entries) != 1 || hookErr != nil {
		t.Fatalf("healthy status = code %d %+v status commands=%d hook=%v, want exit 0 interrupted with one status --json", code, control, countStatusJSON(entries), hookErr)
	}

	fault.reset(true)
	code, response := runRebootStatusJSON(t, fixture)
	fault.requireOneCompletedStatus(t)
	requireFaultedStatusResponse(t, code, response, pueue.ErrUnknown.Error(), pueue.ErrStatusOperational.Error())
}

// requireOneCompletedStatus proves the fault was injected before exactly one
// real status command, which completed with exit zero, and that observation
// stayed read-only.
func (f *lateStatusFault) requireOneCompletedStatus(t *testing.T) {
	t.Helper()
	entries, exits, hookErr := f.snapshot()
	if hookErr != nil {
		t.Fatalf("late status fault was not injected: %v", hookErr)
	}
	if countStatusJSON(entries) != 1 || !slices.Equal(exits, []int{0}) {
		t.Fatalf("status commands = %q exits=%v, want exactly one status --json that completed with exit 0", entries, exits)
	}
	assertObservationPhaseNoMutation(t, entries)
}

// Pure queue uncertainty keeps its historical exit-zero contract: the
// diagnostic is retained and the continuation offer is still observed.
func TestStatusPureUncertaintyKeepsInterruptionOffer(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
	// An empty queue is a successful read that no longer lists the task.
	if err := os.WriteFile(fixture.statusPath, []byte(`{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, response := runRebootStatusJSON(t, fixture)
	if code != 0 || !strings.Contains(response.Error, pueue.ErrUnknown.Error()) {
		t.Fatalf("uncertain status = code %d error=%q, want exit 0 retaining %q", code, response.Error, pueue.ErrUnknown)
	}
	failure := response.Failure
	if response.Status != "interrupted" || response.Continuation == nil || !response.Continuation.Resumable || failure == nil || failure.Code != "task_interrupted" || failure.NextAction != "continue_task" {
		t.Fatalf("uncertain status dropped the interruption offer: status=%q continuation=%+v failure=%+v", response.Status, response.Continuation, failure)
	}
}

func TestSimultaneousUserAndBudgetStopReportsExplicitCancellation(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
	recordRebootStop(t, fixture, strings.Repeat("c", 32), "user")
	recordRebootStop(t, fixture, "budget", "budget")
	// Cancellation changes guidance only: status still succeeds and collect still
	// ends as a finite pending observation.
	for _, tc := range []struct {
		result   commandResult
		wantCode int
	}{{rebootStatus(t, fixture), 0}, {rebootCollect(t, fixture), 3}} {
		result, response := tc.result, tc.result.response
		if result.code != tc.wantCode || result.err != nil {
			t.Fatalf("%s cancelled timeout exit = %d err=%v, want %d", response.Command, result.code, result.err, tc.wantCode)
		}
		if response.Status != "timed_out" || response.Continuation == nil || response.Continuation.Resumable {
			t.Fatalf("%s cancelled timeout = %+v", response.Command, response)
		}
		failure := response.Failure
		if failure == nil || failure.Code != "task_cancelled" || failure.Stage != response.Command || failure.NextAction != "stop_and_report" {
			t.Fatalf("%s failure = %+v, want task_cancelled/stop_and_report", response.Command, failure)
		}
	}
}

// A terminated budget stop uses the same canonical session resolver as an
// interruption: recorded input survives the stop record, and only a genuinely
// absent session is an unavailable offer.
func TestBudgetTimeoutKeepsRecordedInputSession(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
	requireInterruptedExitCodes(t, fixture, "recorded_input", "continue_task")
	recordRebootStop(t, fixture, "budget", "budget")
	observed := rebootStatus(t, fixture)
	continuation := observed.response.Continuation
	if observed.code != 0 || observed.response.Status != "timed_out" || continuation == nil || !continuation.Resumable || continuation.SessionSource != "recorded_input" || continuation.ContinueCommand == "" {
		t.Fatalf("budget timeout with recorded input = code %d %+v continuation=%+v", observed.code, observed.response, continuation)
	}
}

func TestBudgetTimeoutWithoutAnySessionIsUnavailable(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
	recordRebootStop(t, fixture, "budget", "budget")
	observed := rebootStatus(t, fixture)
	continuation := observed.response.Continuation
	if observed.code != 0 || observed.err != nil || observed.response.Status != "timed_out" || continuation == nil || continuation.Resumable || continuation.SessionSource != "" || continuation.Reason != "provider session identity is unavailable" {
		t.Fatalf("budget timeout without session = code %d err=%v %+v", observed.code, observed.err, observed.response)
	}
}

func TestBudgetTimeoutFailsClosedOnInvalidSessionEvidence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		damage func(*testing.T, *rebootFixture)
	}{
		{name: "malformed current reference", damage: func(t *testing.T, fixture *rebootFixture) {
			writeRebootTaskFile(t, fixture, "provider.ref.json", []byte("{"))
		}},
		{name: "mismatched current reference", damage: writeMismatchedRebootIdentity},
		{name: "mismatched predecessor", damage: func(t *testing.T, fixture *rebootFixture) {
			fixture.req.TaskID = strings.Repeat("e", 32)
		}},
		{name: "missing prepared metadata", damage: func(t *testing.T, fixture *rebootFixture) {
			if err := os.Remove(filepath.Join(fixture.td.Dir, "meta.json")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
			recordRebootStop(t, fixture, "budget", "budget")
			tc.damage(t, fixture)
			offer, err := tasklifecycle.ObserveTimeoutContinuation(fixture.root, fixture.td, &fixture.req, &fixture.meta, NativeCatalog())
			if err == nil || offer != nil {
				t.Fatalf("invalid session evidence produced offer=%+v err=%v, want closed refusal", offer, err)
			}
		})
	}
}

func TestCancellationProjectionNeverOverridesOutcome(t *testing.T) {
	id := strings.Repeat("a", 32)
	cancelled := func(cause tasklifecycle.ContinuationCause) *tasklifecycle.ContinuationOffer {
		return &tasklifecycle.ContinuationOffer{Cause: cause, PredecessorID: id, Mode: "native", Reason: "task was cancelled by the user", Cancelled: true}
	}
	cases := []struct {
		name     string
		cause    tasklifecycle.ContinuationCause
		outcome  *task.OutcomeRecord
		wantCode string
	}{
		{name: "timed out without outcome", cause: tasklifecycle.ContinuationTimedOut, wantCode: "task_cancelled"},
		{name: "interrupted without outcome", cause: tasklifecycle.ContinuationInterrupted, wantCode: "task_interrupted"},
		{name: "timed out over committed outcome", cause: tasklifecycle.ContinuationTimedOut, outcome: &task.OutcomeRecord{Verdict: task.VerdictCommitted}},
		{name: "timed out over rejected outcome", cause: tasklifecycle.ContinuationTimedOut, outcome: &task.OutcomeRecord{Verdict: task.VerdictRejected}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := Response{Command: "collect", Outcome: tc.outcome}
			applyContinuationOffer(&response, "/tmp/state", cancelled(tc.cause))
			if tc.wantCode == "" {
				if response.Failure != nil {
					t.Fatalf("cancellation replaced authoritative outcome with failure %+v", response.Failure)
				}
				return
			}
			failure := response.Failure
			if failure == nil || failure.Code != tc.wantCode || failure.Stage != "collect" || failure.NextAction != "stop_and_report" || response.Status != string(tc.cause) {
				t.Fatalf("status=%q failure=%+v, want %s/stop_and_report", response.Status, failure, tc.wantCode)
			}
		})
	}
}

// The pure timeout policy carries the typed session source through its
// boundary instead of collapsing it to "observed".
func TestTimeoutContinuationPreservesTypedSessionSource(t *testing.T) {
	records := []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: "budget"}, Observation: &task.StopObservedRecord{Terminated: true}}}
	request := &task.TaskRecord{TaskID: strings.Repeat("a", 32), Provider: "pi:json"}
	for _, source := range []tasklifecycle.ContinuationSessionSource{tasklifecycle.ContinuationSessionObserved, tasklifecycle.ContinuationSessionRecordedInput} {
		offer := tasklifecycle.TimeoutContinuation(request, records, NativeCatalog(), source, "/usr/bin/delegate-run", "/usr/bin/delegate-run", nil, true, true)
		if offer == nil || !offer.Resumable || offer.SessionSource != source {
			t.Fatalf("offer for %q = %+v, want resumable with the same source", source, offer)
		}
	}
	if offer := tasklifecycle.TimeoutContinuation(request, records, NativeCatalog(), "", "/usr/bin/delegate-run", "/usr/bin/delegate-run", nil, true, true); offer == nil || offer.Resumable || offer.SessionSource != "" {
		t.Fatalf("offer without a session = %+v, want unavailable", offer)
	}
}

// A user stop may suppress only a derived offer. When the session evidence
// refuses the offer, a corrupt stop request must not mask that authoritative
// identity refusal; an eligible offer still reports the unreadable stop.
func TestCorruptCancellationNeverMasksSessionRefusal(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mismatched   bool
		wantIdentity bool
	}{
		{name: "eligible offer reports unreadable cancellation"},
		{name: "mismatched session stays authoritative", mismatched: true, wantIdentity: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
			id := strings.Repeat("c", 32)
			recordRebootStop(t, fixture, id, "user")
			records, err := tasklifecycle.ReadTimeoutRecords(fixture.td)
			if err != nil || len(records) != 1 {
				t.Fatalf("user stop records = %+v err=%v", records, err)
			}
			if tc.mismatched {
				writeMismatchedRebootIdentity(t, fixture)
			}
			if err = os.WriteFile(filepath.Join(fixture.td.Dir, "stop", id+".request.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, readErr := fixture.td.UserStopRequestedContext(t.Context()); readErr == nil {
				t.Fatal("fixture stop request is still readable")
			}
			offer, err := tasklifecycle.ObserveTimeoutContinuationWithRecords(fixture.root, fixture.td, &fixture.req, &fixture.meta, records, NativeCatalog())
			if offer != nil || err == nil || errors.Is(err, task.ErrIdentityMismatch) != tc.wantIdentity {
				t.Fatalf("offer=%+v err=%v, want no offer and identity mismatch=%t", offer, err, tc.wantIdentity)
			}
		})
	}
}
