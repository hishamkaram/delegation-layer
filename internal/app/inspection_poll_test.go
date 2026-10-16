package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
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
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
	claim, err := operation.ClaimSubmission(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = claim.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	writeInspectionPollTestState(t, statusPath, operation, map[string]any{
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

func TestStagedAuxiliaryRecoveryPreservesPendingBeforeRaceFallback(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	writeInspectionPollTestStatus(t, statusPath, operation, false)
	observation, err := bound.ReconcileInspection(t.Context(), operation.Request().SupervisorIdentity())
	if err != nil || !observation.Matched {
		t.Fatalf("prepare exact queue observation: %+v %v", observation, err)
	}
	observation.Job.State = pueue.StateStashed
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	defer release()
	var statuses atomic.Int32
	client := newObservedInspectionTestClient(t, bound, logPath, 50*time.Millisecond, func(event pueue.CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "status") && statuses.Add(1) == 1 {
			<-released
		}
	})
	readbacks := 0
	_, err = recoverStagedAuxiliaryObservation(t.Context(), client, store.Root, observation,
		func(current pueue.InspectionObservation) error {
			return operation.RecordReceiptContext(t.Context(), current.Job.ID)
		},
		func(int64) error { readbacks++; return nil }, nil)
	pending := assertRetainedInspectionPending(t, err)
	if readbacks != 0 {
		t.Errorf("pending command reached completed-race receipt fallback %d times", readbacks)
	}
	if err = client.CloseAfterPending(err); err != nil {
		t.Errorf("pending client cleanup failed: %v", err)
	}
	release()
	drainInspectionPending(t, pending)
	if got := statuses.Load(); got != 1 {
		t.Fatalf("pending recovery started %d observations, want one", got)
	}
}

func TestAcceptInspectionObservationRefusesMissingDurableReceipt(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, _, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
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
			writeInspectionPollTestStatus(t, statusPath, operation, false)
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
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
	completeInspectionPollTestOperation(t, operation)
	writeInspectionPollTestStatus(t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, false)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()
	var polls atomic.Int32
	client := newInspectionPollTestClient(t, bound, logPath, func(event pueue.CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "status") && polls.Add(1) == 2 {
			cancel()
		}
	})
	facts, awaitErr := awaitInspection(ctx, operation, client)
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
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
	if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	failedPath := filepath.Join(filepath.Dir(logPath), "failed-status.json")
	writeInspectionPollTestStatus(t, statusPath, operation, false)
	writeInspectionPollTestStatus(t, failedPath, operation, true)
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
	facts, awaitErr := awaitInspection(ctx, operation, client)
	select {
	case publishErr := <-published:
		if publishErr != nil {
			t.Fatal(publishErr)
		}
	default:
		t.Fatal("fixture never observed its initial running worker")
	}
	if facts != nil || !errors.Is(awaitErr, errInspectionWorkerUnavailable) || polls.Load() != 2 {
		t.Fatalf("watchdog lost failed worker without completion: facts=%s polls=%d err=%v", facts, polls.Load(), awaitErr)
	}
	if completed, completionErr := inspectionCompletionHint(t.Context(), operation); completed || completionErr != nil {
		t.Fatalf("missing completion became a scheduling error or authority: completed=%t err=%v", completed, completionErr)
	}
}

func TestAwaitInspectionPollReadsRecordedProofBeforeQueue(t *testing.T) {
	_, operation, job := newCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if recordErr := operation.RecordWorkerSuccessContext(t.Context(), job.ID); recordErr != nil {
		t.Fatal(recordErr)
	}
	// A queue read would dereference the nil supervisor. Only the independently
	// recorded exit, combined with the local completion, can finish admission.
	facts, awaitErr := awaitInspection(t.Context(), operation, nil)
	if awaitErr != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("saved proof required another queue read: facts=%s err=%v", facts, awaitErr)
	}
}

func TestAwaitCompletedInspectionObservationPublishesTimelyExitProof(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
	completeInspectionPollTestOperation(t, operation)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	writeInspectionPollTestTerminalStatus(
		t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, true,
	)
	client := newInspectionPollTestClient(t, bound, logPath, nil)

	facts, err := awaitCompletedInspectionObservation(operation, client, inspection.ErrAdmissionExpired)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("timely completion was not published during observation grace: facts=%s err=%v", facts, err)
	}
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if err != nil || !succeeded || result.Reason != inspection.ResultEligible {
		t.Fatalf("observation grace did not persist worker success: result=%+v succeeded=%t err=%v", result, succeeded, err)
	}
}

