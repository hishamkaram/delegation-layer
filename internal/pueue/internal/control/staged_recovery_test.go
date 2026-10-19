package control

import (
	"context"
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

	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

func TestClientCloseAfterPendingTransfersEnqueueOwnership(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	appendStagedEnvironment(fake, "FAKE_ENQUEUE_DELAY=0.5")
	fake.client.options.ObservationTimeout = 20 * time.Millisecond
	_, permit, _, launch := prepareStagedOrdinarySubmission(t, fake)
	observation, err := fake.client.SubmitStaged(context.Background(), permit, launch, func(Observation) error { return nil })
	if !errors.Is(err, ErrEnqueueUncertain) || observation.Pending == nil {
		t.Fatalf("enqueue did not retain pending ownership: observation=%+v err=%v", observation, err)
	}

	cleanupCalled := make(chan struct{})
	fake.client.cleanup = func() error {
		close(cleanupCalled)
		return nil
	}
	if closeErr := fake.client.CloseAfterPending(err); closeErr != nil {
		t.Fatal(closeErr)
	}
	select {
	case <-observation.Pending.Done():
		t.Fatal("closing the client completed its pending enqueue")
	default:
	}
	if _, snapshotErr := fake.client.Snapshot(t.Context()); !errors.Is(snapshotErr, ErrConfiguration) {
		t.Fatalf("closing client accepted new work: %v", snapshotErr)
	}
	if !awaitPendingNaturally(observation.Pending, 2*time.Second) {
		t.Fatal("transferred enqueue ownership did not reach natural completion")
	}
	select {
	case <-cleanupCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred client cleanup did not follow pending completion")
	}
}

func TestSubmitStagedPreStartEnqueueFailureRecoversExactRow(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	_, permit, identity, launch := prepareStagedOrdinarySubmission(t, fake)
	observer := installFailFirstEnqueueObserver(t, fake)
	first, submitErr := fake.client.SubmitStaged(context.Background(), permit, launch, func(Observation) error { return nil })
	if observer.observerErr != nil || !errors.Is(submitErr, ErrEnqueueUncertain) || first.Pending != nil || !first.Matched || first.State != StateStashed {
		t.Fatalf("pre-start enqueue failure was not retained as retryable Stashed state: observation=%+v err=%v observer=%v", first, submitErr, observer.observerErr)
	}
	writeStagedOrdinaryStatus(t, fake, identity, 7, StateStashed, "", "default")
	second, recoveryErr := fake.client.RecoverStaged(context.Background(), identity, 7, func(candidate Observation) error {
		if candidate.State != StateStashed || candidate.NumericTaskID != 7 {
			t.Fatalf("recovery callback received the wrong row: %+v", candidate)
		}
		return nil
	})
	if recoveryErr != nil || !second.Matched || second.State != StateStashed || observer.enqueueEntries != 2 {
		t.Fatalf("pre-start enqueue failure was not recovered exactly once: observation=%+v err=%v enqueue_entries=%d", second, recoveryErr, observer.enqueueEntries)
	}
	if got := len(stagedInvocations(t, fake, "add")); got != 1 || len(stagedInvocations(t, fake, "enqueue")) != 1 {
		t.Fatalf("recovery duplicated add or failed to retry enqueue: adds=%d enqueues=%d", got, len(stagedInvocations(t, fake, "enqueue")))
	}
}

func TestRecoverStagedRejectsAlreadyQueuedDuringRace(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	_, _, identity, _ := prepareStagedOrdinarySubmission(t, fake)
	writeStagedOrdinaryStatus(t, fake, identity, 7, StateStashed, "", "default")
	transition := installEnqueueStatusTransition(fake, ordinaryQueuedStatus(t, identity, 7))
	scope := newTestCallScope(t, fake.client.options)
	firstCall := startOrdinaryRecovery(scope, fake.client, identity, 7)
	awaitEntry(scope, transition.entered, firstCall, "first recovery did not reach enqueue")
	secondCallback := false
	second, secondErr := fake.client.RecoverStaged(context.Background(), identity, 7, func(Observation) error {
		secondCallback = true
		return nil
	})
	first := awaitCall(scope, firstCall, "first recovery")
	if transition.writeErr != nil || first.err != nil || !first.observation.Matched || first.observation.State != StateStashed || !errors.Is(secondErr, ErrUnknown) {
		t.Fatalf("concurrent recovery did not complete cleanly: first=%+v second=%+v secondErr=%v observer=%v", first.observation, second, secondErr, transition.writeErr)
	}
	if secondCallback || !second.Matched || second.State != StateQueued || second.NumericTaskID != 7 {
		t.Fatalf("already-queued recovery invoked commit or lost exact queued row: %+v callback=%t", second, secondCallback)
	}
	if got := transition.entries.Load(); got != 1 {
		t.Fatalf("already-enqueued recovery duplicated enqueue: %d", got)
	}
}

func TestRecoverInspectionStagedRejectsAlreadyQueuedDuringRace(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	identity := InspectionIdentity{RootID: fixture.store.RootID, TaskID: fixture.taskDir.TaskID}
	writeStagedInspectionStatus(t, fake, identity, 9, StateStashed, "", identity.Group())
	transition := installEnqueueStatusTransition(fake, inspectionQueuedStatus(t, identity, 9))
	scope := newTestCallScope(t, fake.client.options)
	firstCall := startInspectionRecovery(scope, fake.client, identity, 9)
	awaitEntry(scope, transition.entered, firstCall, "first inspection recovery did not reach enqueue")
	secondCallback := false
	second, secondErr := fake.client.RecoverInspectionStaged(context.Background(), identity, 9, func(InspectionObservation) error {
		secondCallback = true
		return nil
	})
	first := awaitCall(scope, firstCall, "first inspection recovery")
	if transition.writeErr != nil || first.err != nil || !first.observation.Matched || first.observation.Job.State != StateStashed || !errors.Is(secondErr, ErrUnknown) {
		t.Fatalf("concurrent inspection recovery did not complete cleanly: first=%+v second=%+v secondErr=%v observer=%v", first.observation, second, secondErr, transition.writeErr)
	}
	if secondCallback || !second.Matched || second.Job.State != StateQueued || second.Job.ID != 9 {
		t.Fatalf("already-queued inspection recovery invoked commit or lost exact queued row: %+v callback=%t", second, secondCallback)
	}
	if got := transition.entries.Load(); got != 1 {
		t.Fatalf("already-enqueued inspection recovery duplicated enqueue: %d", got)
	}
}

func TestRecoverStagedRequiresExactStashedRow(t *testing.T) {
	cases := []ordinaryStagedRecoveryCase{
		{name: "exact stashed", state: StateStashed, rowID: 7, expectation: expectStashedRecovery},
		{name: "already queued", state: StateQueued, rowID: 7, expectation: expectQueuedRejection},
		{name: "scheduled stashed", state: StateStashed, rowID: 7, scheduled: true, expectation: expectRejectedRecovery},
		{name: "running", state: StateRunning, rowID: 7, expectation: expectRejectedRecovery},
		{name: "ended", state: StateEnded, rowID: 7, expectation: expectRejectedRecovery},
		{name: "unknown", state: StateUnknown, rowID: 7, expectation: expectRejectedRecovery},
		{name: "wrong id", state: StateStashed, rowID: 8, expectation: expectRejectedRecovery},
		{name: "wrong label", state: StateStashed, rowID: 7, label: "delegate:wrong:label", expectation: expectRejectedRecovery},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runOrdinaryStagedRecoveryCase(t, testCase)
		})
	}
}

