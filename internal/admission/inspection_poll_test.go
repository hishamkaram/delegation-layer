package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInspectionPollScheduleBoundsFullQueueReads(t *testing.T) {
	for _, tc := range []struct {
		name           string
		completionTick int
		wantPolls      int
	}{
		{name: "no completion", completionTick: -1, wantPolls: 20},
		{name: "completion before first observation", completionTick: 0, wantPolls: 20},
		{name: "completion between watchdogs", completionTick: 2, wantPolls: 21},
		{name: "completion at watchdog", completionTick: 5, wantPolls: 20},
		{name: "completion just before deadline", completionTick: 99, wantPolls: 21},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var schedule inspectionPollSchedule
			start := time.Unix(100, 0)
			polls := 0
			// One hundred local-proof checks fit inside the unchanged 20s
			// admission window. Persistent completion adds at most one read.
			for tick := range 100 {
				completed := tc.completionTick >= 0 && tick >= tc.completionTick
				if schedule.due(start.Add(time.Duration(tick)*200*time.Millisecond), completed) {
					polls++
				}
			}
			if polls != tc.wantPolls {
				t.Fatalf("100 local checks caused %d full-queue reads, want %d", polls, tc.wantPolls)
			}
		})
	}
}

func TestInspectionPollScheduleCompletionPromptsOnlyOnce(t *testing.T) {
	var schedule inspectionPollSchedule
	start := time.Unix(100, 0)
	var observations []time.Duration
	for tick := range 13 {
		elapsed := time.Duration(tick) * 200 * time.Millisecond
		// Losing and rereading the hint cannot repeatedly bypass the watchdog.
		completed := tick >= 2 && tick != 3
		if schedule.due(start.Add(elapsed), completed) {
			observations = append(observations, elapsed)
		}
	}
	want := []time.Duration{0, 400 * time.Millisecond, 1400 * time.Millisecond, 2400 * time.Millisecond}
	if !slices.Equal(observations, want) {
		t.Fatalf("completion prompt/watchdog observations=%v want=%v", observations, want)
	}
}

func TestInspectionPollSchedulePrefersRegisteredTerminalCallback(t *testing.T) {
	schedule := inspectionPollSchedule{callbackExpected: true}
	start := time.Unix(100, 0)
	var observations []time.Duration
	for tick := range 51 {
		elapsed := time.Duration(tick) * 200 * time.Millisecond
		if schedule.due(start.Add(elapsed), tick >= 2) {
			observations = append(observations, elapsed)
		}
	}
	want := []time.Duration{0, 5 * time.Second, 10 * time.Second}
	if !slices.Equal(observations, want) {
		t.Fatalf("callback watchdog observations=%v want=%v", observations, want)
	}
}

func TestInspectionPollScheduleDoesNotCatchUpMissedWatchdogs(t *testing.T) {
	var schedule inspectionPollSchedule
	start := time.Unix(100, 0)
	for _, step := range []struct {
		elapsed time.Duration
		want    bool
	}{
		{elapsed: 0, want: true},
		{elapsed: 3400 * time.Millisecond, want: true},
		{elapsed: 3400 * time.Millisecond},
		{elapsed: 4399 * time.Millisecond},
		{elapsed: 4400 * time.Millisecond, want: true},
	} {
		if got := schedule.due(start.Add(step.elapsed), false); got != step.want {
			t.Fatalf("observation due at %s=%t want=%t", step.elapsed, got, step.want)
		}
	}
}

func TestReconcileInspectionStashedRaceCommitsAdvancedObservation(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, bound)
	claim, err := operation.ClaimSubmission(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = claim.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	admissionfixture.WriteInspectionPollTestState(t, statusPath, operation, map[string]any{
		"Stashed": map[string]any{"enqueue_at": nil},
	})
	client := newInspectionPollTestClient(t, bound, logPath, stagedInspectionRaceObserver(t, operation, statusPath))
	commits := 0
	observation, err := reconcileInspectionObservation(
		t.Context(),
		operation,
		client,
		operation.Request().SupervisorIdentity(),
		countRunningInspectionCommit(t, &commits),
	)
	if err != nil || !observation.Matched || observation.Job.State != pueue.StateRunning || commits != 1 {
		t.Fatalf("staged race lost advanced observation: observation=%+v commits=%d err=%v", observation, commits, err)
	}
	receipt, err := operation.ReceiptContext(t.Context())
	if err != nil || receipt.NumericTaskID == nil || *receipt.NumericTaskID != 7 {
		t.Fatalf("staged race lost durable receipt: receipt=%+v err=%v", receipt, err)
	}
}