func TestAwaitCompletedInspectionObservationStartsIndependentControlWindow(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	createdAt := time.Now().UTC().Add(-inspection.AdmissionTimeout - pueue.DefaultObservationTimeout - time.Second)
	operation := newInspectionPollTestOperationAt(t, store, bound, createdAt)
	completeInspectionPollTestOperation(t, operation)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	rewriteInspectionPollTestTerminationTime(t, store, operation, createdAt.Add(4*time.Millisecond))
	writeInspectionPollTestTerminalStatus(
		t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, true,
	)
	client := newInspectionPollTestClient(t, bound, logPath, nil)

	facts, err := awaitCompletedInspectionObservation(operation, client, inspection.ErrAdmissionExpired)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("expired admission clock shortened independent control observation: facts=%s err=%v", facts, err)
	}
}

func TestAcceptedTerminalObservationLaunchesMaintenanceAfterCallerExpiry(t *testing.T) {
	store, operation, bound, job := newRecordedTerminalCleanupFixture(t)
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

func newPrivateMaintenanceLaunchClient(t *testing.T, bound *pueue.Client, launcher func(context.Context, string, string) error) *pueue.Client {
	t.Helper()
	client, err := pueue.NewClient(bound.Binding(), pueue.Options{
		ObservationTimeout:          pueue.DefaultObservationTimeout,
		Environment:                 os.Environ(),
		SuccessfulCleanupAuthorizer: authorizePrivateSuccessfulCleanup,
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

func (probe *privateMaintenanceLaunchProbe) launch(launchContext context.Context, executable, root string) error {
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
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	ctx := newInspectionDeadlineContext(t.Context())
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	client := newObservedInspectionTestClient(t, bound, logPath, 10*time.Second, func(event pueue.CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "status") {
			ctx.expire()
			<-released
		}
	})
	operation := newInspectionPollTestOperation(t, store, client)
	completeInspectionPollTestOperation(t, operation)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	writeInspectionPollTestTerminalStatus(
		t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, true,
	)

	identity := operation.Request().SupervisorIdentity()
	observation, observeErr := client.ReconcileInspection(ctx, identity)
	pending := assertRetainedInspectionPending(t, observeErr)
	if observation.Pending != pending {
		release()
		t.Fatalf("exact observation did not expose its retained result: observation=%+v err=%v", observation, observeErr)
	}
	release()
	facts, err := awaitCompletedInspectionObservation(
		operation, client, errors.Join(inspection.ErrAdmissionExpired, observeErr),
	)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("pending status result was not accepted: facts=%s err=%v", facts, err)
	}
	assertInspectionStatusCommandCount(t, logPath, 1)
}

func TestObserveCompletedInspectionRetainsGraceProbeAfterCallerExpiry(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	ctx, cancel := newControllableDeadlineContext(t.Context(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{})
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	signalEntered := sync.OnceFunc(func() { close(entered) })
	client := newObservedInspectionTestClient(t, bound, logPath, 10*time.Second, func(event pueue.CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "status") {
			signalEntered()
			<-released
		}
	})
	operation := newInspectionPollTestOperation(t, store, client)
	completeInspectionPollTestOperation(t, operation)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	writeInspectionPollTestStatus(t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, false)

	type result struct {
		facts json.RawMessage
		err   error
	}
	done := make(chan result, 1)
	go func() {
		facts, err := observeCompletedInspection(ctx, operation, client, nil)
		done <- result{facts: facts, err: err}
	}()
	select {
	case <-entered:
		ctx.expire()
	case <-time.After(5 * time.Second):
		t.Fatal("grace probe did not reach retained completion")
	}
	got := <-done
	facts, err := got.facts, got.err
	pending := assertRetainedInspectionPending(t, err)
	if facts != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, pueue.ErrInFlight) {
		release()
		t.Fatalf("expired grace dropped its in-flight supervisor probe: facts=%s err=%v", facts, err)
	}
	release()
	drainInspectionPending(t, pending)
	assertInspectionStatusCommandCount(t, logPath, 1)
}

func TestTimelyCompletedInspectionEvidenceRequiresTermination(t *testing.T) {
	_, operation, _ := newCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	ready, err := timelyCompletedInspectionEvidence(t.Context(), operation)
	if ready || err != nil {
		t.Fatalf("completion without lifetime termination entered observation grace: ready=%t err=%v", ready, err)
	}
}

func TestTimelyCompletedInspectionEvidenceRejectsBudgetTermination(t *testing.T) {
	_, operation, _ := newCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationBudget)
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

func TestAwaitInspectionPollRetainsPendingWithoutAnotherObservation(t *testing.T) {
	store := newBareInspectionStore(t)
	client, logPath := newBlockingInspectionSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, client)
	completeInspectionPollTestOperation(t, operation)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()
	facts, awaitErr := awaitInspection(ctx, operation, client)
	pending := assertRetainedInspectionPending(t, awaitErr)
	t.Cleanup(func() { drainInspectionPending(t, pending) })
	if facts != nil || !errors.Is(awaitErr, pueue.ErrInFlight) {
		t.Fatalf("pending observation changed admission result: facts=%s err=%v", facts, awaitErr)
	}
	invocations, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 1 {
		t.Fatalf("completion or watchdog overlapped the pending command: status count=%d log=%s", count, invocations)
	}
}