func TestSubmitInspectionStagedPassesExactReceiptAndRegistrationState(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	taskID := fixture.taskDir.TaskID
	identity := InspectionIdentity{RootID: fixture.store.RootID, TaskID: taskID}
	launch := Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root, Supervised: true}
	writeStagedInspectionStatusWithLaunch(t, fake, identity, 9, StateStashed, launch)
	permit := &inspectionTestPermit{
		submissionRoot:       identity.RootID,
		submissionTask:       identity.TaskID,
		submissionSupervisor: fake.client.Binding(),
	}
	var committed InspectionObservation
	observation, err := fake.client.SubmitInspectionStaged(context.Background(), permit, identity, launch, captureInspectionStagedCommit(t, fake, identity, &committed))
	if err != nil || observation != committed || !observation.Matched || observation.Job.State != StateStashed {
		t.Fatalf("staged inspection result was not retained: observation=%+v committed=%+v err=%v", observation, committed, err)
	}
	if permit.calls != 1 || len(stagedInvocations(t, fake, "add")) != 1 || len(stagedInvocations(t, fake, "enqueue")) != 1 {
		t.Fatalf("inspection staged protocol issued the wrong commands: calls=%d invocations=%+v", permit.calls, readInvocations(t, fake.logPath))
	}
}

func TestSubmitInspectionStagedHoldsQueueIdentityThroughCommitAndEnqueue(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	launch := Launch{RunnerExecutable: fake.executable, RootPath: store.Root, Supervised: true}
	permit := &concurrentInspectionTestPermit{
		inspectionTestPermit: &inspectionTestPermit{
			submissionRoot:       identity.RootID,
			submissionTask:       identity.TaskID,
			submissionSupervisor: fake.client.Binding(),
		},
		queueGroup: "default",
	}
	transition := newStagedTransition(t, fake.client, store.Root, stagedSharedIdentity)
	call := transition.start(func(ctx context.Context) stagedTransitionResult {
		observation, err := fake.client.SubmitConcurrentInspectionStaged(ctx, permit, identity, launch, func(candidate InspectionObservation) error {
			if !candidate.Matched || candidate.Job.State != StateStashed || candidate.Job.ID != 9 {
				return fmt.Errorf("submission commit received the wrong staged row: %+v", candidate)
			}
			return transition.commit()
		})
		return inspectionTransitionResult(observation, err)
	})
	assertStagedTransitionHoldsQueueIdentity(t, transition, fake.client, call)
	if got := len(stagedInvocations(t, fake, "add")); got != 1 || len(stagedInvocations(t, fake, "enqueue")) != 1 {
		t.Fatalf("staged submission changed command cardinality: adds=%d enqueues=%d", got, len(stagedInvocations(t, fake, "enqueue")))
	}
}