func TestAcceptInspectionObservationRefusesMissingDurableReceipt(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, _, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, bound)
	identity := operation.Request().SupervisorIdentity()
	label := identity.Label()
	job := pueue.Job{ID: 7, Label: &label, Group: operation.Request().Group, State: pueue.StateRunning}

	facts, ended, err := acceptInspectionObservation(t.Context(), operation, nil, &identity, job)
	if facts != nil || ended || !errors.Is(err, task.ErrEvidenceFault) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("queue-only inspection acceptance = facts=%s ended=%t err=%v", facts, ended, err)
	}
	if identity.NumericTaskID != nil {
		t.Fatalf("queue-only inspection mutated identity: %+v", identity)
	}
	if _, err = operation.ReceiptContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("queue observation created inspection receipt: %v", err)
	}
}

func stagedInspectionRaceObserver(t *testing.T, operation *inspection.Operation, statusPath string) func(pueue.CommandEvent) {
	t.Helper()
	var once sync.Once
	return func(event pueue.CommandEvent) {
		if event.Stage != "completed" || !slices.Contains(event.Argv, "status") ||
			!slices.Contains(event.Argv, "label="+operation.Request().SupervisorIdentity().Label()) {
			return
		}
		once.Do(func() {
			if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
				t.Error(err)
				return
			}
			admissionfixture.WriteInspectionPollTestStatus(t, statusPath, operation, false)
		})
	}
}

func countRunningInspectionCommit(t *testing.T, calls *int) pueue.InspectionObservationCommit {
	t.Helper()
	return func(candidate pueue.InspectionObservation) error {
		(*calls)++
		if candidate.Job.ID != 7 || candidate.Job.State != pueue.StateRunning {
			t.Fatalf("advanced commit received %+v", candidate)
		}
		return nil
	}
}

func TestAwaitInspectionPollCompletionDoesNotAdmitRunningWorker(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, bound)
	completeInspectionPollTestOperation(t, operation)
	admissionfixture.WriteInspectionPollTestStatus(t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, false)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()
	var polls atomic.Int32
	client := newInspectionPollTestClient(t, bound, logPath, func(event pueue.CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "status") && polls.Add(1) == 2 {
			cancel()
		}
	})
	facts, awaitErr := awaitInspectionWithSubmissionState(ctx, operation, client, inspectionSubmissionState{})
	if facts != nil || !errors.Is(awaitErr, inspection.ErrAdmissionExpired) || polls.Load() != 2 {
		t.Fatalf("local completion admitted a running worker: facts=%s polls=%d err=%v", facts, polls.Load(), awaitErr)
	}
	receipt, receiptErr := operation.ReceiptContext(t.Context())
	if receiptErr != nil || receipt.NumericTaskID == nil || *receipt.NumericTaskID != 7 {
		t.Fatalf("running fixture was not independently matched: receipt=%+v err=%v", receipt, receiptErr)
	}
	if _, proofErr := operation.ReadProofContext(t.Context()); !errors.Is(proofErr, os.ErrNotExist) {
		t.Fatalf("running worker obtained independent exit proof: %v", proofErr)
	}
}

