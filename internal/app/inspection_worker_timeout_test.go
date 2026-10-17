package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const inspectionWorkerTestObservationTimeout = 25 * time.Millisecond

func TestResumeSupervisedInspectionWorkerUsesReceiptWithoutQueueSnapshot(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, _, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	operation := newInspectionPollTestOperation(t, store, bound)
	ctx, cancel := context.WithDeadline(t.Context(), operation.Deadline())
	defer cancel()
	if err := operation.RecordReceiptContext(ctx, 7); err != nil {
		t.Fatal(err)
	}
	client, identity, err := resumeSupervisedInspectionWorker(ctx, operation, operation.Request().Binding.Supervisor, pueue.Options{
		Observer: func(event pueue.CommandEvent) {
			t.Fatalf("supervised inspection launched a Pueue command: %+v", event)
		},
	})
	if err != nil || client == nil {
		t.Fatalf("supervised inspection did not consume its receipt: client=%v err=%v", client, err)
	}
	if closeErr := client.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if identity.NumericTaskID == nil || *identity.NumericTaskID != 7 || identity.RootID != store.RootID || identity.TaskID != operation.Request().TaskID {
		t.Fatalf("supervised inspection lost receipt identity: %+v", identity)
	}
}

func TestResumeManagedSupervisedInspectionWorkerWaitsForReceipt(t *testing.T) {
	fixture := newInspectionWorkerStartupFixture(t, task.RunnerOwnershipManaged)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	type resumeResult struct {
		client   *pueue.Client
		identity pueue.InspectionIdentity
		err      error
	}
	var commands atomic.Int32
	started := make(chan struct{})
	done := make(chan resumeResult, 1)
	go func() {
		close(started)
		client, identity, err := resumeSupervisedInspectionWorker(ctx, fixture.operation, fixture.operation.Request().Binding.Supervisor, pueue.Options{
			Observer: func(pueue.CommandEvent) { commands.Add(1) },
		})
		done <- resumeResult{client: client, identity: identity, err: err}
	}()
	<-started

	timer := time.NewTimer(2 * runnerReceiptPollInterval)
	defer timer.Stop()
	select {
	case result := <-done:
		closeInspectionWorkerTestClient(t, result.client)
		t.Fatalf("managed worker invented startup before receipt publication: %+v", result)
	case <-timer.C:
	}
	if err := fixture.operation.RecordReceiptContext(ctx, 41); err != nil {
		t.Fatal(err)
	}

	select {
	case result := <-done:
		if result.client == nil || result.err != nil {
			t.Fatalf("published receipt did not release managed worker: %+v", result)
		}
		if closeErr := result.client.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if result.identity.NumericTaskID == nil || *result.identity.NumericTaskID != 41 || result.identity.RootID != fixture.store.RootID || result.identity.TaskID != fixture.operation.Request().TaskID {
			t.Fatalf("managed worker did not resume the exact receipt identity: %+v", result.identity)
		}
	case <-ctx.Done():
		t.Fatalf("managed worker did not consume published receipt: %v", ctx.Err())
	}
	if got := commands.Load(); got != 0 {
		t.Fatalf("managed receipt startup issued %d Pueue commands", got)
	}
}

func TestSupervisedInspectionMarkerReconcilesNonManagedBindings(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ownership string
	}{
		{name: "legacy", ownership: ""},
		{name: "custom", ownership: task.RunnerOwnershipCustom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testSupervisedInspectionMarkerReconcilesNonManagedBinding(t, tc.ownership)
		})
	}
}

func TestGuardianInspectionWorkerRejectsMissingAuthenticatedChild(t *testing.T) {
	fixture := newInspectionWorkerStartupFixtureWithExitProof(t, task.RunnerOwnershipManaged, inspection.WorkerExitProofGuardianWaitV1)
	if err := fixture.operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	var nativeStarts atomic.Int32
	err := runInspectionWorkerOperationMode(fixture.store.Root, fixture.operation.Request().TaskID, true, Dependencies{
		PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
			nativeStarts.Add(1)
			return commonprovider.ProfileCandidate{}, errors.New("unexpected native inspection")
		},
	}, fixture.store)
	if inspectionWorkerFailureStage(err, "unknown") != "open-lifetime" || nativeStarts.Load() != 0 {
		t.Fatalf("guardian-contract worker accepted an unauthenticated launch: error=%v native_starts=%d", err, nativeStarts.Load())
	}
	assertInspectionWorkerStartUnclaimed(t, fixture.store, fixture.operation.Request().TaskID)
	log, err := os.ReadFile(fixture.logPath)
	if err != nil || len(log) != 0 {
		t.Fatalf("unauthenticated worker touched its supervisor: log=%q error=%v", log, err)
	}
}