func TestSubmitInspectionStagedEnqueueFailureRetainsExactReceipt(t *testing.T) {
	fake := newInspectionSupervisor(t, "enqueue-refuse")
	fixture := newTaskFixture(t, fake.client.Binding())
	identity := InspectionIdentity{RootID: fixture.store.RootID, TaskID: fixture.taskDir.TaskID}
	launch := Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root, Supervised: true}
	writeStagedInspectionStatusWithLaunch(t, fake, identity, 9, StateStashed, launch)
	permit := &inspectionTestPermit{
		submissionRoot:       identity.RootID,
		submissionTask:       identity.TaskID,
		submissionSupervisor: fake.client.Binding(),
	}
	commits := 0
	observation, submitErr := fake.client.SubmitInspectionStaged(context.Background(), permit, identity, launch, func(candidate InspectionObservation) error {
		commits++
		if candidate.Job.State != StateStashed || candidate.Job.ID != 9 {
			t.Fatalf("enqueue failure callback received the wrong receipt: %+v", candidate)
		}
		return nil
	})
	if !errors.Is(submitErr, ErrEnqueueUncertain) || commits != 1 || !observation.Matched || observation.Job.State != StateStashed || observation.Job.ID != 9 {
		t.Fatalf("enqueue failure lost the committed receipt: observation=%+v err=%v commits=%d", observation, submitErr, commits)
	}
	if got := len(stagedInvocations(t, fake, "add")); got != 1 || len(stagedInvocations(t, fake, "enqueue")) != 1 {
		t.Fatalf("enqueue failure duplicated staged commands: adds=%d enqueues=%d", got, len(stagedInvocations(t, fake, "enqueue")))
	}
}

func TestSubmitConcurrentInspectionStagedRegistersBeforeCommit(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	taskID, err := randomid.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	identity := InspectionIdentity{RootID: store.RootID, TaskID: taskID, QueueGroup: "default"}
	launch := Launch{RunnerExecutable: fake.executable, RootPath: store.Root, Supervised: true}
	writeStagedInspectionStatusWithLaunch(t, fake, identity, 9, StateStashed, launch)
	permit := &concurrentInspectionTestPermit{
		inspectionTestPermit: &inspectionTestPermit{
			submissionRoot:       identity.RootID,
			submissionTask:       identity.TaskID,
			submissionSupervisor: fake.client.Binding(),
		},
		queueGroup: "default",
	}
	callbackRegistration := [2]bool{}
	observation, err := fake.client.SubmitConcurrentInspectionStaged(context.Background(), permit, identity, launch, func(candidate InspectionObservation) error {
		callbackRegistration = [2]bool{candidate.TerminalEventRegistrationKnown, candidate.TerminalEventRegistered}
		if candidate.Job.State != StateStashed || candidate.Job.ID != 9 || candidate.Job.Group != "default" {
			t.Fatalf("concurrent inspection callback received the wrong receipt: %+v", candidate)
		}
		return nil
	})
	if err != nil || !observation.Matched || observation.Job.State != StateStashed {
		t.Fatalf("concurrent staged inspection failed: observation=%+v err=%v", observation, err)
	}
	if callbackRegistration != [2]bool{true, true} || !observation.TerminalEventRegistrationKnown || !observation.TerminalEventRegistered {
		t.Fatalf("terminal event registration state was not preserved: callback=%v result=%+v", callbackRegistration, observation)
	}
	if got := len(stagedInvocations(t, fake, "add")); got != 1 || len(stagedInvocations(t, fake, "enqueue")) != 1 {
		t.Fatalf("concurrent staged inspection duplicated scheduler commands: adds=%d enqueues=%d", got, len(stagedInvocations(t, fake, "enqueue")))
	}
}

