package guardian

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestLifetimeGuardianRefusesTerminalReplayBeforeScopeOrWorkerStart(t *testing.T) {
	for _, ownerKind := range []string{task.LifetimeOwnerTask, task.LifetimeOwnerInspection} {
		t.Run(ownerKind, func(t *testing.T) {
			assertLifetimeGuardianRefusesTerminalReplay(t, ownerKind)
		})
	}
}

func assertLifetimeGuardianRefusesTerminalReplay(t *testing.T, ownerKind string) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		if closeErr := serverConn.Close(); closeErr != nil {
			t.Errorf("close server connection: %v", closeErr)
		}
	})
	t.Cleanup(func() {
		if closeErr := clientConn.Close(); closeErr != nil {
			t.Errorf("close client connection: %v", closeErr)
		}
	})
	timeline := &lifetimeGuardianTestTimeline{}
	owner := &lifetimeGuardianTestOwner{timeline: timeline}
	records := &lifetimeGuardianTestRecords{
		timeline:            timeline,
		existingTermination: &task.LifetimeTerminationRecord{ScopeEmpty: true},
	}
	scopeCalls := 0
	startCalls := 0
	guardian, err := New(Options{
		RootID: lifetimeGuardianTestRootID, TaskID: lifetimeGuardianTestTaskID,
		OwnerKind: ownerKind, Deadline: time.Now().Add(time.Hour), Records: records,
		AcquireOwner:   func(context.Context) (Owner, error) { return owner, nil },
		ReconcileOwned: func(context.Context) error { return nil },
		Worker:         exec.Command("unused-lifetime-worker"), Connection: serverConn,
		ScopeFactory: func(lifetime.Plan) (Scope, error) {
			scopeCalls++
			return nil, errors.New("scope must not be created")
		},
		StartWorker: func(*exec.Cmd) error {
			startCalls++
			return errors.New("worker must not start")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = guardian.Run(t.Context()); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("Run error = %v, want terminal task", err)
	}
	if scopeCalls != 0 || startCalls != 0 || len(records.intents) != 0 {
		t.Fatalf("terminal replay created work: scope=%d start=%d intents=%d", scopeCalls, startCalls, len(records.intents))
	}
	if owner.releases != 1 {
		t.Fatalf("owner releases = %d, want 1", owner.releases)
	}
}

func TestLifetimeGuardianRefusesFreshExpiredWorkBeforeCreatingScope(t *testing.T) {
	for _, ownerKind := range []string{task.LifetimeOwnerTask, task.LifetimeOwnerInspection} {
		t.Run(ownerKind, func(t *testing.T) {
			assertLifetimeGuardianRefusesFreshExpiredWork(t, ownerKind)
		})
	}
}

func assertLifetimeGuardianRefusesFreshExpiredWork(t *testing.T, ownerKind string) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		if closeErr := serverConn.Close(); closeErr != nil {
			t.Errorf("close server connection: %v", closeErr)
		}
	})
	t.Cleanup(func() {
		if closeErr := clientConn.Close(); closeErr != nil {
			t.Errorf("close client connection: %v", closeErr)
		}
	})
	timeline := &lifetimeGuardianTestTimeline{}
	owner := &lifetimeGuardianTestOwner{timeline: timeline}
	records := &lifetimeGuardianTestRecords{timeline: timeline, ownerKind: ownerKind}
	scopeCalls := 0
	startCalls := 0
	deadline := time.Now().Add(-time.Second)
	guardian, err := New(Options{
		RootID: lifetimeGuardianTestRootID, TaskID: lifetimeGuardianTestTaskID,
		OwnerKind: ownerKind, Deadline: deadline, Records: records,
		AcquireOwner:   func(context.Context) (Owner, error) { return owner, nil },
		ReconcileOwned: func(context.Context) error { return nil },
		Worker:         exec.Command("unused-lifetime-worker"), Connection: serverConn,
		ScopeFactory: func(lifetime.Plan) (Scope, error) {
			scopeCalls++
			return nil, errors.New("scope must not be created")
		},
		StartWorker: func(*exec.Cmd) error {
			startCalls++
			return errors.New("worker must not start")
		},
		Now: func() time.Time { return deadline },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = guardian.Run(t.Context()); !errors.Is(err, errLifetimeGuardianDeadline) {
		t.Fatalf("Run error = %v, want deadline", err)
	}
	if scopeCalls != 0 || startCalls != 0 || len(records.intents) != 0 || len(records.registrations) != 0 {
		t.Fatalf("expired work created state: scope=%d start=%d intents=%d registrations=%d",
			scopeCalls, startCalls, len(records.intents), len(records.registrations))
	}
	if owner.releases != 1 {
		t.Fatalf("owner releases = %d, want 1", owner.releases)
	}
}

func TestLifetimeGuardianRejectsLaunchBeyondSharedLimitBeforeAck(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	for sequence := uint64(1); sequence <= task.MaxLifetimeLaunches; sequence++ {
		f.callMust(t, "begin", f.message(lifetimeMessageBegin, sequence, 0))
		f.callMust(t, "failed", f.message(lifetimeMessageFailed, sequence, 0))
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, task.MaxLifetimeLaunches+1, 0)); err == nil {
		t.Fatal("launch beyond shared limit was acknowledged")
	}
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestLifetimeGuardianDoesNotDestroyBeforeTerminationIsDurable(t *testing.T) {
	wantErr := errors.New("termination write failed")
	retryEntered := make(chan struct{})
	retryRelease := make(chan struct{})
	var retryOnce sync.Once
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.terminationErr = wantErr
			options.FinalizationBackoff = func(ctx context.Context, _ time.Duration) error {
				retryOnce.Do(func() { close(retryEntered) })
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-retryRelease:
					records.mu.Lock()
					records.terminationErr = nil
					records.mu.Unlock()
					return nil
				}
			}
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	if err := f.call(t, f.message(lifetimeMessageClose, 0, 0)); err == nil {
		t.Fatal("close succeeded despite termination write failure")
	}
	f.worker.release()
	waitForLifetimeGuardianTerminationRetry(t, retryEntered)
	destroy, release := f.scope.disposalCounts()
	if destroy != 0 || release != 0 {
		t.Fatalf("scope disposal before durable termination = destroy %d release %d, want 0/0", destroy, release)
	}
	events := f.timeline.snapshot()
	if timelineIndex(events, "scope-destroy") >= 0 || timelineIndex(events, "release") >= 0 {
		t.Fatalf("scope or owner was released before durable termination: %v", events)
	}
	close(retryRelease)
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want original termination failure", err)
	}
	if destroy, release = f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("scope disposal after durable termination = destroy %d release %d, want 1/1", destroy, release)
	}
	assertFrozenLifetimeGuardianTerminationCause(t, f.records.terminationCauses())
	assertTimelineOrder(t, f.timeline.snapshot(), "termination-record", "scope-destroy", "scope-release", "release")
}