func TestAwaitInspectionPollWatchdogObservesFailureWithoutCompletion(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, bound)
	if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	failedPath := filepath.Join(filepath.Dir(logPath), "failed-status.json")
	admissionfixture.WriteInspectionPollTestStatus(t, statusPath, operation, false)
	admissionfixture.WriteInspectionPollTestStatus(t, failedPath, operation, true)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()
	var polls atomic.Int32
	published := make(chan error, 1)
	client := newInspectionPollTestClient(t, bound, logPath, func(event pueue.CommandEvent) {
		if event.Stage != "completed" || !slices.Contains(event.Argv, "status") || polls.Add(1) != 1 {
			return
		}
		// Wait and capture have finished: the first observation stays running,
		// and the next watchdog sees failure without any local completion.
		renameErr := os.Rename(failedPath, statusPath)
		published <- renameErr
		if renameErr != nil {
			cancel()
		}
	})
	facts, awaitErr := awaitInspectionWithSubmissionState(ctx, operation, client, inspectionSubmissionState{})
	select {
	case publishErr := <-published:
		if publishErr != nil {
			t.Fatal(publishErr)
		}
	default:
		t.Fatal("fixture never observed its initial running worker")
	}
	if facts != nil || !errors.Is(awaitErr, ErrInspectionWorkerUnavailable) || polls.Load() != 2 {
		t.Fatalf("watchdog lost failed worker without completion: facts=%s polls=%d err=%v", facts, polls.Load(), awaitErr)
	}
	if completed, completionErr := inspectionCompletionHint(t.Context(), operation); completed || completionErr != nil {
		t.Fatalf("missing completion became a scheduling error or authority: completed=%t err=%v", completed, completionErr)
	}
}

func TestAwaitInspectionPollReadsRecordedProofBeforeQueue(t *testing.T) {
	_, operation, job := admissionfixture.NewCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if recordErr := operation.RecordWorkerSuccessContext(t.Context(), job.ID); recordErr != nil {
		t.Fatal(recordErr)
	}
	// A queue read would dereference the nil supervisor. Only the independently
	// recorded exit, combined with the local completion, can finish admission.
	facts, awaitErr := awaitInspectionWithSubmissionState(t.Context(), operation, nil, inspectionSubmissionState{})
	if awaitErr != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("saved proof required another queue read: facts=%s err=%v", facts, awaitErr)
	}
}

func TestAwaitCompletedInspectionObservationPublishesTimelyExitProof(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, bound)
	completeInspectionPollTestOperation(t, operation)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	writeInspectionPollTestTerminalStatus(
		t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, true,
	)
	client := newInspectionPollTestClient(t, bound, logPath, nil)

	facts, err := awaitCompletedInspectionObservation(t.Context(), operation, client, inspection.ErrAdmissionExpired)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("timely completion was not published during observation grace: facts=%s err=%v", facts, err)
	}
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if err != nil || !succeeded || result.Reason != inspection.ResultEligible {
		t.Fatalf("observation grace did not persist worker success: result=%+v succeeded=%t err=%v", result, succeeded, err)
	}
}

func TestAwaitCompletedInspectionObservationStartsIndependentControlWindow(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	createdAt := time.Now().UTC().Add(-inspection.AdmissionTimeout - pueue.DefaultObservationTimeout - time.Second)
	operation := admissionfixture.NewInspectionPollTestOperationAt(t, store, bound, createdAt)
	completeInspectionPollTestOperation(t, operation)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	rewriteInspectionPollTestTerminationTime(t, store, operation, createdAt.Add(4*time.Millisecond))
	writeInspectionPollTestTerminalStatus(
		t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, true,
	)
	client := newInspectionPollTestClient(t, bound, logPath, nil)

	facts, err := awaitCompletedInspectionObservation(t.Context(), operation, client, inspection.ErrAdmissionExpired)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("expired admission clock shortened independent control observation: facts=%s err=%v", facts, err)
	}
}

func TestAcceptedTerminalObservationLaunchesMaintenanceAfterCallerExpiry(t *testing.T) {
	store, operation, bound, job := admissionfixture.NewRecordedTerminalCleanupFixture(t, TerminalEvidence{})
	probe := newPrivateMaintenanceLaunchProbe(t)
	client := newPrivateMaintenanceLaunchClient(t, bound, probe.launch)
	observation := pueue.InspectionObservation{
		Binding:  client.Binding(),
		Identity: operation.Request().SupervisorIdentity(),
		Job:      job,
		Matched:  true,
	}
	facts, terminal, acceptErr := acceptCompletedInspectionObservation(probe.callerContext, operation, client, observation)
	if acceptErr != nil || !terminal || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("accepted terminal observation facts=%s terminal=%t err=%v", facts, terminal, acceptErr)
	}
	probe.assert(t, store.Root)
}