func TestRecoverInspectionStagedRegistersBeforeCommit(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	writeStagedInspectionStatus(t, fake, identity, 9, StateStashed, "", "default")

	var committed InspectionObservation
	observation, err := fake.client.RecoverInspectionStaged(context.Background(), identity, 9, func(candidate InspectionObservation) error {
		committed = candidate
		if !candidate.Matched || candidate.Job.State != StateStashed || candidate.Job.ID != 9 {
			t.Fatalf("recovery callback received the wrong staged row: %+v", candidate)
		}
		if !candidate.TerminalEventRegistrationKnown || !candidate.TerminalEventRegistered {
			t.Fatalf("recovery callback lost terminal-event registration state: %+v", candidate)
		}
		ticketPath := filepath.Join(store.Root, privateSupervisorDirectory, privateTerminalEventTicketDir, "9.json")
		if _, statErr := os.Stat(ticketPath); statErr != nil {
			t.Fatalf("terminal-event ticket was not present before recovery callback: %v", statErr)
		}
		if got := len(stagedInvocations(t, fake, "enqueue")); got != 0 {
			t.Fatalf("recovery enqueue ran before commit: %d invocations", got)
		}
		return nil
	})
	if err != nil || observation != committed || !observation.TerminalEventRegistrationKnown || !observation.TerminalEventRegistered {
		t.Fatalf("staged inspection recovery lost registration state: observation=%+v committed=%+v err=%v", observation, committed, err)
	}
	if got := len(stagedInvocations(t, fake, "enqueue")); got != 1 {
		t.Fatalf("staged inspection recovery did not enqueue exactly once: %d", got)
	}
}

func TestRecoverStagedHoldsQueueIdentityThroughCommitAndEnqueue(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	_, _, identity, _ := prepareStagedOrdinarySubmission(t, fake)
	writeStagedOrdinaryStatus(t, fake, identity, 7, StateStashed, "", "default")
	transition := newStagedTransition(t, fake.client, store.Root, stagedExclusiveMutation)
	call := transition.start(func(ctx context.Context) stagedTransitionResult {
		observation, err := fake.client.RecoverStaged(ctx, identity, 7, func(candidate Observation) error {
			if !candidate.Matched || candidate.State != StateStashed || candidate.NumericTaskID != 7 {
				return fmt.Errorf("recovery commit received the wrong staged row: %+v", candidate)
			}
			return transition.commit()
		})
		return stagedTransitionResult{err: err, pending: observation.Pending, observation: fmt.Sprintf("%+v", observation)}
	})
	assertStagedTransitionHoldsQueueIdentity(t, transition, fake.client, call)
}

func TestRecoverInspectionStagedHoldsQueueIdentityThroughRegistrationCommitAndEnqueue(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	writeStagedInspectionStatus(t, fake, identity, 9, StateStashed, "", "default")
	transition := newStagedTransition(t, fake.client, store.Root, stagedExclusiveMutation)
	call := transition.start(recoverInspectionTransition(fake.client, identity, 9, transition))
	assertStagedTransitionHoldsQueueIdentity(t, transition, fake.client, call)
}

// Canceling the run while the commit callback is held must stop the staged
// transition before its effective enqueue, even though the callback itself
// then returns nil; the released queue identity admits a later writer.
func TestRecoverInspectionStagedCanceledAtCommitDoesNotEnqueue(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	writeStagedInspectionStatus(t, fake, identity, 9, StateStashed, "", "default")
	transition := newStagedTransition(t, fake.client, store.Root, stagedExclusiveMutation)
	call := transition.start(recoverInspectionTransition(fake.client, identity, 9, transition))
	awaitEntry(transition.scope, transition.entered, call, "staged recovery did not reach its commit callback")
	transition.cancelRun()
	transition.releaseCommit()
	result := awaitCall(transition.scope, call, "canceled staged recovery")
	if !errors.Is(result.err, context.Canceled) || result.pending != nil {
		t.Fatalf("canceled staged recovery did not stop before enqueue: err=%v pending=%t observation=%s", result.err, result.pending != nil, result.observation)
	}
	awaitLockCall(transition.scope, transition.startWriter(fake.client), "later queue identity writer")
	facts := transition.snapshot()
	assertStagedTransitionFacts(t, transition.mode, facts, 0)
	if !facts.writerEntered {
		t.Fatal("later queue identity writer did not enter after the canceled recovery")
	}
	if got := len(stagedInvocations(t, fake, "enqueue")); got != 0 {
		t.Fatalf("canceled staged recovery issued %d enqueue commands", got)
	}
}

