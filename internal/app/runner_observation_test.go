package app

import (
	"bytes"
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

	"github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestReconcileRunnerUsesCallerObservationDeadline(t *testing.T) {
	fixture := newRunnerRefreshFixture(t)
	t.Cleanup(func() { fixture.close(t) })
	recordRunnerSupervisorReceipt(t, fixture, 7)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		t.Fatal("runner fixture has no finite observation deadline")
	}
	call := startRunnerObservationTestCall(t, ctx, fixture)
	waitInspectionWorkerTimeoutSignal(t, ctx, call.entered, "runner status completion gate")

	// A completed valid status is withheld at Pending.Done publication. The
	// ordinary control timeout must not truncate the runner's finite context.
	faultContext, cancelFault := context.WithTimeout(ctx, 2*inspectionWorkerTestObservationTimeout)
	defer cancelFault()
	select {
	case <-call.done:
		t.Errorf("runner abandoned valid status before its caller deadline: %v", call.err)
	case <-faultContext.Done():
	}
	assertRunnerObservationTestReceipt(t, fixture, 7)
	call.release()
	waitInspectionWorkerTimeoutSignal(t, ctx, call.done, "runner reconciliation")
	if call.err != nil || call.client == nil {
		t.Fatalf("delayed valid status prevented runner startup: client=%v err=%v", call.client, call.err)
	}
	if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) || ctx.Err() != nil {
		t.Fatalf("runner changed or exhausted its caller deadline: got=%v want=%v err=%v", got, deadline, ctx.Err())
	}
	assertRunnerObservationTestAdmitted(t, fixture, call)
}

func TestReconcileRunnerRetainsPendingAtCallerDeadline(t *testing.T) {
	fixture := newRunnerRefreshFixture(t)
	t.Cleanup(func() { fixture.close(t) })
	ctx, cancel := testsupport.NewControllableDeadlineContext(t.Context(), 10*time.Second)
	defer cancel()
	call := startRunnerObservationTestCall(t, ctx, fixture)
	readyContext, cancelReady := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelReady()
	waitInspectionWorkerTimeoutSignal(t, readyContext, call.entered, "runner status completion gate")
	ctx.Expire()
	joinContext, cancelJoin := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelJoin()
	waitInspectionWorkerTimeoutSignal(t, joinContext, call.done, "expired runner reconciliation")

	var inFlight *pueue.InFlightError
	if !errors.Is(call.err, pueue.ErrUnknown) || !errors.Is(call.err, pueue.ErrInFlight) || !errors.As(call.err, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("expired runner lost its uncertain command owner: %v", call.err)
	}
	if call.observation.Pending != inFlight.Pending || call.observation.Matched || call.client != nil {
		t.Fatalf("expired runner changed pending ownership or invented a match: observation=%+v client=%v", call.observation, call.client)
	}
	if call.response.Admission != task.AdmissionUnknown.String() || call.response.Pending == nil || call.response.Outcome != nil {
		t.Fatalf("unfinished runner observation invented authority: %+v", call.response)
	}
	if _, done := inFlight.Pending.Result(); done {
		t.Fatal("runner status completed before the fixture gate was released")
	}
	assertRunnerObservationTestNoReceipt(t, fixture)
	call.release()
	waitInspectionWorkerTimeoutSignal(t, joinContext, inFlight.Pending.Done(), "owned runner status completion")
	assertInspectionWorkerTimeoutResult(t, inFlight.Pending)
	assertRunnerObservationTestNoReceipt(t, fixture)
}