func testSupervisedInspectionMarkerReconcilesNonManagedBinding(t *testing.T, ownership string) {
	t.Helper()
	fixture := newInspectionWorkerStartupFixture(t, ownership)
	taskID := fixture.operation.Request().TaskID
	if err := fixture.operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if closeErr := fixture.operation.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err := os.WriteFile(filepath.Join(fixture.store.Root, ".supervisor", "bootstrap.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var nativeStarts atomic.Int32
	err := runInspectionWorkerOperationMode(fixture.store.Root, taskID, true, Dependencies{
		PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
			nativeStarts.Add(1)
			return commonprovider.ProfileCandidate{}, errors.New("native inspection crossed reconciliation guard")
		},
		SupervisorOptions: pueue.Options{
			Environment:        fixture.supervisorEnvironment,
			ObservationTimeout: time.Second,
		},
	}, fixture.store)
	if !errors.Is(err, admissioncore.ErrInspectionWorkerUnavailable) {
		t.Fatalf("marker bypass returned %v, want fixed worker error", err)
	}
	if got := nativeStarts.Load(); got != 0 {
		t.Fatalf("non-managed marker reached native inspection %d times", got)
	}
	assertInspectionWorkerStartUnclaimed(t, fixture.store, taskID)

	data, readErr := os.ReadFile(fixture.logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if calls := strings.Count(string(data), "arg=status\n"); calls != 1 {
		t.Fatalf("non-managed marker skipped exact reconciliation: status calls=%d log=%s", calls, data)
	}
}

func closeInspectionWorkerTestClient(t *testing.T, client *pueue.Client) {
	t.Helper()
	if client == nil {
		return
	}
	if closeErr := client.Close(); closeErr != nil {
		t.Error(closeErr)
	}
}

func TestReconcileInspectionWorkerDoesNotRecoverItsOwnPrivateSupervisor(t *testing.T) {
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	operation := newInspectionPollTestOperation(t, store, bound)
	if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	writeInspectionPollTestStatus(t, statusPath, operation, false)
	if err := os.WriteFile(filepath.Join(store.Root, ".supervisor", "bootstrap.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithDeadline(t.Context(), operation.Deadline())
	defer cancel()
	supervisor, identity, err := reconcileInspectionWorker(ctx, operation, operation.Request().Binding.Supervisor, pueue.Options{
		Environment:        append(os.Environ(), "FAKE_LOG="+logPath, "FAKE_STATUS="+statusPath),
		ObservationTimeout: time.Second,
	})
	if err != nil || supervisor == nil {
		t.Fatalf("worker could not observe its running private row: supervisor=%v err=%v", supervisor, err)
	}
	if closeErr := supervisor.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if identity.NumericTaskID == nil || *identity.NumericTaskID != 7 {
		t.Fatalf("worker did not retain its saved private row: %+v", identity)
	}

	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	log := string(data)
	if calls := strings.Count(log, "---\n"); calls != 1 || strings.Count(log, "arg=status\n") != 1 {
		t.Fatalf("worker did not make one bounded status observation: calls=%d log=%s", calls, log)
	}
	if strings.Contains(log, "arg=--version\n") || strings.Contains(log, "arg=shutdown\n") {
		t.Fatalf("worker attempted private-supervisor recovery: %s", log)
	}
}

func TestReconcileInspectionWorkerUsesInspectionObservationWindow(t *testing.T) {
	fixture := newInspectionWorkerTimeoutFixture(t)
	deadline := fixture.operation.Deadline()
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	call := fixture.start(t, ctx)
	waitInspectionWorkerTimeoutSignal(t, ctx, fixture.entered, "status completion gate")

	// This is a negative wait at the supplied observation boundary. The
	// operation keeps its original admission deadline throughout the call.
	faultContext, cancelFault := context.WithTimeout(ctx, 2*inspectionWorkerTestObservationTimeout)
	defer cancelFault()
	select {
	case <-call.done:
		t.Errorf("worker abandoned valid status within its inspection window: %v", call.err)
	case <-faultContext.Done():
	}
	fixture.release()
	waitInspectionWorkerTimeoutSignal(t, ctx, call.done, "worker reconciliation")
	if call.err != nil || call.client == nil {
		t.Fatalf("delayed valid status prevented worker startup: client=%v err=%v", call.client, call.err)
	}
	if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) || !fixture.operation.Deadline().Equal(deadline) || ctx.Err() != nil {
		t.Fatalf("reconciliation changed or exhausted the admission deadline: deadline=%v context=%v err=%v", deadline, got, ctx.Err())
	}
	if call.identity.NumericTaskID == nil || *call.identity.NumericTaskID != 7 {
		t.Fatalf("worker did not retain the exact running job: %+v", call.identity)
	}
	receipt, receiptErr := fixture.operation.ReceiptContext(ctx)
	if receiptErr != nil || receipt.NumericTaskID == nil || *receipt.NumericTaskID != 7 {
		t.Fatalf("worker startup did not record its exact receipt: receipt=%+v err=%v", receipt, receiptErr)
	}
}

func TestReconcileInspectionWorkerRetainsPendingAtContextDeadline(t *testing.T) {
	fixture := newInspectionWorkerTimeoutFixture(t)
	deadline := fixture.operation.Deadline()
	ctx, cancel := testsupport.NewControllableDeadlineContext(t.Context(), 10*time.Second)
	defer cancel()
	call := fixture.start(t, ctx)
	readyContext, cancelReady := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelReady()
	waitInspectionWorkerTimeoutSignal(t, readyContext, fixture.entered, "status completion gate")
	ctx.Expire()
	joinContext, cancelJoin := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelJoin()
	waitInspectionWorkerTimeoutSignal(t, joinContext, call.done, "expired worker reconciliation")

	var inFlight *pueue.InFlightError
	if !errors.Is(call.err, pueue.ErrInFlight) || !errors.As(call.err, &inFlight) || inFlight.Pending == nil {
		t.Fatalf("worker discarded the unfinished status owner at its deadline: %v", call.err)
	}
	if call.client != nil || call.identity.NumericTaskID != nil || !fixture.operation.Deadline().Equal(deadline) || fixture.operation.Expired(time.Now()) {
		t.Fatalf("caller expiry changed the worker operation: client=%v identity=%+v deadline=%v", call.client, call.identity, fixture.operation.Deadline())
	}
	if _, done := inFlight.Pending.Result(); done {
		t.Fatal("pending status completed before the fixture gate was released")
	}
	if receipt, receiptErr := fixture.operation.ReceiptContext(joinContext); receiptErr != nil || receipt.NumericTaskID == nil || *receipt.NumericTaskID != 7 {
		t.Fatalf("unfinished status changed the staged startup receipt: receipt=%+v err=%v", receipt, receiptErr)
	}
	fixture.release()
	waitInspectionWorkerTimeoutSignal(t, joinContext, inFlight.Pending.Done(), "owned status completion")
	assertInspectionWorkerTimeoutResult(t, inFlight.Pending)
}

func TestReconcileInspectionWorkerPreservesReturnedClientStopTimeout(t *testing.T) {
	fixture := newInspectionWorkerTimeoutFixture(t)
	// Startup may finish normally; hold only the returned client's subsequent
	// stop-routing observation. Both calls use the same saved client options.
	fixture.release()
	startupObserver := fixture.options.Observer
	stopEntered, stopObserved := make(chan struct{}), make(chan struct{})
	stopReleased := make(chan struct{})
	releaseStop := sync.OnceFunc(func() { close(stopReleased) })
	var observations atomic.Int32
	fixture.options.Observer = func(event pueue.CommandEvent) {
		if event.Stage != "completed" || !slices.Contains(event.Argv, "status") {
			return
		}
		switch observations.Add(1) {
		case 1:
			startupObserver(event)
		case 2:
			close(stopEntered)
			<-stopReleased
			close(stopObserved)
		}
	}
	ctx, cancel := context.WithDeadline(t.Context(), fixture.operation.Deadline())
	defer cancel()
	call := fixture.start(t, ctx)
	waitInspectionWorkerTimeoutSignal(t, ctx, call.done, "worker startup before stop")
	if call.err != nil || call.client == nil {
		t.Fatalf("worker startup failed before stop timing could be tested: %v", call.err)
	}
	permit, _, permitErr := fixture.operation.ClaimStop(fixture.operation.Deadline())
	if permitErr != nil {
		t.Fatal(permitErr)
	}
	var stopped pueue.StopResult
	t.Cleanup(func() {
		releaseStop()
		cleanupContext, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancelCleanup()
		waitInspectionWorkerTimeoutSignal(t, cleanupContext, stopObserved, "stop observer cleanup")
		if stopped.Pending != nil {
			waitInspectionWorkerTimeoutSignal(t, cleanupContext, stopped.Pending.Done(), "stop command cleanup")
		}
	})

	// A client accidentally widened to the inspection timeout can return only
	// when this fault deadline expires. The original 25ms client must return
	// with a pending owner while the caller still has time remaining.
	stopContext, cancelStop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancelStop()
	var stopErr error
	stopped, stopErr = call.client.StopInspection(stopContext, permit, call.identity)
	if stopContext.Err() != nil {
		t.Errorf("inspection observation widened the returned client's stop timeout: %v", stopContext.Err())
	}
	waitInspectionWorkerTimeoutSignal(t, ctx, stopEntered, "stop routing gate")
	assertInspectionWorkerTimeoutStop(t, stopped, stopErr)
	if got := observations.Load(); got != 2 {
		t.Errorf("stop observation overlapped or retried: status completions=%d want=2", got)
	}
	releaseStop()
	waitInspectionWorkerTimeoutSignal(t, ctx, stopped.Pending.Done(), "stop routing completion")
	assertInspectionWorkerTimeoutResult(t, stopped.Pending)
}

type inspectionWorkerStartupFixture struct {
	store                 *taskdir.Store
	operation             *inspection.Operation
	logPath               string
	supervisorEnvironment []string
}

func newInspectionWorkerStartupFixture(t *testing.T, runnerOwnership string) *inspectionWorkerStartupFixture {
	t.Helper()
	return newInspectionWorkerStartupFixtureWithExitProof(t, runnerOwnership, "")
}

func newInspectionWorkerStartupFixtureWithExitProof(t *testing.T, runnerOwnership, workerExitProof string) *inspectionWorkerStartupFixture {
	t.Helper()
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	if closeErr := bound.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	worker := canonicalAppTestExecutable(t)
	workerSHA, err := commonprovider.FingerprintExecutable(worker)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	helperSHA, err := commonprovider.FingerprintExecutable(helper)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := inspection.OpenOperation(store, newBareInspectionRequest(t, store), inspection.Binding{
		DefinitionRevision: "inspection-v1",
		DefinitionSHA256:   strings.Repeat("a", 64),
		HelperExecutable:   helper,
		HelperSHA256:       helperSHA,
		WorkerExecutable:   worker,
		WorkerSHA256:       workerSHA,
		RunnerOwnership:    runnerOwnership,
		WorkerExitProof:    workerExitProof,
		Supervisor:         bound.Binding(),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = validateInspectionWorkerStartup(t.Context(), store.Root, operation, operation.Request().Binding); err != nil {
		t.Fatalf("invalid inspection-worker startup fixture: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return &inspectionWorkerStartupFixture{
		store:     store,
		operation: operation,
		logPath:   logPath,
		supervisorEnvironment: append(os.Environ(),
			"FAKE_LOG="+logPath,
			"FAKE_STATUS="+filepath.Join(filepath.Dir(logPath), "status.json"),
		),
	}
}

func assertInspectionWorkerStartUnclaimed(t *testing.T, store *taskdir.Store, taskID string) {
	t.Helper()
	control, err := store.OpenInspection(taskID, false)
	if err != nil {
		t.Fatal(err)
	}
	var start inspection.StartRecord
	readErr := control.Read("start.json", &start)
	if closeErr := control.Close(); closeErr != nil {
		t.Error(closeErr)
	}
	if !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("non-managed marker claimed worker start: start=%+v err=%v", start, readErr)
	}
}

type inspectionWorkerTimeoutFixture struct {
	root         string
	operation    *inspection.Operation
	options      pueue.Options
	entered      chan struct{}
	observerDone chan struct{}
	release      func()
}

type inspectionWorkerTimeoutCall struct {
	done     chan struct{}
	client   *pueue.Client
	identity pueue.InspectionIdentity
	err      error
}

func newInspectionWorkerTimeoutFixture(t *testing.T) *inspectionWorkerTimeoutFixture {
	t.Helper()
	store := newBareInspectionStore(t)
	bound, logPath, _ := newUncertainGroupSupervisor(t, store.RootID)
	operation := newInspectionPollTestOperation(t, store, bound)
	if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	writeInspectionPollTestStatus(t, statusPath, operation, false)
	released := make(chan struct{})
	fixture := &inspectionWorkerTimeoutFixture{
		root: store.Root, operation: operation,
		entered: make(chan struct{}), observerDone: make(chan struct{}),
		release: sync.OnceFunc(func() { close(released) }),
	}
	fixture.options = pueue.Options{
		Environment:        append(os.Environ(), "FAKE_LOG="+logPath, "FAKE_STATUS="+statusPath),
		ObservationTimeout: inspectionWorkerTestObservationTimeout,
		Observer: func(event pueue.CommandEvent) {
			if event.Stage != "completed" || !slices.Contains(event.Argv, "status") {
				return
			}
			// The real command has passed Wait and capture. Hold publication of
			// Pending.Done so the caller must use its intended observation window.
			close(fixture.entered)
			<-released
			close(fixture.observerDone)
		},
	}
	return fixture
}

func (fixture *inspectionWorkerTimeoutFixture) start(t *testing.T, ctx context.Context) *inspectionWorkerTimeoutCall {
	t.Helper()
	call := &inspectionWorkerTimeoutCall{done: make(chan struct{})}
	go func() {
		call.client, call.identity, call.err = reconcileInspectionWorker(ctx, fixture.operation, fixture.operation.Request().Binding.Supervisor, fixture.options)
		close(call.done)
	}()
	t.Cleanup(func() {
		fixture.release()
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		waitInspectionWorkerTimeoutSignal(t, cleanupContext, call.done, "worker cleanup")
		waitInspectionWorkerTimeoutSignal(t, cleanupContext, fixture.observerDone, "status observer cleanup")
		if call.client != nil {
			if closeErr := call.client.CloseAfterPending(call.err); closeErr != nil {
				t.Error(closeErr)
			}
		}
	})
	return call
}

func waitInspectionWorkerTimeoutSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, stage string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("timed out awaiting %s: %v", stage, ctx.Err())
	}
}

func assertInspectionWorkerTimeoutResult(t *testing.T, pending *pueue.Pending) {
	t.Helper()
	result, done := pending.Result()
	if !done || !result.Started || result.PID <= 0 || result.ExitCode != 0 || result.Err != nil {
		t.Fatalf("status did not retain its natural completion: result=%+v done=%t", result, done)
	}
}

func assertInspectionWorkerTimeoutStop(t *testing.T, result pueue.StopResult, stopErr error) {
	t.Helper()
	var inFlight *pueue.InFlightError
	if !errors.Is(stopErr, pueue.ErrInFlight) || !errors.As(stopErr, &inFlight) || inFlight.Pending == nil || result.Pending != inFlight.Pending {
		t.Fatalf("stop did not retain its unfinished routing observation: result=%+v err=%v", result, stopErr)
	}
	if !result.Requested || !result.InFlight || result.Attempted || result.Acknowledged != nil {
		t.Fatalf("unfinished routing observation acquired stop authority: %+v", result)
	}
	if result.NumericTaskID == nil || *result.NumericTaskID != 7 {
		t.Fatalf("stop changed the worker's saved numeric target: %+v", result)
	}
	if _, done := result.Pending.Result(); done {
		t.Fatal("stop routing completed before its fixture gate was released")
	}
}