func waitForLifetimeGuardianTerminationRetry(t *testing.T, retryEntered <-chan struct{}) {
	t.Helper()
	select {
	case <-retryEntered:
	case <-time.After(time.Second):
		t.Fatal("guardian did not retain authority for termination retry")
	}
}

func assertFrozenLifetimeGuardianTerminationCause(t *testing.T, causes []string) {
	t.Helper()
	if len(causes) < 2 {
		t.Fatalf("termination retry attempts = %v, want at least two", causes)
	}
	for _, cause := range causes[1:] {
		if cause != causes[0] {
			t.Fatalf("termination retry changed its frozen cause: %v", causes)
		}
	}
}

func TestLifetimeGuardianAcknowledgesAmbiguousTerminationWrite(t *testing.T) {
	wantErr := errors.New("termination acknowledgement lost")
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			records := lifetimeGuardianTestRecordsFromOptions(t, options)
			records.terminationErrors = []error{wantErr}
			records.terminationCommitOnError = true
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("Run after ambiguous committed termination: %v", err)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("ambiguous committed termination disposal = destroy %d release %d, want 1/1", destroy, release)
	}
}

func TestLifetimeGuardianDeadlineFencesBegin(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	f.setNow(time.Now().Add(2 * time.Hour))
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err == nil {
		t.Fatal("begin after deadline was accepted")
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil && !errors.Is(err, errLifetimeGuardianDeadline) {
		t.Fatalf("run: %v", err)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationBudget {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationBudget)
	}
	start, wait := f.worker.counts()
	if start != 1 || wait != 1 {
		t.Fatalf("worker calls = start %d wait %d, want 1/1", start, wait)
	}
}

func TestLifetimeGuardianFailedStartClearsAuthorization(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err != nil {
		t.Fatalf("begin 1: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageFailed, 1, 0)); err != nil {
		t.Fatalf("failed 1: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 2, 0)); err != nil {
		t.Fatalf("begin 2: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageFailed, 2, 0)); err != nil {
		t.Fatalf("failed 2: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageClose, 0, 0)); err != nil {
		t.Fatalf("close: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(f.records.launches); got != 0 {
		t.Fatalf("launch records = %d, want 0", got)
	}
}

func TestLifetimeGuardianLaunchRecordUsesWorkContext(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	f.callMust(t, "started", f.message(lifetimeMessageStarted, 1, 404))

	f.records.mu.Lock()
	launchDeadline := f.records.launchContextDeadline
	f.records.mu.Unlock()
	if launchDeadline.IsZero() || !launchDeadline.Equal(f.guardian.options.Deadline) {
		t.Fatalf("launch record deadline = %v, want guardian deadline %v", launchDeadline, f.guardian.options.Deadline)
	}

	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestLifetimeGuardianLaunchRecordFailureCleansBeforeRefusal(t *testing.T) {
	wantErr := errors.New("launch record write failed")
	assertLifetimeGuardianLaunchRecordFailure(t, wantErr, func(records *lifetimeGuardianTestRecords) {
		records.launchErr = wantErr
	})
}

func TestLifetimeGuardianInvalidLaunchRecordCleansBeforeRefusal(t *testing.T) {
	assertLifetimeGuardianLaunchRecordFailure(t, errLifetimeGuardianLaunchRecord, func(records *lifetimeGuardianTestRecords) {
		records.invalidLaunchRecord = true
	})
}

func assertLifetimeGuardianLaunchRecordFailure(t *testing.T, wantErr error, configure func(*lifetimeGuardianTestRecords)) {
	t.Helper()
	f := newLifetimeGuardianTestFixture(t)
	f.scope.emptyResults = []bool{false, true}
	configure(f.records)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	if err := f.call(t, f.message(lifetimeMessageStarted, 1, 404)); err == nil {
		t.Fatal("started was accepted despite an invalid launch record")
	}
	f.worker.release()
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("run error = %v, want %v", err, wantErr)
	}
	if causes := f.records.terminationCauses(); len(causes) != 1 {
		t.Fatalf("termination causes = %v, want one recovery record", causes)
	}
	events := f.timeline.snapshot()
	assertTimelineOrder(t, events, "launch-record", "scope-terminate", "termination-record")
	if f.guardian.authorizedSequence != 0 {
		t.Fatalf("authorized sequence = %d, want cleared after guardian cleanup", f.guardian.authorizedSequence)
	}
	f.scope.mu.Lock()
	pendingProofCalls := f.scope.pendingStartEmptyCalls
	terminateCalls := f.scope.terminateCalls
	f.scope.mu.Unlock()
	if pendingProofCalls != 0 {
		t.Fatalf("pending-start proof calls = %d, want 0 after guardian cleanup", pendingProofCalls)
	}
	if terminateCalls != 1 {
		t.Fatalf("guardian terminate calls = %d, want 1", terminateCalls)
	}
}

func TestLifetimeGuardianWorkerStartFailureRecovers(t *testing.T) {
	startErr := errors.New("worker start failed")
	f := newLifetimeGuardianTestFixtureWithStartError(t, startErr)
	if err := f.waitRun(t); !errors.Is(err, startErr) {
		t.Fatalf("run error = %v, want worker start failure", err)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationRecovery {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationRecovery)
	}
	start, wait := f.worker.counts()
	if start != 1 || wait != 0 {
		t.Fatalf("worker calls = start %d wait %d, want 1/0", start, wait)
	}
	waits := f.records.recordedWorkerWaits()
	if len(waits) != 1 || waits[0].Result != task.LifetimeWorkerWaitNotStarted || waits[0].WorkerSHA256 != "" || waits[0].ChildPID != 0 || waits[0].ExitCode != nil {
		t.Fatalf("worker completion dispositions = %+v, want one not-started record", waits)
	}
}

func TestLifetimeGuardianStopTerminatesBeforeAck(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.scope.emptyResults = []bool{false, true}
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageStarted, 1, 202)); err != nil {
		t.Fatalf("started: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageStop, 0, 0)); err != nil {
		t.Fatalf("stop: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	_, terminate, _ := f.scope.counts()
	if terminate != 1 {
		t.Fatalf("terminate calls = %d, want 1", terminate)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationBudget {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationBudget)
	}
}

func TestLifetimeGuardianAcceptsConcurrentCleanupOnlyAfterEmptyRecheck(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.scope.emptyResults = []bool{false, true}
	f.scope.terminateErr = lifetime.ErrUnverifiableLaunch
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageStarted, 1, 212)); err != nil {
		t.Fatalf("started: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageStop, 0, 0)); err != nil {
		t.Fatalf("stop after concurrent cleanup: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	empty, terminate, _ := f.scope.counts()
	if empty != 2 || terminate != 1 {
		t.Fatalf("scope calls empty=%d terminate=%d, want 2/1", empty, terminate)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationBudget {
		t.Fatalf("termination causes = %v", causes)
	}
}

func TestLifetimeGuardianPendingCooperativeStartUsesBoundOwnerProof(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	f.scope.emptyResults = []bool{false, true}
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err != nil {
		t.Fatalf("begin: %v", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.call(t, f.message(lifetimeMessageClose, 0, 0)) }()
	select {
	case err := <-closeDone:
		if err == nil {
			t.Fatal("pending cooperative start was acknowledged")
		}
	case <-time.After(time.Second):
		t.Fatal("pending start did not close the worker control channel")
	}
	f.worker.release()
	if err := f.waitRun(t); errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("bound owner was rejected as unverifiable: %v", err)
	}
	if causes := f.records.terminationCauses(); len(causes) != 1 || causes[0] != task.LifetimeTerminationCompleted {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationCompleted)
	}
	_, terminate, _ := f.scope.counts()
	if terminate != 1 {
		t.Fatalf("cooperative scope terminate calls = %d, want 1 through bound owner proof", terminate)
	}
}

func TestLifetimeGuardianPendingStartFailsClosedWithoutProof(t *testing.T) {
	wantErr := lifetime.ErrUnverifiableLaunch
	retryEntered := make(chan struct{})
	retryRelease := make(chan struct{})
	var retryOnce sync.Once
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		nil,
		func(options *Options) {
			options.FinalizationBackoff = func(ctx context.Context, _ time.Duration) error {
				retryOnce.Do(func() { close(retryEntered) })
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-retryRelease:
					return nil
				}
			}
		},
	)
	f.scope.pendingStartEmptyErr = wantErr
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.call(t, f.message(lifetimeMessageClose, 0, 0)) }()
	select {
	case err := <-closeDone:
		if err == nil {
			t.Fatal("pending start was acknowledged without an emptiness proof")
		}
	case <-time.After(time.Second):
		t.Fatal("pending start did not close the worker control channel")
	}
	f.worker.release()
	select {
	case err := <-f.runDone:
		t.Fatalf("guardian escaped before pending-start proof became available: %v", err)
	case <-retryEntered:
	}
	if causes := f.records.terminationCauses(); len(causes) != 0 {
		t.Fatalf("termination recorded without pending-start proof: %v", causes)
	}
	f.scope.mu.Lock()
	pendingCalls := f.scope.pendingStartEmptyCalls
	f.scope.mu.Unlock()
	if pendingCalls == 0 {
		t.Fatal("pending-start proof was not attempted")
	}
	f.scope.mu.Lock()
	f.scope.pendingStartEmptyErr = nil
	f.scope.mu.Unlock()
	close(retryRelease)
	if err := f.waitRun(t); !errors.Is(err, wantErr) {
		t.Fatalf("run error = %v, want ErrUnverifiableLaunch", err)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("finalized pending start disposal = destroy %d release %d, want 1/1", destroy, release)
	}
}

func TestLifetimeGuardianPendingStartTerminatesDiscoveredScopeBeforeRecord(t *testing.T) {
	pendingEmpty := false
	f := newLifetimeGuardianTestFixtureWithAssurance(t, nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative)
	f.scope.pendingStartEmptyResult = &pendingEmpty
	f.scope.emptyResults = []bool{false, true}
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.call(t, f.message(lifetimeMessageClose, 0, 0)) }()
	select {
	case err := <-closeDone:
		if err == nil {
			t.Fatal("pending start was acknowledged before scope termination")
		}
	case <-time.After(time.Second):
		t.Fatal("pending start did not close the worker control channel")
	}
	f.worker.release()
	if err := f.waitRun(t); err == nil {
		t.Fatal("closed control channel must remain visible to the caller")
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationCompleted {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationCompleted)
	}
	_, terminate, _ := f.scope.counts()
	if terminate != 1 {
		t.Fatalf("pending-start scope terminate calls = %d, want 1", terminate)
	}
}

func TestLifetimeGuardianPendingExactStartRecordsOnlyAfterWorkerExitAndEmptyProof(t *testing.T) {
	f := newLifetimeGuardianTestFixtureWithAssurance(t, nil, lifetime.KindCgroupV2, lifetime.AssuranceExact)
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err != nil {
		t.Fatalf("begin: %v", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.call(t, f.message(lifetimeMessageClose, 0, 0)) }()
	select {
	case err := <-closeDone:
		if err == nil {
			t.Fatal("pending exact start was acknowledged before proof")
		}
	case <-time.After(time.Second):
		t.Fatal("pending start did not close the worker control channel")
	}
	if causes := f.records.terminationCauses(); len(causes) != 0 {
		t.Fatalf("termination recorded before worker exit: %v", causes)
	}
	f.worker.release()
	if err := f.waitRun(t); err == nil {
		t.Fatal("closed control channel must remain visible to the caller")
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationCompleted {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationCompleted)
	}
	events := f.timeline.snapshot()
	if timelineIndex(events, "worker-exited") >= timelineIndex(events, "termination-record") {
		t.Fatalf("termination order = %v", events)
	}
}

func TestLifetimeGuardianPendingStartFinalizesAfterNonzeroWorkerExit(t *testing.T) {
	waitErr := errors.New("worker exited after pending start was fenced")
	f := newLifetimeGuardianTestFixtureWithAssurance(t, nil, lifetime.KindCgroupV2, lifetime.AssuranceExact)
	f.worker.setWaitError(waitErr)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.call(t, f.message(lifetimeMessageClose, 0, 0)) }()
	select {
	case err := <-closeDone:
		if err == nil {
			t.Fatal("pending start was acknowledged before the worker exited")
		}
	case <-time.After(time.Second):
		t.Fatal("pending start did not close the worker control channel")
	}
	f.worker.release()
	if err := f.waitRun(t); !errors.Is(err, waitErr) {
		t.Fatalf("run error = %v, want exact worker error", err)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationCompleted {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationCompleted)
	}
	events := f.timeline.snapshot()
	assertTimelineOrder(t, events, "worker-exited", "termination-record")
}

func TestLifetimeGuardianWorkerLossRecoversWithoutOutcome(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	f.worker.release()
	err := f.waitRun(t)
	if !errors.Is(err, errLifetimeGuardianWorkerLost) {
		t.Fatalf("run error = %v, want worker loss", err)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationRecovery {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationRecovery)
	}
}

func TestLifetimeGuardianControlLossRecovers(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	if err := f.waitForWorker(t); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Close(); err != nil {
		t.Fatalf("close control: %v", err)
	}
	f.worker.release()
	err := f.waitRun(t)
	if !errors.Is(err, errLifetimeGuardianWorkerLost) {
		t.Fatalf("run error = %v, want control loss recovery", err)
	}
	causes := f.records.terminationCauses()
	if len(causes) != 1 || causes[0] != task.LifetimeTerminationRecovery {
		t.Fatalf("termination causes = %v, want [%s]", causes, task.LifetimeTerminationRecovery)
	}
}

func (f *lifetimeGuardianTestFixture) waitForWorker(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case <-f.worker.waitEntered:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestLifetimeGuardianDoesNotAckBeforeEmptyProof(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	emptyEntered := make(chan struct{})
	emptyRelease := make(chan bool, 1)
	f.scope.emptyResults = []bool{false}
	f.scope.blockEmptyCall = 1
	f.scope.emptyEntered = emptyEntered
	f.scope.emptyRelease = emptyRelease
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageStarted, 1, 303)); err != nil {
		t.Fatalf("started: %v", err)
	}
	finishDone := make(chan error, 1)
	go func() { finishDone <- f.call(t, f.message(lifetimeMessageFinish, 1, 0)) }()
	select {
	case <-emptyEntered:
	case <-time.After(time.Second):
		t.Fatal("finish did not reach empty observation")
	}
	select {
	case err := <-finishDone:
		t.Fatalf("finish acknowledged before empty proof: %v", err)
	default:
	}
	emptyRelease <- false
	if err := <-finishDone; err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageClose, 0, 0)); err != nil {
		t.Fatalf("close: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestLifetimeGuardianFinishUsesImmutableWorkDeadline(t *testing.T) {
	emptyEntered := make(chan struct{})
	emptyRelease := make(chan bool, 1)
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		func(scope *lifetimeGuardianTestScope) {
			scope.blockEmptyCall = 1
			scope.emptyEntered = emptyEntered
			scope.emptyRelease = emptyRelease
		},
		func(options *Options) {
			options.CleanupTimeout = 20 * time.Millisecond
			options.Deadline = time.Now().Add(time.Second)
			options.Now = time.Now
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "begin", f.message(lifetimeMessageBegin, 1, 0))
	f.callMust(t, "started", f.message(lifetimeMessageStarted, 1, 303))
	finishDone := make(chan error, 1)
	go func() { finishDone <- f.call(t, f.message(lifetimeMessageFinish, 1, 0)) }()
	select {
	case <-emptyEntered:
	case <-time.After(time.Second):
		t.Fatal("finish did not reach empty observation")
	}
	select {
	case err := <-finishDone:
		t.Fatalf("finish used the shorter cleanup fallback: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	emptyRelease <- true
	if err := <-finishDone; err != nil {
		t.Fatalf("finish after empty proof: %v", err)
	}
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestLifetimeGuardianCloseUsesImmutableWorkDeadline(t *testing.T) {
	emptyEntered := make(chan struct{})
	emptyRelease := make(chan bool, 1)
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		func(scope *lifetimeGuardianTestScope) {
			scope.blockEmptyCall = 1
			scope.emptyEntered = emptyEntered
			scope.emptyRelease = emptyRelease
		},
		func(options *Options) {
			options.CleanupTimeout = 100 * time.Millisecond
			options.Deadline = time.Now().Add(2 * time.Second)
			options.Now = time.Now
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.call(t, f.message(lifetimeMessageClose, 0, 0)) }()
	select {
	case <-emptyEntered:
	case <-time.After(time.Second):
		t.Fatal("close did not reach empty observation")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("close used the shorter cleanup fallback: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	emptyRelease <- true
	if err := <-closeDone; err != nil {
		t.Fatalf("close after empty proof: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestLifetimeGuardianRejectsInvalidOrdering(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	if err := f.call(t, f.message(lifetimeMessageBegin, 1, 0)); err == nil {
		t.Fatal("begin before hello was accepted")
	}
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageFinish, 1, 0)); err == nil {
		t.Fatal("finish without active launch was accepted")
	}
	if err := f.call(t, f.message(lifetimeMessageClose, 0, 0)); err != nil {
		t.Fatalf("close: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestLifetimeGuardianRegistrationUsesBootID(t *testing.T) {
	f := newLifetimeGuardianTestFixture(t)
	if err := f.call(t, f.message(lifetimeMessageHello, 0, 0)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := f.call(t, f.message(lifetimeMessageClose, 0, 0)); err != nil {
		t.Fatalf("close: %v", err)
	}
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	facts := f.records.registrationFacts()
	if facts.BootID != "test-boot" {
		t.Fatalf("registration boot ID = %q, want test-boot", facts.BootID)
	}
}

type lifetimeGuardianDeadlineConn struct {
	net.Conn
	deadline chan time.Time
	once     sync.Once
}

func (c *lifetimeGuardianDeadlineConn) recordDeadline(deadline time.Time) {
	c.once.Do(func() { c.deadline <- deadline })
}

func (c *lifetimeGuardianDeadlineConn) SetDeadline(deadline time.Time) error {
	c.recordDeadline(deadline)
	return c.Conn.SetDeadline(deadline)
}

func (c *lifetimeGuardianDeadlineConn) SetReadDeadline(deadline time.Time) error {
	c.recordDeadline(deadline)
	return c.Conn.SetReadDeadline(deadline)
}

func TestLifetimeGuardianReaderUsesTaskDeadlineAndCancellation(t *testing.T) {
	serverConn, peerConn := net.Pipe()
	defer func() {
		if err := peerConn.Close(); err != nil {
			t.Errorf("close peer connection: %v", err)
		}
	}()
	conn := &lifetimeGuardianDeadlineConn{Conn: serverConn, deadline: make(chan time.Time, 1)}
	codec, err := newLifetimeCodec(conn)
	if err != nil {
		t.Fatal(err)
	}
	taskDeadline := time.Now().Add(2 * time.Minute)
	receiveCtx, cancel := context.WithDeadline(context.Background(), taskDeadline)
	defer cancel()
	reader := startLifetimeGuardianReader(codec, receiveCtx)
	select {
	case got := <-conn.deadline:
		if got.Before(taskDeadline.Add(-time.Second)) || got.After(taskDeadline.Add(time.Second)) {
			t.Fatalf("receive deadline = %v, want task deadline %v", got, taskDeadline)
		}
	case <-time.After(time.Second):
		t.Fatal("reader did not establish the task deadline")
	}
	cancel()
	select {
	case <-reader.done:
	case <-time.After(time.Second):
		t.Fatal("reader did not stop when its context was canceled")
	}
}

func (r *lifetimeGuardianTestRecords) ReadLifetimeWorkerWaitContext(ctx context.Context) (*task.LifetimeWorkerWaitRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.existingCompletion == nil {
		return nil, os.ErrNotExist
	}
	record := *r.existingCompletion
	return &record, nil
}

func (r *lifetimeGuardianTestRecords) ReadLifetimeUnlaunchedContext(ctx context.Context) (*task.LifetimeUnlaunchedRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, os.ErrNotExist
}

func (r *lifetimeGuardianTestRecords) TaskLaunchRefusedContext(ctx context.Context) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	return false, ctx.Err()
}