func TestRunRunnerObservationExpiryNeverStartsProviderAfterLateStatus(t *testing.T) {
	fixture := newRunnerRefreshFixture(t)
	t.Cleanup(func() { fixture.close(t) })
	prepareRunnerObservationDeadlineTask(t, fixture)
	call := newRunnerObservationTestCall()
	options := pueue.Options{
		Environment: fixture.environment, ObservationTimeout: inspectionWorkerTestObservationTimeout,
		Observer: call.observe,
	}
	if pueue.PrivateControlTimeout(options) >= time.Duration(fixture.req.BudgetNanos) {
		t.Fatal("fixture task budget must own the runner's finite observation deadline")
	}
	var starts atomic.Int32
	providerStarted := make(chan struct{}, 1)
	deps := Dependencies{
		PrepareProfile:    func(task.TaskRecord) (PreparedProfile, error) { return fixture.profile(), nil },
		PredicateRegistry: predicate.Default,
		SupervisorOptions: options,
		ExecutionHooks: execution.Hooks{Start: func(*exec.Cmd) error {
			starts.Add(1)
			select {
			case providerStarted <- struct{}{}:
			default:
			}
			return errors.New("provider Start crossed expired runner observation")
		}},
	}
	var stdout, stderr bytes.Buffer
	var code int
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	registerRunnerObservationTestCleanup(t, ctx, call)
	go func() {
		code = RunRunner([]string{"--root", fixture.store.Root, "--json", fixture.req.TaskID}, &stdout, &stderr, deps)
		close(call.done)
	}()
	waitInspectionWorkerTimeoutSignal(t, ctx, call.entered, "public runner status gate")
	waitInspectionWorkerTimeoutSignal(t, ctx, call.done, "public runner deadline")
	assertRunnerObservationTestExpiredResponse(t, fixture, code, stdout.Bytes(), stderr.Bytes())
	assertRunnerObservationTestNoProviderStart(t, fixture, starts.Load())

	// RunRunner has already returned. Releasing its still-owned status result
	// must not resume startup or create provider authority in the background.
	call.release()
	waitInspectionWorkerTimeoutSignal(t, ctx, call.observerDone, "late public runner status")
	lateContext, cancelLate := context.WithTimeout(ctx, 2*inspectionWorkerTestObservationTimeout)
	defer cancelLate()
	select {
	case <-providerStarted:
		t.Error("late status completion started a provider after RunRunner returned")
	case <-lateContext.Done():
	}
	assertRunnerObservationTestNoProviderStart(t, fixture, starts.Load())
}

func TestRunRunnerUsesDurableReceiptWithoutSupervisorObservation(t *testing.T) {
	fixture := newRunnerRefreshFixture(t)
	t.Cleanup(func() { fixture.close(t) })
	recordRunnerSupervisorReceipt(t, fixture, 7)

	var supervisorCommands atomic.Int32
	var providerStarts atomic.Int32
	result := runRunner(runnerArguments{Root: fixture.store.Root, TaskID: fixture.req.TaskID, Supervised: true}, Dependencies{
		PrepareProfile:    func(task.TaskRecord) (PreparedProfile, error) { return fixture.profile(), nil },
		PredicateRegistry: predicate.Default,
		SupervisorOptions: pueue.Options{
			Environment: fixture.environment,
			Observer:    func(pueue.CommandEvent) { supervisorCommands.Add(1) },
		},
		ExecutionHooks: execution.Hooks{Start: func(*exec.Cmd) error {
			providerStarts.Add(1)
			return errors.New("provider launch reached test guard")
		}},
	})
	if result.code != 0 || result.err != nil || result.response.Outcome == nil {
		t.Fatalf("receipt-backed runner did not reach its provider boundary: %+v", result)
	}
	if result.response.Admission != task.AdmissionAdmitted.String() || result.response.Liveness != task.LivenessRunning.String() {
		t.Fatalf("receipt-backed runner lost admission facts: %+v", result.response)
	}
	if got := supervisorCommands.Load(); got != 0 {
		t.Fatalf("already-running worker issued %d supervisor commands", got)
	}
	if got := providerStarts.Load(); got != 1 {
		t.Fatalf("receipt-backed runner crossed provider Start %d times", got)
	}
}