type inspectionStatusFailureCase struct {
	name        string
	status      []byte
	environment []string
	retry       bool
	underlying  func(*testing.T, error)
}

func TestInspectionStatusFailuresUseSharedClassification(t *testing.T) {
	for _, failure := range inspectionStatusFailureCases() {
		for _, path := range []string{"watchdog", "terminal"} {
			t.Run(failure.name+"/"+path, func(t *testing.T) {
				runInspectionStatusFailureCase(t, failure, path)
			})
		}
	}
}

func inspectionStatusFailureCases() []inspectionStatusFailureCase {
	return []inspectionStatusFailureCase{
		{
			name:   "malformed-json",
			status: []byte("{"),
			underlying: func(t *testing.T, err error) {
				t.Helper()
				var syntaxErr *json.SyntaxError
				if !errors.As(err, &syntaxErr) || !errors.Is(err, pueue.ErrUnknown) {
					t.Fatalf("malformed status lost parser errors: %v", err)
				}
			},
		},
		{
			name:        "nonzero-exit",
			status:      []byte(`{"tasks":{},"groups":{}}`),
			environment: []string{"FAKE_STATUS_EXIT=23"},
			retry:       true,
			underlying: func(t *testing.T, err error) {
				t.Helper()
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
					t.Fatalf("nonzero status lost exit error: %v", err)
				}
			},
		},
	}
}

func runInspectionStatusFailureCase(t *testing.T, failure inspectionStatusFailureCase, path string) {
	t.Helper()
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	if err := os.WriteFile(statusPath, failure.status, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	client := newObservedInspectionTestClient(t, bound, logPath, 10*time.Second, nil, failure.environment...)

	var observeErr error
	if path == "watchdog" {
		identity := operation.Request().SupervisorIdentity()
		_, _, observeErr = pollInspectionStatus(t.Context(), operation, client, &identity)
	} else {
		_, _, observeErr = currentInspectionTerminalJob(t.Context(), operation, client, 7)
	}
	if !errors.Is(observeErr, pueue.ErrStatusOperational) {
		t.Fatalf("completed status failure lost its operational cause: %v", observeErr)
	}
	retryErr := inspectionStatusRetryError(observeErr)
	if failure.retry != (retryErr != nil) {
		t.Fatalf("completed status retry = %t, want %t: %v", retryErr != nil, failure.retry, observeErr)
	}
	failure.underlying(t, observeErr)
	assertInspectionStatusCommandCount(t, logPath, 1)
}

func TestAwaitInspectionRetriesTransientStatusUntilDurableProof(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
	completeInspectionPollTestOperation(t, operation)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	writeInspectionPollTestTerminalStatus(t, statusPath, operation, true)
	failOnce := filepath.Join(filepath.Dir(logPath), "status-failed-once")
	var polls atomic.Int32
	client := newInspectionPollTestClient(t, bound, logPath, func(event pueue.CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "status") {
			polls.Add(1)
		}
	}, "FAKE_STATUS_FAIL_ONCE="+failOnce)

	facts, err := awaitInspection(t.Context(), operation, client)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("transient operational status ended admission: facts=%s polls=%d err=%v", facts, polls.Load(), err)
	}
	if polls.Load() < 2 {
		t.Fatalf("admission did not retry the exact status observation: polls=%d", polls.Load())
	}
}

