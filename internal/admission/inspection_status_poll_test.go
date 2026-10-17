package admission

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

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestAwaitInspectionPollRetainsPendingWithoutAnotherObservation(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	client, logPath := supervisorfixture.NewBlockingSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, client)
	completeInspectionPollTestOperation(t, operation)
	ctx, cancel := context.WithTimeout(t.Context(), inspection.AdmissionTimeout)
	defer cancel()
	facts, awaitErr := AwaitInspection(ctx, operation, client)
	pending := admissionfixture.AssertRetainedInspectionPending(t, awaitErr)
	t.Cleanup(func() { admissionfixture.DrainInspectionPending(t, pending) })
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
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, bound)
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	if err := os.WriteFile(statusPath, failure.status, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	client := admissionfixture.NewObservedInspectionTestClient(t, bound, logPath, 10*time.Second, nil, failure.environment...)

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
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, bound)
	completeInspectionPollTestOperation(t, operation)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	writeInspectionPollTestTerminalStatus(t, statusPath, operation, true)
	failOnce := filepath.Join(filepath.Dir(logPath), "status-failed-once")
	var polls atomic.Int32
	client := newInspectionPollTestClient(t, bound, logPath, func(event pueue.CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "status") {
			polls.Add(1)
		}
	}, "FAKE_STATUS_FAIL_ONCE="+failOnce)

	facts, err := AwaitInspection(t.Context(), operation, client)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("transient operational status ended admission: facts=%s polls=%d err=%v", facts, polls.Load(), err)
	}
	if polls.Load() < 2 {
		t.Fatalf("admission did not retry the exact status observation: polls=%d", polls.Load())
	}
}

func TestInspectionAwaitStateReportsPersistentTransientStatusAtDeadline(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, bound)
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	admissionfixture.WriteInspectionPollTestStatus(t, statusPath, operation, false)
	client := admissionfixture.NewObservedInspectionTestClient(t, bound, logPath, 10*time.Second, nil, "FAKE_STATUS_EXIT=23")
	ctx := newInspectionDeadlineContext(t.Context())
	state, err := newInspectionAwaitState(ctx, operation, client, inspectionSubmissionState{})
	if err != nil {
		t.Fatal(err)
	}
	if facts, ended, observeErr := state.observe(ctx, operation, client); facts != nil || ended || observeErr != nil {
		t.Fatalf("first operational failure was not retained: facts=%s ended=%t err=%v", facts, ended, observeErr)
	}
	ctx.Expire()
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
			store := supervisorfixture.NewInspectionStore(t)
			bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
			operation := admissionfixture.NewInspectionPollTestOperation(t, store, bound)
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
	store := supervisorfixture.NewInspectionStore(t)
	client, logPath := supervisorfixture.NewBlockingSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, client)
	_, current, err := currentInspectionTerminalJob(t.Context(), operation, client, 7)
	pending := admissionfixture.AssertRetainedInspectionPending(t, err)
	t.Cleanup(func() { admissionfixture.DrainInspectionPending(t, pending) })
	if current || !errors.Is(err, pueue.ErrInFlight) {
		t.Fatalf("unfinished terminal probe changed reconciliation: current=%t err=%v", current, err)
	}
	assertInspectionStatusCommandCount(t, logPath, 1)
}

func TestAwaitInspectionPollReturnsAtCallerDeadlineWithPendingCommand(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, bound)
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	admissionfixture.WriteInspectionPollTestStatus(t, statusPath, operation, false)

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

	ctx, cancel := admissionfixture.NewControllableDeadlineContext(t.Context(), 10*time.Second)
	defer cancel()
	type result struct {
		facts json.RawMessage
		err   error
	}
	done := make(chan result, 1)
	go func() {
		facts, observeErr := AwaitInspection(ctx, operation, client)
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
	ctx.Expire()

	joinContext, cancelJoin := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelJoin()
	select {
	case got := <-done:
		if got.facts != nil || !errors.Is(got.err, inspection.ErrAdmissionExpired) || !errors.Is(got.err, pueue.ErrInFlight) {
			release()
			t.Fatalf("expired caller lost its stable pending result: facts=%s err=%v", got.facts, got.err)
		}
		pending := admissionfixture.AssertRetainedInspectionPending(t, got.err)
		release()
		admissionfixture.DrainInspectionPending(t, pending)
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
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, bound)
	completeInspectionPollTestOperation(t, operation)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
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
	store, operation, _ := admissionfixture.NewCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	path := filepath.Join(store.Root, "inspections", operation.Request().TaskID, "completion.json")
	if writeErr := os.WriteFile(path, []byte(`{}`), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if completed, completionErr := inspectionCompletionHint(t.Context(), operation); completed || completionErr == nil || errors.Is(completionErr, os.ErrNotExist) {
		t.Fatalf("malformed completion became a usable or absent hint: completed=%t err=%v", completed, completionErr)
	}
	facts, awaitErr := AwaitInspection(t.Context(), operation, nil)
	if facts != nil || awaitErr == nil || errors.Is(awaitErr, os.ErrNotExist) {
		t.Fatalf("malformed completion reached queue observation: facts=%s err=%v", facts, awaitErr)
	}
}

func TestAwaitInspectionPollDoesNotProbeAfterAdmissionDeadline(t *testing.T) {
	_, operation := admissionfixture.NewInspectionWorkerOperation(t, time.Unix(100, 0).UTC())
	facts, awaitErr := AwaitInspection(t.Context(), operation, nil)
	if facts != nil || !errors.Is(awaitErr, inspection.ErrAdmissionExpired) {
		t.Fatalf("expired admission reached queue observation: facts=%s err=%v", facts, awaitErr)
	}
}

func TestAwaitInspectionNormalizesJournalLockDeadline(t *testing.T) {
	_, operation := admissionfixture.NewInspectionWorkerOperation(t, time.Now().UTC())
	journalTimeout := errors.Join(context.DeadlineExceeded, task.ErrLockBusy)
	t.Run("keeps a local journal ceiling distinct", func(t *testing.T) {
		got := normalizeInspectionAwaitError(t.Context(), operation, journalTimeout)
		if errors.Is(got, inspection.ErrAdmissionExpired) || !errors.Is(got, context.DeadlineExceeded) || !errors.Is(got, task.ErrLockBusy) {
			t.Fatalf("local journal deadline changed admission classification: %v", got)
		}
	})
	t.Run("classifies the caller admission deadline", func(t *testing.T) {
		ctx := newInspectionDeadlineContext(t.Context())
		ctx.Expire()
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

func (ctx *inspectionDeadlineContext) Expire() { ctx.once.Do(func() { close(ctx.done) }) }

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

func newInspectionPollTestClient(t *testing.T, bound *pueue.Client, logPath string, observer func(pueue.CommandEvent), extraEnvironment ...string) *pueue.Client {
	return admissionfixture.NewObservedInspectionTestClient(t, bound, logPath, 10*time.Second, observer, extraEnvironment...)
}

func writeInspectionPollTestTerminalStatus(t *testing.T, path string, operation *inspection.Operation, succeeded bool) {
	t.Helper()
	result := "Killed"
	if succeeded {
		result = "Success"
	}
	admissionfixture.WriteInspectionPollTestState(t, path, operation, map[string]any{
		"Done": map[string]any{"enqueued_at": admissionfixture.InspectionPollStamp, "start": admissionfixture.InspectionPollStamp, "end": admissionfixture.InspectionPollStamp, "result": result},
	})
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
