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
	firstDone := startOrdinaryRecovery(fake.client, identity, 7)
	transition.wait(t)
	secondCallback := false
	second, secondErr := fake.client.RecoverStaged(context.Background(), identity, 7, func(Observation) error {
		secondCallback = true
		return nil
	})
	first := <-firstDone
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
	firstDone := startInspectionRecovery(fake.client, identity, 9)
	transition.wait(t)
	secondCallback := false
	second, secondErr := fake.client.RecoverInspectionStaged(context.Background(), identity, 9, func(InspectionObservation) error {
		secondCallback = true
		return nil
	})
	first := <-firstDone
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
	commitEntered := make(chan struct{})
	releaseCommit := make(chan struct{})
	submissionDone := make(chan error, 1)
	go func() {
		_, err := fake.client.SubmitConcurrentInspectionStaged(t.Context(), permit, identity, launch, func(candidate InspectionObservation) error {
			if !candidate.Matched || candidate.Job.State != StateStashed || candidate.Job.ID != 9 {
				return errors.New("submission commit received the wrong staged row")
			}
			close(commitEntered)
			<-releaseCommit
			return nil
		})
		submissionDone <- err
	}()
	assertStagedTransitionBlocksQueueIdentityMutation(t, fake.client, commitEntered, releaseCommit, submissionDone)
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
	fake, _ := newPrivateInspectionSupervisor(t, "ok")
	_, _, identity, _ := prepareStagedOrdinarySubmission(t, fake)
	writeStagedOrdinaryStatus(t, fake, identity, 7, StateStashed, "", "default")
	commitEntered := make(chan struct{})
	releaseCommit := make(chan struct{})
	recoveryDone := make(chan error, 1)
	go func() {
		_, err := fake.client.RecoverStaged(t.Context(), identity, 7, func(Observation) error {
			close(commitEntered)
			<-releaseCommit
			return nil
		})
		recoveryDone <- err
	}()
	assertStagedTransitionBlocksQueueIdentityMutation(t, fake.client, commitEntered, releaseCommit, recoveryDone)
}

func TestRecoverInspectionStagedHoldsQueueIdentityThroughRegistrationCommitAndEnqueue(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	writeStagedInspectionStatus(t, fake, identity, 9, StateStashed, "", "default")
	commitEntered := make(chan struct{})
	releaseCommit := make(chan struct{})
	recoveryDone := make(chan error, 1)
	go func() {
		_, err := fake.client.RecoverInspectionStaged(t.Context(), identity, 9, func(candidate InspectionObservation) error {
			if !candidate.TerminalEventRegistered {
				return errors.New("terminal callback was not registered before commit")
			}
			close(commitEntered)
			<-releaseCommit
			return nil
		})
		recoveryDone <- err
	}()
	assertStagedTransitionBlocksQueueIdentityMutation(t, fake.client, commitEntered, releaseCommit, recoveryDone)
}

func TestPrivateQueueWriterIntentBlocksNewReaders(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	initialEntered := make(chan struct{})
	releaseInitial := make(chan struct{})
	releaseInitialOnce := sync.OnceFunc(func() { close(releaseInitial) })
	t.Cleanup(releaseInitialOnce)
	initialDone := make(chan error, 1)
	go func() {
		initialDone <- fake.client.withPrivateQueueIdentityLease(t.Context(), func(context.Context) error {
			close(initialEntered)
			<-releaseInitial
			return nil
		})
	}()
	waitForStagedCommit(t, initialEntered, "initial queue identity reader did not enter")

	writerEntered := make(chan struct{})
	releaseWriter := make(chan struct{})
	releaseWriterOnce := sync.OnceFunc(func() { close(releaseWriter) })
	t.Cleanup(releaseWriterOnce)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- fake.client.withPrivateQueueIdentityMutation(t.Context(), func(context.Context) error {
			close(writerEntered)
			<-releaseWriter
			return nil
		})
	}()
	waitForPrivateQueueWriterIntent(t, store.Root)

	const readerCount = 32
	readerEntered := make(chan struct{}, readerCount)
	readerDone := make(chan error, readerCount)
	for range readerCount {
		go func() {
			readerDone <- fake.client.withPrivateQueueIdentityLease(t.Context(), func(context.Context) error {
				readerEntered <- struct{}{}
				return nil
			})
		}()
	}
	assertNoQueueIdentityReader(t, readerEntered, "new reader overtook a waiting writer")
	releaseInitialOnce()
	waitForStagedCommit(t, writerEntered, "writer did not enter after the existing reader drained")
	assertNoQueueIdentityReader(t, readerEntered, "new reader crossed an active writer")
	releaseWriterOnce()
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-initialDone; err != nil {
		t.Fatal(err)
	}
	for range readerCount {
		select {
		case <-readerEntered:
		case <-time.After(2 * time.Second):
			t.Fatal("reader did not enter after writer completion")
		}
		if err := <-readerDone; err != nil {
			t.Fatal(err)
		}
	}
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