func TestInspectionAwaitStateReportsPersistentTransientStatusAtDeadline(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	writeInspectionPollTestStatus(t, statusPath, operation, false)
	client := newObservedInspectionTestClient(t, bound, logPath, 10*time.Second, nil, "FAKE_STATUS_EXIT=23")
	ctx := newInspectionDeadlineContext(t.Context())
	state, err := newInspectionAwaitState(ctx, operation, client, inspectionSubmissionState{})
	if err != nil {
		t.Fatal(err)
	}
	if facts, ended, observeErr := state.observe(ctx, operation, client); facts != nil || ended || observeErr != nil {
		t.Fatalf("first operational failure was not retained: facts=%s ended=%t err=%v", facts, ended, observeErr)
	}
	ctx.expire()
	facts, ended, observeErr := state.observe(ctx, operation, client)
	if facts != nil || !ended || !errors.Is(observeErr, inspection.ErrAdmissionExpired) || !errors.Is(observeErr, pueue.ErrStatusOperational) {
		t.Fatalf("deadline lost persistent status failure: facts=%s ended=%t err=%v", facts, ended, observeErr)
	}
}

func TestInspectionStatusSchemaFailureIsTerminal(t *testing.T) {
	retry, err := classifyInspectionStatusObservation(
		pueue.InspectionObservation{}, errors.Join(pueue.ErrUnknown, pueue.ErrStatusOperational, &json.SyntaxError{}),
	)
	if retry || err == nil || inspectionStatusRetryError(err) != nil {
		t.Fatalf("schema failure classified as retry=%t err=%v", retry, err)
	}
}

func TestInspectionStatusClassificationNeverRetriesAuthorityFaults(t *testing.T) {
	for _, fault := range []error{
		task.ErrIdentityMismatch,
		task.ErrEvidenceFault,
		pueue.ErrBinding,
		pueue.ErrConfiguration,
	} {
		retry, err := classifyInspectionStatusObservation(
			pueue.InspectionObservation{}, errors.Join(pueue.ErrStatusOperational, fault),
		)
		if retry || !errors.Is(err, fault) {
			t.Fatalf("authority fault %v classified as retry=%t err=%v", fault, retry, err)
		}
	}
}

func TestInspectionValidNoMatchRemainsRetryable(t *testing.T) {
	for _, path := range []string{"watchdog", "terminal"} {
		t.Run(path, func(t *testing.T) {
			store := newBareInspectionStore(t)
			bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
			operation := newInspectionPollTestOperation(t, store, bound)
			writeInspectionPollTestNoMatchStatus(t, filepath.Join(filepath.Dir(logPath), "status.json"), operation)
			if err := os.WriteFile(logPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			client := newInspectionPollTestClient(t, bound, logPath, nil)

			if path == "watchdog" {
				identity := operation.Request().SupervisorIdentity()
				facts, observed, err := pollInspectionStatus(t.Context(), operation, client, &identity)
				if facts != nil || observed || err != nil {
					t.Fatalf("valid no-match stopped watchdog polling: facts=%s observed=%t err=%v", facts, observed, err)
				}
			} else if job, current, err := currentInspectionTerminalJob(t.Context(), operation, client, 7); current || err != nil || job.ID != 0 {
				t.Fatalf("valid no-match stopped terminal reconciliation: job=%+v current=%t err=%v", job, current, err)
			}
			assertInspectionStatusCommandCount(t, logPath, 1)
		})
	}
}

func TestInspectionTerminalReconciliationRetainsOnePendingProbe(t *testing.T) {
	store := newBareInspectionStore(t)
	client, logPath := newBlockingInspectionSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, client)
	_, current, err := currentInspectionTerminalJob(t.Context(), operation, client, 7)
	pending := assertRetainedInspectionPending(t, err)
	t.Cleanup(func() { drainInspectionPending(t, pending) })
	if current || !errors.Is(err, pueue.ErrInFlight) {
		t.Fatalf("unfinished terminal probe changed reconciliation: current=%t err=%v", current, err)
	}
	assertInspectionStatusCommandCount(t, logPath, 1)
}