// The staged owner reports a transition that refuses before its commit with
// its exact result, here the already-queued exact-row refusal.
func TestStagedTransitionReportsRefusalBeforeCommit(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	writeStagedInspectionStatus(t, fake, identity, 9, StateQueued, "", "default")
	transition := newStagedTransition(t, fake.client, store.Root, stagedExclusiveMutation)
	var refused InspectionObservation
	call := transition.start(func(ctx context.Context) stagedTransitionResult {
		observation, err := fake.client.RecoverInspectionStaged(ctx, identity, 9, func(InspectionObservation) error {
			return transition.commit()
		})
		refused = observation
		return inspectionTransitionResult(observation, err)
	})
	if err := waitForEntry(transition.scope, transition.entered, call); !errors.Is(err, errCallCompletedBeforeEntry) {
		t.Fatalf("staged owner did not report the refusal before commit: %v", err)
	}
	facts := transition.snapshot()
	assertRejectedInspectionQueuedRecovery(t, refused, call.result.err, facts.commits, facts.enqueueEntries)
}

func TestPrivateQueueWriterIntentRepairsAbandonedState(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	base := filepath.Join(store.Root, privateSupervisorDirectory)
	if _, _, err := fake.client.preparePrivateQueueIdentityLocks(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, privateQueueWriterIntentLockName)
	if err := os.WriteFile(path, []byte(privateQueueWriterWaitingState), 0o600); err != nil {
		t.Fatal(err)
	}
	entered := false
	if err := fake.client.withPrivateQueueIdentityLease(t.Context(), func(context.Context) error {
		entered = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != privateQueueWriterIdleState || !entered {
		t.Fatalf("abandoned writer intent was not repaired: entered=%t state=%q err=%v", entered, data, err)
	}
}

// waitForPrivateQueueWriterIntent polls the durable intent a writer publishes
// before it waits for the gate. Callers use it only while the writer cannot
// complete, so a writer that returns first is a failure.
func waitForPrivateQueueWriterIntent(t *testing.T, stateRoot string, writerDone <-chan struct{}, cutoff time.Time) {
	t.Helper()
	path := filepath.Join(stateRoot, privateSupervisorDirectory, privateQueueWriterIntentLockName)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == privateQueueWriterWaitingState {
			return
		}
		if !time.Now().Before(cutoff) {
			t.Fatalf("writer intent was not published before the operation cutoff: state=%q err=%v", data, err)
		}
		select {
		case <-writerDone:
			t.Fatalf("writer returned before publishing its intent: state=%q err=%v", data, err)
		case <-ticker.C:
		}
	}
}

func TestSubmitConcurrentInspectionStagedSerializesRecoveryAfterCommit(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	launch := Launch{RunnerExecutable: fake.executable, RootPath: store.Root, Supervised: true}
	writeStagedInspectionStatusWithLaunch(t, fake, identity, 9, StateStashed, launch)
	transition := installEnqueueStatusTransition(fake, inspectionQueuedStatusWithLaunch(t, identity, 9, launch))
	permit := &concurrentInspectionTestPermit{
		inspectionTestPermit: &inspectionTestPermit{
			submissionRoot:       identity.RootID,
			submissionTask:       identity.TaskID,
			submissionSupervisor: fake.client.Binding(),
		},
		queueGroup: "default",
	}
	staged := newStagedTransition(t, fake.client, store.Root, stagedSharedIdentity)
	var original inspectionRecoveryResult
	var originalCommitted InspectionObservation
	originalCall := staged.start(func(ctx context.Context) stagedTransitionResult {
		observation, err := fake.client.SubmitConcurrentInspectionStaged(ctx, permit, identity, launch, func(candidate InspectionObservation) error {
			originalCommitted = candidate
			return staged.commit()
		})
		original = inspectionRecoveryResult{observation: observation, err: err}
		return inspectionTransitionResult(observation, err)
	})
	awaitEntry(staged.scope, staged.entered, originalCall, "original submission did not reach its durable commit barrier")
	var recoveryCommitted atomic.Bool
	recoveryCall := startInspectionRecoveryWithCommitFlag(staged.scope, fake.client, identity, 9, &recoveryCommitted)
	waitForPrivateQueueWriterIntent(t, store.Root, recoveryCall.done, staged.scope.cutoff)
	select {
	case <-recoveryCall.done:
		t.Fatalf("recovery crossed the original submission's queue identity lease: %+v", recoveryCall.result)
	default:
	}
	if got := transition.entries.Load(); got != 0 {
		t.Fatalf("enqueue crossed the durable commit barrier: %d", got)
	}
	staged.releaseCommit()
	awaitCall(staged.scope, originalCall, "original submission")
	recovery := awaitCall(staged.scope, recoveryCall, "serialized recovery")
	assertStagedTransitionFacts(t, staged.mode, staged.snapshot(), 1)
	assertSerializedInspectionSubmission(t, fake, transition, recovery, recoveryCommitted.Load(), original, originalCommitted)
}

func startInspectionRecoveryWithCommitFlag(scope *testCallScope, client *Client, identity InspectionIdentity, id int64, committed *atomic.Bool) *testCall[inspectionRecoveryResult] {
	ctx := scope.context()
	return startTestCall(scope, func() inspectionRecoveryResult {
		observation, err := client.RecoverInspectionStaged(ctx, identity, id, func(InspectionObservation) error {
			committed.Store(true)
			return nil
		})
		return inspectionRecoveryResult{observation: observation, err: err}
	})
}

func assertSerializedInspectionSubmission(t *testing.T, fake *fakeSupervisor, transition *enqueueStatusTransition, recovery inspectionRecoveryResult, recoveryCommitted bool, original inspectionRecoveryResult, originalCommitted InspectionObservation) {
	t.Helper()
	assertSerializedInspectionRecovery(t, transition, recovery, recoveryCommitted, original, originalCommitted)
	assertSingleInspectionEnqueue(t, fake, transition)
}

func assertSerializedInspectionRecovery(t *testing.T, transition *enqueueStatusTransition, recovery inspectionRecoveryResult, recoveryCommitted bool, original inspectionRecoveryResult, originalCommitted InspectionObservation) {
	t.Helper()
	if transition.writeErr != nil || !errors.Is(recovery.err, ErrUnknown) || !recovery.observation.Matched || recovery.observation.Job.State != StateQueued || recovery.observation.Job.ID != 9 {
		t.Fatalf("serialized recovery did not observe the exact queued row: observation=%+v err=%v observer=%v", recovery.observation, recovery.err, transition.writeErr)
	}
	if recoveryCommitted {
		t.Fatal("serialized recovery repeated the durable commit")
	}
	if original.err != nil || original.observation != originalCommitted || !original.observation.Matched || original.observation.Job.State != StateStashed || original.observation.Job.ID != 9 {
		t.Fatalf("original submission did not return its exact committed receipt: observation=%+v committed=%+v err=%v", original.observation, originalCommitted, original.err)
	}
}

func assertSingleInspectionEnqueue(t *testing.T, fake *fakeSupervisor, transition *enqueueStatusTransition) {
	t.Helper()
	if got := transition.entries.Load(); got != 1 {
		t.Fatalf("serialized submission issued %d effective enqueue commands, want one", got)
	}
	adds, enqueues := len(stagedInvocations(t, fake, "add")), stagedInvocations(t, fake, "enqueue")
	if adds != 1 || len(enqueues) != 1 {
		t.Fatalf("serialized recovery changed staged command cardinality: adds=%d enqueues=%d", adds, len(enqueues))
	}
	if !slices.Equal(enqueues[0], []string{"-c", fake.configPath, "enqueue", "9"}) {
		t.Fatalf("serialized recovery mutated a stale numeric ID: enqueue=%v", enqueues)
	}
}

func TestStagedInspectionRegistrationCapabilityErrorUsesPollingFallback(t *testing.T) {
	fake, _ := newPrivateInspectionSupervisor(t, "ok")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	known, registered := fake.client.registerStagedInspectionTerminalEvent(ctx, 9)
	if !known || registered {
		t.Fatalf("capability error changed the existing registration contract: known=%t registered=%t", known, registered)
	}
}

func TestRecoverInspectionStagedRequiresExactStashedRow(t *testing.T) {
	cases := []inspectionStagedRecoveryCase{
		{name: "exact stashed", state: StateStashed, rowID: 9, expectation: expectStashedRecovery},
		{name: "already queued", state: StateQueued, rowID: 9, expectation: expectQueuedRejection},
		{name: "scheduled stashed", state: StateStashed, rowID: 9, scheduled: true, expectation: expectRejectedRecovery},
		{name: "running", state: StateRunning, rowID: 9, expectation: expectRejectedRecovery},
		{name: "ended", state: StateEnded, rowID: 9, expectation: expectRejectedRecovery},
		{name: "wrong id", state: StateStashed, rowID: 10, expectation: expectRejectedRecovery},
		{name: "wrong label", state: StateStashed, rowID: 9, label: "wrong-label", expectation: expectRejectedRecovery},
		{name: "wrong group", state: StateStashed, rowID: 9, group: "default", expectation: expectRejectedRecovery},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runInspectionStagedRecoveryCase(t, testCase)
		})
	}
}