type privateMaintenanceLaunchObservation struct {
	callerErr  error
	launchErr  error
	executable string
	root       string
	requested  uint64
	err        error
}

type privateMaintenanceLaunchProbe struct {
	callerContext context.Context
	cancelCaller  context.CancelFunc
	launched      chan privateMaintenanceLaunchObservation
}

func newPrivateMaintenanceLaunchProbe(t *testing.T) *privateMaintenanceLaunchProbe {
	t.Helper()
	callerContext, cancelCaller := context.WithCancel(t.Context())
	t.Cleanup(cancelCaller)
	return &privateMaintenanceLaunchProbe{
		callerContext: callerContext,
		cancelCaller:  cancelCaller,
		launched:      make(chan privateMaintenanceLaunchObservation, 1),
	}
}

func newPrivateMaintenanceLaunchClient(t *testing.T, bound *pueue.Client, launcher func(context.Context, string, string, []string) error) *pueue.Client {
	t.Helper()
	client, err := pueue.NewClient(bound.Binding(), pueue.Options{
		ObservationTimeout:          pueue.DefaultObservationTimeout,
		Environment:                 os.Environ(),
		SuccessfulCleanupAuthorizer: newInspectionTestCleanup().AuthorizeSuccessfulCleanup,
		PrivateMaintenanceLauncher:  launcher,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client
}

func (probe *privateMaintenanceLaunchProbe) launch(launchContext context.Context, executable, root string, _ []string) error {
	requested, readErr := readPrivateMaintenanceRequested(root)
	probe.cancelCaller()
	probe.launched <- privateMaintenanceLaunchObservation{
		callerErr:  probe.callerContext.Err(),
		launchErr:  launchContext.Err(),
		executable: executable,
		root:       root,
		requested:  requested,
		err:        readErr,
	}
	return readErr
}

func readPrivateMaintenanceRequested(root string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(root, ".supervisor", "queue-maintenance.json"))
	if err != nil {
		return 0, err
	}
	var record struct {
		Requested uint64 `json:"requested"`
	}
	if err = json.Unmarshal(data, &record); err != nil {
		return 0, err
	}
	return record.Requested, nil
}

func (probe *privateMaintenanceLaunchProbe) assert(t *testing.T, wantRoot string) {
	t.Helper()
	select {
	case launch := <-probe.launched:
		if launch.err != nil {
			t.Fatal(launch.err)
		}
		if !errors.Is(launch.callerErr, context.Canceled) {
			t.Fatalf("publication caller context remained live at handoff: %v", launch.callerErr)
		}
		if launch.launchErr != nil {
			t.Fatalf("fresh maintenance launch context was already expired: %v", launch.launchErr)
		}
		if launch.root != wantRoot || launch.executable != "" {
			t.Fatalf("maintenance launch target executable=%q root=%q", launch.executable, launch.root)
		}
		if launch.requested == 0 {
			t.Fatal("maintenance launched before its generation was durable")
		}
	default:
		t.Fatal("accepted terminal observation did not hand off maintenance")
	}
}

func TestAwaitCompletedInspectionObservationConsumesPendingStatusResult(t *testing.T) {
	gate := newHeldStatusGate()
	fixture := newPendingStatusResultFixture(t, gate)
	pending := admissionfixture.AssertRetainedInspectionPending(t, fixture.observeErr)
	if fixture.observation.Pending != pending {
		gate.release()
		t.Fatalf("exact observation did not expose its retained result: observation=%+v err=%v", fixture.observation, fixture.observeErr)
	}
	gate.release()
	facts, err := awaitCompletedInspectionObservation(t.Context(),
		fixture.operation, fixture.client, errors.Join(inspection.ErrAdmissionExpired, fixture.observeErr),
	)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("pending status result was not accepted: facts=%s err=%v", facts, err)
	}
	assertInspectionStatusCommandCount(t, fixture.logPath, 1)
}

