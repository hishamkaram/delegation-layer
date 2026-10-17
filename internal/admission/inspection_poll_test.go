package admission

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

	facts, err := awaitCompletedInspectionObservation(operation, client, inspection.ErrAdmissionExpired)
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

func newPrivateMaintenanceLaunchClient(t *testing.T, bound *pueue.Client, launcher func(context.Context, string, string) error) *pueue.Client {
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
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	ctx := newInspectionDeadlineContext(t.Context())
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	client := admissionfixture.NewObservedInspectionTestClient(t, bound, logPath, 10*time.Second, func(event pueue.CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "status") {
			ctx.Expire()
			<-released
		}
	})
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, client)
	completeInspectionPollTestOperation(t, operation)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	writeInspectionPollTestTerminalStatus(
		t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, true,
	)

	identity := operation.Request().SupervisorIdentity()
	observation, observeErr := client.ReconcileInspection(ctx, identity)
	pending := admissionfixture.AssertRetainedInspectionPending(t, observeErr)
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
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	ctx, cancel := admissionfixture.NewControllableDeadlineContext(t.Context(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{})
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	signalEntered := sync.OnceFunc(func() { close(entered) })
	client := admissionfixture.NewObservedInspectionTestClient(t, bound, logPath, 10*time.Second, func(event pueue.CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "status") {
			signalEntered()
			<-released
		}
	})
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, client)
	completeInspectionPollTestOperation(t, operation)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	admissionfixture.WriteInspectionPollTestStatus(t, filepath.Join(filepath.Dir(logPath), "status.json"), operation, false)

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
		ctx.Expire()
	case <-time.After(5 * time.Second):
		t.Fatal("grace probe did not reach retained completion")
	}
	got := <-done
	facts, err := got.facts, got.err
	pending := admissionfixture.AssertRetainedInspectionPending(t, err)
	if facts != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, pueue.ErrInFlight) {
		release()
		t.Fatalf("expired grace dropped its in-flight supervisor probe: facts=%s err=%v", facts, err)
	}
	release()
	admissionfixture.DrainInspectionPending(t, pending)
	assertInspectionStatusCommandCount(t, logPath, 1)
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