// stagedQueueLockMode names the queue identity locks a staged transition must
// hold from its commit callback through its effective enqueue.
type stagedQueueLockMode string

const (
	// stagedSharedIdentity is concurrent submission: it holds only the shared
	// queue identity lease. Writer intent and the gate are not its locks, so a
	// waiting writer may hold them.
	stagedSharedIdentity stagedQueueLockMode = "shared queue identity lease"
	// stagedExclusiveMutation is recovery: writer intent, gate and identity
	// are held exclusively and the intent is committed.
	stagedExclusiveMutation stagedQueueLockMode = "exclusive queue identity mutation"
)

func (m stagedQueueLockMode) held(ctx context.Context, base string) error {
	switch m {
	case stagedSharedIdentity:
		return requirePrivateLockState(ctx, filepath.Join(base, privateQueueIdentityLockName), privateLockShared)
	case stagedExclusiveMutation:
		return privateQueueIdentityMutationHeld(ctx, base)
	default:
		return fmt.Errorf("unknown staged queue lock mode %q", m)
	}
}

// stagedTransition owns one staged submission or recovery under test: its
// scope, its cancelable run context, the release-once commit barrier, and the
// facts its commit callback, command observer and competing writer record
// under mu. Callbacks only record facts; the test goroutine asserts them.
// Scope cleanup cancels the run before it releases the commit, then joins the
// transition and the writer and settles any returned pending command.
type stagedTransition struct {
	scope         *testCallScope
	base          string
	mode          stagedQueueLockMode
	run           context.Context
	cancelRun     context.CancelFunc
	probe         context.Context
	entered       chan struct{}
	enterOnce     sync.Once
	released      <-chan struct{}
	releaseCommit func()

	mu    sync.Mutex
	facts stagedTransitionFacts
}