type pendingStatusResultFixture struct {
	operation   *inspection.Operation
	client      *pueue.Client
	logPath     string
	observation pueue.InspectionObservation
	observeErr  error
}

// newPendingStatusResultFixture returns an exact status observation whose
// caller expired while the completed status command is held by gate.
func newPendingStatusResultFixture(t *testing.T, gate *heldStatusGate) pendingStatusResultFixture {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	ctx := newInspectionDeadlineContext(t.Context())
	client := newHeldStatusClient(t, bound, logPath, gate, func(event pueue.CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "status") {
			ctx.Expire()
		}
		gate.observe(event)
	})
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, client)
	completeInspectionPollTestOperation(t, operation)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	writeInspectionPollTestTerminalStatus(
		t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, true,
	)

	identity := operation.Request().SupervisorIdentity()
	observation, observeErr := client.ReconcileInspection(ctx, identity)
	return pendingStatusResultFixture{
		operation: operation, client: client, logPath: logPath,
		observation: observation, observeErr: observeErr,
	}
}

func TestObserveCompletedInspectionRetainsGraceProbeAfterCallerExpiry(t *testing.T) {
	gate := newHeldStatusGate()
	fixture := newGraceProbeFixture(t, gate)
	// Arm the finite caller deadline only after every fixture is published, so
	// slow setup cannot consume the window this probe exercises.
	ctx, cancel := admissionfixture.NewControllableDeadlineContext(t.Context(), 10*time.Second)
	done := fixture.startGraceProbe(t, ctx, cancel)
	if err := gate.awaitEntered(done, 5*time.Second); err != nil {
		t.Fatalf("%v; caller context: %v", err, ctx.Err())
	}
	ctx.Expire()
	got := awaitGraceProbeReturn(t, done)
	pending := admissionfixture.AssertRetainedInspectionPending(t, got.err)
	if got.facts != nil || !errors.Is(got.err, context.DeadlineExceeded) || !errors.Is(got.err, pueue.ErrInFlight) {
		t.Fatalf("expired grace dropped its in-flight supervisor probe: facts=%s err=%v", got.facts, got.err)
	}
	gate.release()
	admissionfixture.DrainInspectionPending(t, pending)
	assertInspectionStatusCommandCount(t, fixture.logPath, 1)
}

func TestGraceProbeHandshakeReportsEarlyProbeReturn(t *testing.T) {
	gate := newHeldStatusGate()
	fixture := newGraceProbeFixture(t, gate)
	ctx, cancel := admissionfixture.NewControllableDeadlineContext(t.Context(), 10*time.Second)
	// A caller deadline spent before observation starts must surface as the
	// probe's own error, not as a watchdog miss that hides it.
	ctx.Expire()
	done := fixture.startGraceProbe(t, ctx, cancel)
	if err := gate.awaitEntered(done, 5*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handshake hid the probe's early return: %v", err)
	}
}