func TestResumeSupervisedRunnerWaitsForDurableReceipt(t *testing.T) {
	fixture := newRunnerRefreshFixture(t)
	t.Cleanup(func() { fixture.close(t) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	type resumeResult struct {
		client      *pueue.Client
		observation pueue.Observation
		err         error
	}
	started := make(chan struct{})
	done := make(chan resumeResult, 1)
	go func() {
		close(started)
		response := newResponse("run")
		client, observation, err := resumeSupervisedRunner(ctx, &response, fixture.td, pueue.Options{Environment: fixture.environment})
		done <- resumeResult{client: client, observation: observation, err: err}
	}()
	<-started
	timer := time.NewTimer(2 * runnerReceiptPollInterval)
	select {
	case result := <-done:
		timer.Stop()
		t.Fatalf("runner invented admission before receipt publication: %+v", result)
	case <-timer.C:
	}
	recordRunnerSupervisorReceipt(t, fixture, 7)
	select {
	case result := <-done:
		if result.err != nil || result.client == nil || !result.observation.Matched || result.observation.State != pueue.StateRunning || result.observation.NumericTaskID != 7 {
			t.Fatalf("published receipt did not release runner startup: %+v", result)
		}
	case <-ctx.Done():
		t.Fatalf("runner did not consume published receipt: %v", ctx.Err())
	}
}

func TestWaitRunnerSupervisorReceiptStopsAtCallerCancellation(t *testing.T) {
	fixture := newRunnerRefreshFixture(t)
	t.Cleanup(func() { fixture.close(t) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := tasklifecycle.WaitRunnerSupervisorReceipt(ctx, fixture.td, time.Minute); !errors.Is(err, context.Canceled) || !errors.Is(err, pueue.ErrUnknown) {
		t.Fatalf("missing receipt did not retain bounded uncertainty: %v", err)
	}
}

func TestRunRunnerRejectsMalformedSupervisorReceiptBeforeProviderStart(t *testing.T) {
	fixture := newRunnerRefreshFixture(t)
	t.Cleanup(func() { fixture.close(t) })
	if err := os.WriteFile(filepath.Join(fixture.td.Dir, "supervisor.ref.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var providerStarts atomic.Int32
	result := runRunner(runnerArguments{Root: fixture.store.Root, TaskID: fixture.req.TaskID, Supervised: true}, Dependencies{
		PrepareProfile:    func(task.TaskRecord) (PreparedProfile, error) { return fixture.profile(), nil },
		PredicateRegistry: predicate.Default,
		SupervisorOptions: pueue.Options{Environment: fixture.environment},
		ExecutionHooks: execution.Hooks{Start: func(*exec.Cmd) error {
			providerStarts.Add(1)
			return nil
		}},
	})
	if result.code != 1 || result.err == nil {
		t.Fatalf("malformed receipt was accepted: %+v", result)
	}
	if got := providerStarts.Load(); got != 0 {
		t.Fatalf("malformed receipt crossed provider Start %d times", got)
	}
}

func recordRunnerSupervisorReceipt(t *testing.T, fixture *runnerRefreshFixture, numericTaskID int64) {
	t.Helper()
	submit, err := fixture.td.ReadSubmission()
	if err != nil {
		t.Fatal(err)
	}
	id := numericTaskID
	receipt, err := (pueue.Observation{
		Matched: true,
		State:   pueue.StateQueued,
		Identity: pueue.Identity{
			RootID:        submit.RootID,
			TaskID:        submit.TaskID,
			SpecSHA256:    submit.SpecSHA256,
			MetaSHA256:    submit.MetaSHA256,
			NumericTaskID: &id,
		},
		Binding:       submit.Supervisor,
		NumericTaskID: numericTaskID,
	}).Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.td.RecordSupervisorReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
}

type runnerObservationTestCall struct {
	entered      chan struct{}
	observerDone chan struct{}
	done         chan struct{}
	release      func()
	released     <-chan struct{}
	client       *pueue.Client
	observation  pueue.Observation
	response     Response
	err          error
}

func startRunnerObservationTestCall(t *testing.T, ctx context.Context, fixture *runnerRefreshFixture) *runnerObservationTestCall {
	t.Helper()
	_, meta, preparedErr := fixture.td.PreparedRecords()
	if preparedErr != nil {
		t.Fatal(preparedErr)
	}
	call := newRunnerObservationTestCall()
	options := pueue.Options{
		Environment: fixture.environment, ObservationTimeout: inspectionWorkerTestObservationTimeout,
		Observer: call.observe,
	}
	go func() {
		call.client, call.observation, call.err = reconcileRunner(ctx, fixture.store.Root, &call.response, fixture.td, fixture.req, meta, options, "")
		close(call.done)
	}()
	registerRunnerObservationTestCleanup(t, ctx, call)
	return call
}

func newRunnerObservationTestCall() *runnerObservationTestCall {
	released := make(chan struct{})
	return &runnerObservationTestCall{
		entered: make(chan struct{}), observerDone: make(chan struct{}), done: make(chan struct{}),
		release: sync.OnceFunc(func() { close(released) }), released: released, response: newResponse("run"),
	}
}

func (call *runnerObservationTestCall) observe(event pueue.CommandEvent) {
	if event.Stage != "completed" || !slices.Contains(event.Argv, "status") {
		return
	}
	close(call.entered)
	<-call.released
	close(call.observerDone)
}

func registerRunnerObservationTestCleanup(t *testing.T, ctx context.Context, call *runnerObservationTestCall) {
	t.Helper()
	t.Cleanup(func() {
		call.release()
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		waitInspectionWorkerTimeoutSignal(t, cleanupContext, call.done, "runner caller cleanup")
		waitInspectionWorkerTimeoutSignal(t, cleanupContext, call.observerDone, "runner observer cleanup")
		if call.observation.Pending != nil {
			waitInspectionWorkerTimeoutSignal(t, cleanupContext, call.observation.Pending.Done(), "runner pending cleanup")
		}
		if call.client != nil {
			if closeErr := call.client.CloseAfterPending(call.err); closeErr != nil {
				t.Error(closeErr)
			}
		}
	})
}

func prepareRunnerObservationDeadlineTask(t *testing.T, fixture *runnerRefreshFixture) {
	t.Helper()
	_, meta, preparedErr := fixture.td.PreparedRecords()
	if preparedErr != nil {
		t.Fatal(preparedErr)
	}
	brief, briefErr := fixture.td.ReadBrief()
	if briefErr != nil {
		t.Fatal(briefErr)
	}
	// Create a separate immutable task, keeping the original fixture records
	// intact and reusing its bound supervisor and valid provider profile.
	request := *fixture.req
	request.TaskID = strings.Repeat("9", 32)
	request.BudgetNanos = int64(time.Second)
	request.RequestedConfig.Budget = time.Second.String()
	meta.TaskID, meta.RequestedConfig = request.TaskID, request.RequestedConfig
	if closeErr := fixture.td.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	td, createErr := fixture.store.CreateTask(request.TaskID, &request, brief, meta)
	if createErr != nil {
		t.Fatal(createErr)
	}
	fixture.td, fixture.req = td, &request
	permit, permitErr := td.PrepareSubmission(fixture.supervisor.Binding())
	if permitErr != nil {
		t.Fatal(permitErr)
	}
	if releaseErr := permit.Release(); releaseErr != nil {
		t.Fatal(releaseErr)
	}
	writeRunnerStatus(t, filepath.Join(fixture.base, "status.json"), fixture.store.RootID, request.TaskID, 7)
}

func assertRunnerObservationTestExpiredResponse(t *testing.T, fixture *runnerRefreshFixture, code int, stdout, stderr []byte) {
	t.Helper()
	if code != 1 || len(stderr) != 0 {
		t.Fatalf("expired public runner exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	var response Response
	if decodeErr := json.Unmarshal(stdout, &response); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if response.RootID != fixture.store.RootID || response.TaskID != fixture.req.TaskID {
		t.Fatalf("public runner lost the saved task identity: %+v", response)
	}
	if response.Admission != task.AdmissionUnknown.String() || response.Pending == nil || response.Outcome != nil || !strings.Contains(response.Error, pueue.ErrInFlight.Error()) {
		t.Fatalf("expired public runner invented admission or lost uncertainty: %+v", response)
	}
}

func assertRunnerObservationTestNoProviderStart(t *testing.T, fixture *runnerRefreshFixture, starts int32) {
	t.Helper()
	if starts != 0 {
		t.Errorf("expired runner reached the provider Start hook %d times", starts)
	}
	assertRunnerObservationTestNoReceipt(t, fixture)
	for _, name := range []string{"provider.start", "provider.started.json", "provider.exit", "outcome.json"} {
		if _, statErr := os.Stat(filepath.Join(fixture.td.Dir, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("expired runner created %s: %v", name, statErr)
		}
	}
}

func assertRunnerObservationTestNoReceipt(t *testing.T, fixture *runnerRefreshFixture) {
	t.Helper()
	if receipt, receiptErr := fixture.td.ReadSupervisorReceipt(); !errors.Is(receiptErr, os.ErrNotExist) {
		t.Fatalf("unfinished runner observation published a receipt: receipt=%+v err=%v", receipt, receiptErr)
	}
}

func assertRunnerObservationTestReceipt(t *testing.T, fixture *runnerRefreshFixture, numericTaskID int64) {
	t.Helper()
	receipt, err := fixture.td.ReadSupervisorReceipt()
	if err != nil || receipt.NumericTaskID != numericTaskID {
		t.Fatalf("durable runner receipt = %+v err=%v, want task %d", receipt, err, numericTaskID)
	}
}

func assertRunnerObservationTestAdmitted(t *testing.T, fixture *runnerRefreshFixture, call *runnerObservationTestCall) {
	t.Helper()
	if !call.observation.Matched || call.observation.State != pueue.StateRunning || call.observation.NumericTaskID != 7 || call.observation.Pending != nil {
		t.Fatalf("runner did not match the completed running row: %+v", call.observation)
	}
	if call.response.Admission != task.AdmissionAdmitted.String() || call.response.Pending != nil || call.response.Outcome != nil {
		t.Fatalf("runner response did not retain only observed admission: %+v", call.response)
	}
	submission, submitErr := fixture.td.ReadSubmission()
	if submitErr != nil {
		t.Fatal(submitErr)
	}
	receipt, receiptErr := fixture.td.ReadSupervisorReceipt()
	if receiptErr != nil {
		t.Fatal(receiptErr)
	}
	if receipt.NumericTaskID != 7 || receipt.Label != submission.Label || receipt.SpecSHA256 != submission.SpecSHA256 || receipt.MetaSHA256 != submission.MetaSHA256 {
		t.Fatalf("runner receipt did not preserve its exact saved submission: receipt=%+v submission=%+v", receipt, submission)
	}
}