// stagedTransitionFacts is the synchronized record of one transition. Lock
// errors are nil when every probe found the mode's locks held.
type stagedTransitionFacts struct {
	commits                  int
	enqueuesAtCommit         int
	commitLocks              error
	enqueueEntries           int
	enqueueCompletions       int
	enqueueLocks             error
	writerEntered            bool
	completionsAtWriterEntry int
}

type stagedTransitionResult struct {
	err         error
	pending     *Pending
	observation string
}

func (r stagedTransitionResult) pendingCommand() *Pending { return r.pending }

func inspectionTransitionResult(observation InspectionObservation, err error) stagedTransitionResult {
	return stagedTransitionResult{err: err, pending: observation.Pending, observation: fmt.Sprintf("%+v", observation)}
}

// newStagedTransition must follow the fixture so its cleanup runs first. It
// wraps the client's existing observer, which still sees every event.
func newStagedTransition(t *testing.T, client *Client, stateRoot string, mode stagedQueueLockMode) *stagedTransition {
	t.Helper()
	scope := newTestCallScope(t, client.options)
	transition := &stagedTransition{
		scope:   scope,
		base:    filepath.Join(stateRoot, privateSupervisorDirectory),
		mode:    mode,
		probe:   scope.context(),
		entered: make(chan struct{}),
	}
	transition.run, transition.cancelRun = scope.cancelableContext()
	transition.released, transition.releaseCommit = scope.barrier()
	next := client.options.Observer
	client.options.Observer = func(event CommandEvent) {
		transition.observe(event)
		if next != nil {
			next(event)
		}
	}
	return transition
}

func (s *stagedTransition) start(run func(context.Context) stagedTransitionResult) *testCall[stagedTransitionResult] {
	return startTestCall(s.scope, func() stagedTransitionResult { return run(s.run) })
}

// commit is the application commit. It records lock evidence at its
// boundary, announces entry and waits for release. It returns nil, so only
// the run context decides whether the enqueue follows.
func (s *stagedTransition) commit() error {
	locks := s.mode.held(s.probe, s.base)
	s.mu.Lock()
	s.facts.commits++
	s.facts.enqueuesAtCommit = s.facts.enqueueEntries
	s.facts.commitLocks = errors.Join(s.facts.commitLocks, locks)
	s.mu.Unlock()
	s.enterOnce.Do(func() { close(s.entered) })
	<-s.released
	return nil
}