func TestHeldProbeCleanupHoldsResourcesUntilProbeExits(t *testing.T) {
	owner := newCleanupRecorder()
	gate := newHeldStatusGate()
	var probeReturned atomic.Bool
	resourceSawProbeReturned := make(chan bool, 1)
	// Registered before the probe, so it runs after the probe cleanup, like the
	// client and journal fixture cleanups.
	owner.Cleanup(func() { resourceSawProbeReturned <- probeReturned.Load() })
	held := make(chan struct{})
	releaseProbe := sync.OnceFunc(func() { close(held) })
	cancelled := make(chan struct{})
	startHeldProbe(owner, gate, sync.OnceFunc(func() { close(cancelled) }), func() (json.RawMessage, error) {
		// Ignores cancellation and the status gate, forcing the join watchdog.
		<-held
		probeReturned.Store(true)
		return nil, context.Canceled
	})
	cleaned := make(chan struct{})
	go func() {
		defer close(cleaned)
		owner.runCleanups()
	}()
	t.Cleanup(func() {
		releaseProbe()
		<-cleaned
	})

	reportTimer := time.NewTimer(2 * heldProbeJoinWatchdog)
	defer reportTimer.Stop()
	select {
	case <-owner.reported:
	case <-reportTimer.C:
		t.Fatal("probe cleanup never reported its join watchdog")
	}
	// Green ordering is proven by probeReturned below; this window only gives
	// an unjoined cleanup the chance to advance while the probe is still held.
	settle := time.NewTimer(time.Second)
	defer settle.Stop()
	select {
	case sawReturned := <-resourceSawProbeReturned:
		t.Fatalf("resource cleanup advanced while the probe was still held: probe returned=%t", sawReturned)
	case <-settle.C:
	}
	assertClosed(t, cancelled, "probe cleanup reached its watchdog without cancelling the caller")
	assertClosed(t, gate.released, "probe cleanup did not release the held status after its watchdog")
	releaseProbe()
	joinTimer := time.NewTimer(heldProbeJoinWatchdog)
	defer joinTimer.Stop()
	select {
	case sawReturned := <-resourceSawProbeReturned:
		if !sawReturned {
			t.Fatal("resource cleanup ran before the probe exited")
		}
	case <-joinTimer.C:
		t.Fatal("resource cleanup did not advance after the probe exited")
	}
	if errs := owner.reportedErrors(); len(errs) != 1 || !strings.Contains(errs[0], "probe did not return") {
		t.Fatalf("join watchdog diagnostics=%q", errs)
	}
}

func assertClosed(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	default:
		t.Fatal(message)
	}
}

// cleanupRecorder implements heldProbeOwner. The test runs the recorded
// cleanups itself in testing's LIFO order and observes reported errors.
type cleanupRecorder struct {
	reported chan struct{}
	report   func()

	mu       sync.Mutex
	cleanups []func()
	errors   []string
}

func newCleanupRecorder() *cleanupRecorder {
	recorder := &cleanupRecorder{reported: make(chan struct{})}
	recorder.report = sync.OnceFunc(func() { close(recorder.reported) })
	return recorder
}

func (recorder *cleanupRecorder) Helper() {}

func (recorder *cleanupRecorder) Cleanup(cleanup func()) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.cleanups = append(recorder.cleanups, cleanup)
}

func (recorder *cleanupRecorder) Error(args ...any) {
	recorder.mu.Lock()
	recorder.errors = append(recorder.errors, fmt.Sprint(args...))
	recorder.mu.Unlock()
	recorder.report()
}

func (recorder *cleanupRecorder) runCleanups() {
	recorder.mu.Lock()
	cleanups := slices.Clone(recorder.cleanups)
	recorder.mu.Unlock()
	for _, cleanup := range slices.Backward(cleanups) {
		cleanup()
	}
}

func (recorder *cleanupRecorder) reportedErrors() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return slices.Clone(recorder.errors)
}

func TestHeldStatusCleanupReleasesBeforeClientClose(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold func(*testing.T, *heldStatusGate)
	}{
		{name: "grace probe", hold: holdGraceProbeStatus},
		{name: "pending status result", hold: holdPendingStatusResult},
		{name: "await poll caller deadline", hold: holdAwaitPollStatus},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertHeldStatusCleanupIsFinite(t, tc.hold)
		})
	}
}

// assertHeldStatusCleanupIsFinite leaves a subtest while its status command is
// held, as an assertion failure would, and bounds only the cleanup phase. On
// timeout it releases the observer itself so the red path stays finite.
func assertHeldStatusCleanupIsFinite(t *testing.T, hold func(*testing.T, *heldStatusGate)) {
	t.Helper()
	gate := newHeldStatusGate()
	exiting := make(chan struct{})
	finished := make(chan bool, 1)
	go func() {
		finished <- t.Run("exit while status is held", func(t *testing.T) {
			hold(t, gate)
			// Only registered cleanup may release the observer before client.Close.
			close(exiting)
		})
	}()
	select {
	case <-exiting:
	case passed := <-finished:
		t.Fatalf("subtest ended before holding its status command: passed=%t", passed)
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case passed := <-finished:
		if !passed {
			t.Fatal("held-status cleanup failed")
		}
	case <-timer.C:
		gate.release()
		<-finished
		t.Fatal("cleanup closed the client before releasing its held status observer")
	}
}