func TestAwaitInspectionPollReturnsAtCallerDeadlineWithPendingCommand(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	writeInspectionPollTestStatus(t, statusPath, operation, false)

	entered := make(chan struct{})
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	signalEntered := sync.OnceFunc(func() { close(entered) })
	client, err := pueue.NewClient(bound.Binding(), pueue.Options{
		Environment:        append(os.Environ(), "FAKE_LOG="+logPath, "FAKE_STATUS="+statusPath),
		ObservationTimeout: 10 * time.Second,
		Observer: func(event pueue.CommandEvent) {
			if event.Stage == "completed" && slices.Contains(event.Argv, "status") {
				signalEntered()
				<-released
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})

	ctx, cancel := newControllableDeadlineContext(t.Context(), 10*time.Second)
	defer cancel()
	type result struct {
		facts json.RawMessage
		err   error
	}
	done := make(chan result, 1)
	go func() {
		facts, observeErr := awaitInspection(ctx, operation, client)
		done <- result{facts: facts, err: observeErr}
	}()

	ready, cancelReady := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelReady()
	select {
	case <-entered:
	case got := <-done:
		release()
		t.Fatalf("inspection returned before its supervisor command was retained: facts=%s err=%v", got.facts, got.err)
	case <-ready.Done():
		release()
		t.Fatalf("supervisor command did not reach the controlled pending state: %v", ready.Err())
	}
	ctx.expire()

	joinContext, cancelJoin := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelJoin()
	select {
	case got := <-done:
		if got.facts != nil || !errors.Is(got.err, inspection.ErrAdmissionExpired) || !errors.Is(got.err, pueue.ErrInFlight) {
			release()
			t.Fatalf("expired caller lost its stable pending result: facts=%s err=%v", got.facts, got.err)
		}
		pending := assertRetainedInspectionPending(t, got.err)
		release()
		drainInspectionPending(t, pending)
	case <-joinContext.Done():
		release()
		<-done
		t.Fatalf("inspection did not return after its caller deadline: %v", joinContext.Err())
	}

	invocations, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != 1 {
		t.Fatalf("deadline started another supervisor observation: status count=%d log=%s", count, invocations)
	}
}

func TestInspectionAwaitStateUsesExactObservationAfterCompletion(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
	completeInspectionPollTestOperation(t, operation)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	writeInspectionPollTestTerminalStatus(
		t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, true,
	)
	client := newInspectionPollTestClient(t, bound, logPath, nil)
	state, err := newInspectionAwaitState(t.Context(), operation, client, inspectionSubmissionState{})
	if err != nil {
		t.Fatal(err)
	}

	facts, ended, err := state.observe(t.Context(), operation, client)
	if string(facts) != "{\"eligible\":true}\n" || !ended || err != nil {
		t.Fatalf("exact observation did not accept completion: facts=%s ended=%t err=%v", facts, ended, err)
	}
	invocations, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(invocations), "arg=label="+operation.Request().SupervisorIdentity().Label()+"\n") {
		t.Fatalf("completion used a root-wide queue snapshot: %s", invocations)
	}
}

func TestAwaitInspectionPollRejectsMalformedCompletionBeforeQueue(t *testing.T) {
	store, operation, _ := newCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	path := filepath.Join(store.Root, "inspections", operation.Request().TaskID, "completion.json")
	if writeErr := os.WriteFile(path, []byte(`{}`), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if completed, completionErr := inspectionCompletionHint(t.Context(), operation); completed || completionErr == nil || errors.Is(completionErr, os.ErrNotExist) {
		t.Fatalf("malformed completion became a usable or absent hint: completed=%t err=%v", completed, completionErr)
	}
	facts, awaitErr := awaitInspection(t.Context(), operation, nil)
	if facts != nil || awaitErr == nil || errors.Is(awaitErr, os.ErrNotExist) {
		t.Fatalf("malformed completion reached queue observation: facts=%s err=%v", facts, awaitErr)
	}
}

func TestAwaitInspectionPollDoesNotProbeAfterAdmissionDeadline(t *testing.T) {
	_, operation := newInspectionWorkerOperation(t, time.Unix(100, 0).UTC())
	facts, awaitErr := awaitInspection(t.Context(), operation, nil)
	if facts != nil || !errors.Is(awaitErr, inspection.ErrAdmissionExpired) {
		t.Fatalf("expired admission reached queue observation: facts=%s err=%v", facts, awaitErr)
	}
}

func TestAwaitInspectionNormalizesJournalLockDeadline(t *testing.T) {
	_, operation := newInspectionWorkerOperation(t, time.Now().UTC())
	journalTimeout := errors.Join(context.DeadlineExceeded, task.ErrLockBusy)
	t.Run("keeps a local journal ceiling distinct", func(t *testing.T) {
		got := normalizeInspectionAwaitError(t.Context(), operation, journalTimeout)
		if errors.Is(got, inspection.ErrAdmissionExpired) || !errors.Is(got, context.DeadlineExceeded) || !errors.Is(got, task.ErrLockBusy) {
			t.Fatalf("local journal deadline changed admission classification: %v", got)
		}
	})
	t.Run("classifies the caller admission deadline", func(t *testing.T) {
		ctx := newInspectionDeadlineContext(t.Context())
		ctx.expire()
		got := normalizeInspectionAwaitError(ctx, operation, journalTimeout)
		if !errors.Is(got, inspection.ErrAdmissionExpired) || !errors.Is(got, context.DeadlineExceeded) || !errors.Is(got, task.ErrLockBusy) {
			t.Fatalf("admission deadline lost the local journal error: %v", got)
		}
	})
}

type inspectionDeadlineContext struct {
	parent context.Context
	done   chan struct{}
	once   sync.Once
}

func newInspectionDeadlineContext(parent context.Context) *inspectionDeadlineContext {
	return &inspectionDeadlineContext{parent: parent, done: make(chan struct{})}
}

func (ctx *inspectionDeadlineContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (ctx *inspectionDeadlineContext) Done() <-chan struct{} { return ctx.done }

func (ctx *inspectionDeadlineContext) Err() error {
	select {
	case <-ctx.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (ctx *inspectionDeadlineContext) Value(key any) any { return ctx.parent.Value(key) }

func (ctx *inspectionDeadlineContext) expire() { ctx.once.Do(func() { close(ctx.done) }) }

func newInspectionPollTestOperation(t *testing.T, store *taskdir.Store, client *pueue.Client) *inspection.Operation {
	return newInspectionPollTestOperationAt(t, store, client, time.Now().UTC())
}

func newInspectionPollTestOperationAt(t *testing.T, store *taskdir.Store, client *pueue.Client, createdAt time.Time) *inspection.Operation {
	return newInspectionPollTestOperationAtWithOwnership(t, store, client, createdAt, "")
}

func newInspectionPollTestOperationAtWithOwnership(t *testing.T, store *taskdir.Store, client *pueue.Client, createdAt time.Time, ownership string) *inspection.Operation {
	t.Helper()
	digest := strings.Repeat("a", 64)
	workerExitProof := ""
	if ownership == task.RunnerOwnershipManaged {
		workerExitProof = inspection.WorkerExitProofGuardianWaitV1
	}
	operation, openErr := inspection.OpenOperation(store, newBareInspectionRequest(t, store), inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: "/usr/bin/true", WorkerSHA256: digest,
		RunnerOwnership: ownership, WorkerExitProof: workerExitProof,
		Supervisor: client.Binding(),
	}, createdAt)
	if openErr != nil {
		t.Fatal(openErr)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return operation
}

func rewriteInspectionPollTestTerminationTime(t *testing.T, store *taskdir.Store, operation *inspection.Operation, observedAt time.Time) {
	t.Helper()
	path := filepath.Join(store.Root, "inspections", operation.Request().TaskID, "lifetime-termination.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record task.LifetimeTerminationRecord
	if err = task.DecodeStrict(data, &record); err != nil {
		t.Fatal(err)
	}
	record.ObservedAt = observedAt.UTC().Format(time.RFC3339Nano)
	data, err = task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func completeInspectionPollTestOperation(t *testing.T, operation *inspection.Operation) {
	t.Helper()
	created, parseErr := time.Parse(time.RFC3339Nano, operation.Request().CreatedAt)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	submission, claimErr := operation.ClaimSubmission(created.Add(time.Millisecond))
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if consumeErr := submission.ConsumeInspectionSubmission(); consumeErr != nil {
		t.Fatal(consumeErr)
	}
	if receiptErr := operation.RecordReceiptContext(t.Context(), 7); receiptErr != nil {
		t.Fatal(receiptErr)
	}
	start, startErr := operation.ClaimStart(created.Add(2 * time.Millisecond))
	if startErr != nil {
		t.Fatal(startErr)
	}
	if consumeErr := start.Consume(); consumeErr != nil {
		t.Fatal(consumeErr)
	}
	if completeErr := operation.Complete(inspection.ResultEligible, []byte(`{"eligible":true}`), created.Add(3*time.Millisecond)); completeErr != nil {
		t.Fatal(completeErr)
	}
}

func completeInspectionPollTestLifetime(t *testing.T, operation *inspection.Operation, cause string) {
	t.Helper()
	const bootID = "inspection-poll-boot"
	_, err := operation.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "inspection-poll-process-group",
		BootID:    bootID,
		Deadline:  operation.Deadline(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = operation.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   "inspection-poll-process-group",
		BootID:    bootID,
		Deadline:  operation.Deadline(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if operation.Request().Binding.WorkerExitProof == inspection.WorkerExitProofGuardianWaitV1 {
		worker, workerErr := operation.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{
			PID: 307, ProcessStart: "inspection-poll-worker", BootID: bootID,
		})
		if workerErr != nil {
			t.Fatal(workerErr)
		}
		workerBytes, marshalErr := task.MarshalCanonical(worker)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		result, _, resultErr := operation.ReadCompletedContext(t.Context())
		if resultErr != nil {
			t.Fatal(resultErr)
		}
		waitResult := task.LifetimeWorkerWaitSuccess
		exitCode := 0
		if result.Reason != inspection.ResultEligible {
			waitResult = task.LifetimeWorkerWaitExitError
			exitCode = 1
		}
		if _, waitErr := operation.RecordLifetimeWorkerWait(taskdir.LifetimeWorkerWaitFacts{
			WorkerSHA256: task.ComputeSHA256(workerBytes),
			ChildPID:     worker.PID,
			Result:       waitResult,
			ExitCode:     &exitCode,
			ObservedAt:   time.Now().UTC(),
		}); waitErr != nil {
			t.Fatal(waitErr)
		}
	}
	if _, err = operation.RecordLifetimeTermination(cause); err != nil {
		t.Fatal(err)
	}
}

func newInspectionPollTestClient(t *testing.T, bound *pueue.Client, logPath string, observer func(pueue.CommandEvent), extraEnvironment ...string) *pueue.Client {
	return newObservedInspectionTestClient(t, bound, logPath, 10*time.Second, observer, extraEnvironment...)
}

func writeInspectionPollTestStatus(t *testing.T, path string, operation *inspection.Operation, failed bool) {
	t.Helper()
	state := map[string]any{"Running": map[string]any{"enqueued_at": inspectionPollStamp, "start": inspectionPollStamp}}
	if failed {
		state = map[string]any{"Done": map[string]any{"enqueued_at": inspectionPollStamp, "start": inspectionPollStamp, "end": inspectionPollStamp, "result": "Killed"}}
	}
	writeInspectionPollTestState(t, path, operation, state)
}

func writeInspectionPollTestTerminalStatus(t *testing.T, path string, operation *inspection.Operation, succeeded bool) {
	t.Helper()
	result := "Killed"
	if succeeded {
		result = "Success"
	}
	writeInspectionPollTestState(t, path, operation, map[string]any{
		"Done": map[string]any{"enqueued_at": inspectionPollStamp, "start": inspectionPollStamp, "end": inspectionPollStamp, "result": result},
	})
}

const inspectionPollStamp = "2026-10-09T00:00:00Z"

func writeInspectionPollTestState(t *testing.T, path string, operation *inspection.Operation, state map[string]any) {
	t.Helper()
	identity := operation.Request().SupervisorIdentity()
	data, marshalErr := json.Marshal(map[string]any{
		"tasks": map[string]any{"7": map[string]any{
			"id": 7, "created_at": inspectionPollStamp, "original_command": "inspection-worker", "command": "inspection-worker",
			"path": "/tmp", "envs": map[string]string{}, "group": identity.Group(),
			"dependencies": []int64{}, "priority": 0, "label": identity.Label(), "status": state,
		}},
		"groups": map[string]any{identity.Group(): map[string]any{"status": "Running", "parallel_tasks": 1}},
	})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
}

func writeInspectionPollTestNoMatchStatus(t *testing.T, path string, operation *inspection.Operation) {
	t.Helper()
	identity := operation.Request().SupervisorIdentity()
	data, err := json.Marshal(map[string]any{
		"tasks":  map[string]any{},
		"groups": map[string]any{identity.Group(): map[string]any{"status": "Running", "parallel_tasks": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertInspectionStatusCommandCount(t *testing.T, logPath string, want int) {
	t.Helper()
	invocations, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(invocations), "arg=status\n"); count != want {
		t.Fatalf("status command count=%d want=%d log=%s", count, want, invocations)
	}
}