// observe probes the mode's locks at every stage of an effective enqueue.
// The completed event precedes pending completion and lease release.
func (s *stagedTransition) observe(event CommandEvent) {
	if !effectiveEnqueueCommand(event.Argv) {
		return
	}
	locks := s.mode.held(s.probe, s.base)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch event.Stage {
	case "entry":
		s.facts.enqueueEntries++
	case "completed":
		s.facts.enqueueCompletions++
	}
	if locks != nil {
		s.facts.enqueueLocks = errors.Join(s.facts.enqueueLocks, fmt.Errorf("enqueue %s: %w", event.Stage, locks))
	}
}

// startWriter starts an exclusive queue identity writer on a context the
// test never cancels. Its entry records how many enqueues had completed.
func (s *stagedTransition) startWriter(client *Client) *testCall[lockCallResult] {
	return startQueueIdentityMutation(s.scope, client, func(context.Context) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.facts.writerEntered = true
		s.facts.completionsAtWriterEntry = s.facts.enqueueCompletions
		return nil
	})
}

func (s *stagedTransition) snapshot() stagedTransitionFacts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.facts
}

// recoverInspectionTransition recovers row id through transition's commit
// after requiring the exact Stashed row with its terminal event registered.
func recoverInspectionTransition(client *Client, identity InspectionIdentity, id int64, transition *stagedTransition) func(context.Context) stagedTransitionResult {
	return func(ctx context.Context) stagedTransitionResult {
		observation, err := client.RecoverInspectionStaged(ctx, identity, id, func(candidate InspectionObservation) error {
			if !candidate.Matched || candidate.Job.State != StateStashed || candidate.Job.ID != id || !candidate.TerminalEventRegistered {
				return fmt.Errorf("recovery commit received an unregistered or wrong staged row: %+v", candidate)
			}
			return transition.commit()
		})
		return inspectionTransitionResult(observation, err)
	}
}

// assertStagedTransitionHoldsQueueIdentity starts a competing exclusive
// writer at the commit barrier and releases the commit. The writer's entry
// after the single enqueue completed is the release proof.
func assertStagedTransitionHoldsQueueIdentity(t *testing.T, transition *stagedTransition, client *Client, call *testCall[stagedTransitionResult]) {
	t.Helper()
	awaitEntry(transition.scope, transition.entered, call, "staged transition did not reach its commit callback")
	writer := transition.startWriter(client)
	transition.releaseCommit()
	if result := awaitCall(transition.scope, call, "staged transition"); result.err != nil {
		t.Fatalf("staged transition failed: err=%v observation=%s", result.err, result.observation)
	}
	awaitLockCall(transition.scope, writer, "queue identity writer")
	facts := transition.snapshot()
	assertStagedTransitionFacts(t, transition.mode, facts, 1)
	if !facts.writerEntered || facts.completionsAtWriterEntry != 1 {
		t.Fatalf("queue identity writer entered before the staged enqueue completed: entered=%t completions=%d", facts.writerEntered, facts.completionsAtWriterEntry)
	}
}

func assertStagedTransitionFacts(t *testing.T, mode stagedQueueLockMode, facts stagedTransitionFacts, enqueues int) {
	t.Helper()
	if facts.commits != 1 || facts.enqueuesAtCommit != 0 || facts.commitLocks != nil {
		t.Fatalf("%s was not held at one commit before enqueue: commits=%d enqueues_at_commit=%d err=%v", mode, facts.commits, facts.enqueuesAtCommit, facts.commitLocks)
	}
	if facts.enqueueEntries != enqueues || facts.enqueueCompletions != enqueues || facts.enqueueLocks != nil {
		t.Fatalf("%s did not cover %d effective enqueues: entries=%d completions=%d err=%v", mode, enqueues, facts.enqueueEntries, facts.enqueueCompletions, facts.enqueueLocks)
	}
}

// startQueueIdentityLease and startQueueIdentityMutation run one private
// queue identity reader or writer callback as a scope call.
func startQueueIdentityLease(scope *testCallScope, client *Client, callback func(context.Context) error) *testCall[lockCallResult] {
	ctx := scope.context()
	return startTestCall(scope, func() lockCallResult {
		return lockCallResult{err: client.withPrivateQueueIdentityLease(ctx, callback)}
	})
}

func startQueueIdentityMutation(scope *testCallScope, client *Client, callback func(context.Context) error) *testCall[lockCallResult] {
	ctx := scope.context()
	return startTestCall(scope, func() lockCallResult {
		return lockCallResult{err: client.withPrivateQueueIdentityMutation(ctx, callback)}
	})
}