func holdGraceProbeStatus(t *testing.T, gate *heldStatusGate) {
	t.Helper()
	fixture := newGraceProbeFixture(t, gate)
	ctx, cancel := admissionfixture.NewControllableDeadlineContext(t.Context(), 10*time.Second)
	done := fixture.startGraceProbe(t, ctx, cancel)
	if err := gate.awaitEntered(done, 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func holdPendingStatusResult(t *testing.T, gate *heldStatusGate) {
	t.Helper()
	fixture := newPendingStatusResultFixture(t, gate)
	admissionfixture.AssertRetainedInspectionPending(t, fixture.observeErr)
}

// heldStatusGate holds the observed status command at its completed stage,
// before the transport completes its Pending, until the test releases it.
type heldStatusGate struct {
	entered  chan struct{}
	released chan struct{}
	enter    func()
	release  func()

	mu     sync.Mutex
	stages []string
}

func newHeldStatusGate() *heldStatusGate {
	gate := &heldStatusGate{entered: make(chan struct{}), released: make(chan struct{})}
	gate.enter = sync.OnceFunc(func() { close(gate.entered) })
	gate.release = sync.OnceFunc(func() { close(gate.released) })
	return gate
}

func (gate *heldStatusGate) observe(event pueue.CommandEvent) {
	if !slices.Contains(event.Argv, "status") {
		return
	}
	gate.mu.Lock()
	gate.stages = append(gate.stages, event.Stage)
	gate.mu.Unlock()
	if event.Stage == "completed" {
		gate.enter()
		<-gate.released
	}
}

func (gate *heldStatusGate) observedStages() []string {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return slices.Clone(gate.stages)
}

// awaitEntered also selects an already-returned probe so an early failure
// reports its own error instead of waiting for the watchdog.
func (gate *heldStatusGate) awaitEntered(done <-chan heldProbeResult, watchdog time.Duration) error {
	timer := time.NewTimer(watchdog)
	defer timer.Stop()
	select {
	case <-gate.entered:
		return nil
	case got := <-done:
		return fmt.Errorf("grace probe returned before its status command was held: facts=%s status stages=%v: %w", got.facts, gate.observedStages(), got.err)
	case <-timer.C:
		return fmt.Errorf("grace probe did not reach retained completion within %s: status stages=%v", watchdog, gate.observedStages())
	}
}

type heldProbeResult struct {
	facts json.RawMessage
	err   error
}

type graceProbeFixture struct {
	operation *inspection.Operation
	client    *pueue.Client
	logPath   string
	gate      *heldStatusGate
}

func newGraceProbeFixture(t *testing.T, gate *heldStatusGate) graceProbeFixture {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	client := newHeldStatusClient(t, bound, logPath, gate, gate.observe)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, client)
	completeInspectionPollTestOperation(t, operation)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	admissionfixture.WriteInspectionPollTestStatus(t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, false)
	return graceProbeFixture{operation: operation, client: client, logPath: logPath, gate: gate}
}

// newHeldStatusClient registers gate.release after the client, so LIFO cleanup
// releases a held observer before client.Close waits for that command's Pending.
func newHeldStatusClient(t *testing.T, bound *pueue.Client, logPath string, gate *heldStatusGate, observer func(pueue.CommandEvent)) *pueue.Client {
	t.Helper()
	client := admissionfixture.NewObservedInspectionTestClient(t, bound, logPath, 10*time.Second, observer)
	t.Cleanup(gate.release)
	return client
}

func (fixture graceProbeFixture) startGraceProbe(t *testing.T, ctx *admissionfixture.ControllableDeadlineContext, cancel func()) <-chan heldProbeResult {
	t.Helper()
	return startHeldProbe(t, fixture.gate, cancel, func() (json.RawMessage, error) {
		return observeCompletedInspection(ctx, fixture.operation, fixture.client, nil)
	})
}

// heldProbeJoinWatchdog bounds the silent wait for a cancelled probe; it is a
// diagnostic deadline, not permission to continue cleanup without the probe.
const heldProbeJoinWatchdog = 10 * time.Second

// heldProbeOwner is the part of *testing.T that startHeldProbe uses, so a test
// can drive its cleanup ordering with a recorder.
type heldProbeOwner interface {
	Helper()
	Cleanup(func())
	Error(args ...any)
}

// startHeldProbe runs probe on its own goroutine. Its cleanup, registered after
// the fixture, cancels the caller and joins the probe before the held command
// is released by the client fixture and the client and journal fixtures close.
// After the watchdog it reports, releases the held status so a probe waiting on
// that command can finish, and still waits for the exact probe to exit.
func startHeldProbe(t heldProbeOwner, gate *heldStatusGate, cancel func(), probe func() (json.RawMessage, error)) <-chan heldProbeResult {
	t.Helper()
	done := make(chan heldProbeResult, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		facts, err := probe()
		done <- heldProbeResult{facts: facts, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		timer := time.NewTimer(heldProbeJoinWatchdog)
		defer timer.Stop()
		select {
		case <-exited:
			return
		case <-timer.C:
		}
		gate.release()
		t.Error("probe did not return after its caller was cancelled; released its held status and holding cleanup until it exits")
		<-exited
	})
	return done
}

func awaitGraceProbeReturn(t *testing.T, done <-chan heldProbeResult) heldProbeResult {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case got := <-done:
		return got
	case <-timer.C:
		t.Fatal("grace probe did not return after its caller deadline")
		return heldProbeResult{}
	}
}