func waitForPrivateQueueWriterIntent(t *testing.T, stateRoot string) {
	t.Helper()
	path := filepath.Join(stateRoot, privateSupervisorDirectory, privateQueueWriterIntentLockName)
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == privateQueueWriterWaitingState {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("writer intent was not published: state=%q err=%v", data, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertNoQueueIdentityReader(t *testing.T, entered <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-entered:
		t.Fatal(message)
	case <-time.After(100 * time.Millisecond):
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
	originalCommitEntered, releaseOriginalCommit, originalDone, originalCommitted := startBlockedConcurrentInspectionSubmission(t, fake, permit, identity, launch)
	waitForStagedCommit(t, originalCommitEntered, "original submission did not reach its durable commit barrier")
	var recoveryCommitted atomic.Bool
	recoveryDone := startInspectionRecoveryWithCommitFlag(fake.client, identity, 9, &recoveryCommitted)
	assertInspectionRecoveryBlocked(t, recoveryDone)
	if got := transition.entries.Load(); got != 0 {
		t.Fatalf("enqueue crossed the durable commit barrier: %d", got)
	}
	close(releaseOriginalCommit)
	transition.wait(t)
	recovery := <-recoveryDone
	original := <-originalDone
	assertSerializedInspectionSubmission(t, fake, transition, recovery, recoveryCommitted.Load(), original, *originalCommitted)
}

func startBlockedConcurrentInspectionSubmission(t *testing.T, fake *fakeSupervisor, permit InspectionSubmissionPermit, identity InspectionIdentity, launch Launch) (<-chan struct{}, chan<- struct{}, <-chan inspectionRecoveryResult, *InspectionObservation) {
	t.Helper()
	commitEntered := make(chan struct{})
	releaseCommit := make(chan struct{})
	done := make(chan inspectionRecoveryResult, 1)
	committed := &InspectionObservation{}
	go func() {
		observation, submitErr := fake.client.SubmitConcurrentInspectionStaged(t.Context(), permit, identity, launch, func(candidate InspectionObservation) error {
			*committed = candidate
			if enqueueCount := len(stagedInvocations(t, fake, "enqueue")); enqueueCount != 0 {
				return fmt.Errorf("original enqueue ran before its durable commit returned: %d", enqueueCount)
			}
			close(commitEntered)
			<-releaseCommit
			return nil
		})
		done <- inspectionRecoveryResult{observation: observation, err: submitErr}
	}()
	return commitEntered, releaseCommit, done, committed
}

func startInspectionRecoveryWithCommitFlag(client *Client, identity InspectionIdentity, id int64, committed *atomic.Bool) <-chan inspectionRecoveryResult {
	done := make(chan inspectionRecoveryResult, 1)
	go func() {
		observation, err := client.RecoverInspectionStaged(context.Background(), identity, id, func(InspectionObservation) error {
			committed.Store(true)
			return nil
		})
		done <- inspectionRecoveryResult{observation: observation, err: err}
	}()
	return done
}

func assertInspectionRecoveryBlocked(t *testing.T, done <-chan inspectionRecoveryResult) {
	t.Helper()
	select {
	case recovery := <-done:
		t.Fatalf("recovery crossed the original submission's queue identity lease: observation=%+v err=%v", recovery.observation, recovery.err)
	case <-time.After(100 * time.Millisecond):
	}
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

func waitForStagedCommit(t *testing.T, entered <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

func assertStagedTransitionBlocksQueueIdentityMutation(t *testing.T, client *Client, commitEntered <-chan struct{}, releaseCommit chan<- struct{}, recoveryDone <-chan error) {
	t.Helper()
	select {
	case <-commitEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("staged recovery did not reach its commit callback")
	}
	mutationEntered := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- client.withPrivateQueueIdentityMutation(t.Context(), func(context.Context) error {
			close(mutationEntered)
			return nil
		})
	}()
	select {
	case <-mutationEntered:
		t.Fatal("queue identity mutation crossed an unfinished staged recovery")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseCommit)
	if err := <-recoveryDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-mutationEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("queue identity mutation did not enter after staged recovery")
	}
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
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