func TestTimelyCompletedInspectionEvidenceRequiresTermination(t *testing.T) {
	_, operation, _ := admissionfixture.NewCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	ready, err := timelyCompletedInspectionEvidence(t.Context(), operation)
	if ready || err != nil {
		t.Fatalf("completion without lifetime termination entered observation grace: ready=%t err=%v", ready, err)
	}
}

func TestTimelyCompletedInspectionEvidenceRejectsBudgetTermination(t *testing.T) {
	_, operation, _ := admissionfixture.NewCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationBudget)
	ready, err := timelyCompletedInspectionEvidence(t.Context(), operation)
	if ready || !errors.Is(err, inspection.ErrAdmissionExpired) {
		t.Fatalf("budget termination entered completion observation grace: ready=%t err=%v", ready, err)
	}
}

func TestValidateTimelyCompletedInspectionEvidenceUsesIndependentDeadlines(t *testing.T) {
	deadline := time.Unix(200, 0).UTC()
	completion := inspection.CompletionRecord{CompletedAt: deadline.Add(-2 * time.Second).Format(time.RFC3339Nano)}
	result := inspection.ResultRecord{Reason: inspection.ResultEligible}
	for _, tc := range []struct {
		name        string
		termination task.LifetimeTerminationRecord
		want        error
	}{
		{
			name: "late",
			termination: task.LifetimeTerminationRecord{
				Cause: task.LifetimeTerminationCompleted, ScopeEmpty: true,
				ObservedAt: deadline.Format(time.RFC3339Nano),
			},
			want: inspection.ErrAdmissionExpired,
		},
		{
			name: "scope empties before completion publication",
			termination: task.LifetimeTerminationRecord{
				Cause: task.LifetimeTerminationCompleted, ScopeEmpty: true,
				ObservedAt: deadline.Add(-3 * time.Second).Format(time.RFC3339Nano),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTimelyCompletedInspectionEvidence(deadline, result, completion, &tc.termination)
			if !errors.Is(err, tc.want) {
				t.Fatalf("timing validation error=%v want=%v", err, tc.want)
			}
		})
	}
}
