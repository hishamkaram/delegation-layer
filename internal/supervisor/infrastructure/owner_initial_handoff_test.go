package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOwnerInitialFailureBufferedReplySurvivesTerminalEndpointClosure(t *testing.T) {
	for _, failure := range []PreparationFailure{
		{Causes: PreparationCausePrivateMigrationRequired | PreparationCauseBinding, Reason: PreparationReasonStateUnreadable, IndependentFault: true},
		{Causes: PreparationCauseBinding | PreparationCauseEndpointRefused, Reason: PreparationReasonEndpointRefusedLive, IndependentFault: true},
	} {
		t.Run(string(failure.Reason), func(t *testing.T) { exerciseOwnerBufferedTerminalReply(t, failure) })
	}
}

func exerciseOwnerBufferedTerminalReply(t *testing.T, failure PreparationFailure) {
	t.Helper()
	var calls atomic.Int32
	cause := errors.Join(errors.New("opaque preparation failure"), io.ErrUnexpectedEOF)
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{
		recoverSaved: func(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
			calls.Add(1)
			return Disposition{}, cause
		},
		classify: func(error) PreparationFailure { return failure },
	}, ownerServerFixtureSelection{initial: InitialRecoverSaved, deadline: time.Now().Add(8 * time.Second)})
	intent, assumption, publication, _, err := f.server.readResident(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request := f.request(t, ownerRecoverSaved)
	request.Source = intent.Inputs.Initial.Source
	request.InitialResultFor = request.Owner
	pin := f.entry.journal.pin
	reader := contextPassiveReader(t, f.entry.journal)
	connection := f.dial(t)
	if err = authenticateOwnerPeer(t.Context(), connection, intent, assumption, publication); err != nil {
		t.Fatal(err)
	}
	if err = writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	_, done, join := startOwnerRuntimeTest(t, f)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal initial result did not finish serving")
	}
	if err = join(); !errors.Is(err, cause) {
		t.Fatalf("runtime lost original preparation graph: %v", err)
	}
	if _, statErr := os.Lstat(publication.Endpoint.Path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("fixture must prove canonical endpoint closure: %v", statErr)
	}
	reply, err := readOwnerFrame[ownerReply](bufio.NewReader(connection))
	if err != nil {
		t.Fatalf("full buffered reply lost after server closure: %v", err)
	}
	if err = validateOwnerReplyBinding(intent, assumption, publication, request, reply); err != nil {
		t.Fatal(err)
	}
	if reply.Code != ownerFault || reply.Failure != failure || !reply.Failure.IndependentFault || calls.Load() != 1 {
		t.Fatalf("classified initial failure lost: calls=%d reply=%v", calls.Load(), reply)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = readOwnerContactAfterReply(ctx, reader, pin, request, reply); err != nil {
		t.Fatalf("verified failure incorrectly requires a retired endpoint: %v", err)
	}
}

func startOwnerRuntimeTest(t *testing.T, f *ownerServerFixture) (context.CancelFunc, <-chan struct{}, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var result error
	var once sync.Once
	join := func() error {
		once.Do(func() {
			select {
			case <-done:
				f.expectedCleanupFault = result
				f.server = nil
			case <-time.After(12 * time.Second):
				f.retain = true
				t.Error("runtime actors failed to join")
				result = context.DeadlineExceeded
			}
		})
		return result
	}
	t.Cleanup(func() {
		cancel()
		if err := join(); err != nil {
			t.Logf("runtime outcome: %v", err)
		}
	})
	go func() { result = runResidentServer(ctx, f.server); close(done) }()
	return cancel, done, join
}

func TestOwnerTerminalHandoffRejectsFreshMismatchAndStatusWithoutHost(t *testing.T) {
	var calls atomic.Int32
	failure := PreparationFailure{Causes: PreparationCauseBinding, Reason: PreparationReasonEndpointRefusedLive, IndependentFault: true}
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{
		recoverSaved: func(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
			calls.Add(1)
			return Disposition{}, io.ErrUnexpectedEOF
		},
		classify: func(error) PreparationFailure { return failure },
	}, ownerServerFixtureSelection{initial: InitialRecoverSaved})
	if _, err := f.server.prepareInitial(t.Context()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	f.start()
	owner := f.request(t, ownerStatus).Owner
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reader := contextPassiveReader(t, f.entry.journal)
	selection := PreparationRequest{Requirements: warmOwnerRequirements(f.entry.operation.Artifacts()), Operation: f.entry.operation.Intent().Inputs.Initial}
	outcome, err := RequestBoundPreparation(ctx, reader, owner, selection)
	if err != nil || outcome.Condition != PreparationBusy {
		t.Fatalf("fresh handoff request was admitted: %v %v", outcome, err)
	}
	selection.InitialResultFor = owner
	selection.Operation.Source.OwnerID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	outcome, err = RequestBoundPreparation(ctx, reader, owner, selection)
	if err != nil || outcome.Condition != PreparationBusy {
		t.Fatalf("mismatched handoff request was not busy: %v %v", outcome, err)
	}
	if _, err = ProbeOwner(ctx, reader); !errors.Is(err, ErrOwnerBusy) {
		t.Fatalf("status during handoff did not return busy: %v", err)
	}
	selection.Operation = f.entry.operation.Intent().Inputs.Initial
	outcome, err = RequestBoundPreparation(ctx, reader, owner, selection)
	if err != nil || outcome.Condition != PreparationFault || outcome.Failure != failure || calls.Load() != 1 {
		t.Fatalf("exact handoff outcome lost/reran: %v calls=%d err=%v", outcome, calls.Load(), err)
	}
}

func TestOwnerUndeliveredHandoffEndsAtOriginalDeadline(t *testing.T) {
	entered := make(chan context.Context, 1)
	var calls atomic.Int32
	deadline := time.Now().Add(2 * time.Second)
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{recoverSaved: func(ctx context.Context, _ Operation, _ LaunchContext, _ SavedBindingSource, _ ResidentGuards, _ DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		entered <- ctx
		return Disposition{}, io.ErrUnexpectedEOF
	}}, ownerServerFixtureSelection{initial: InitialRecoverSaved, deadline: deadline})
	_, done, join := startOwnerRuntimeTest(t, f)
	initial := awaitOwnerServerHost(t, entered)
	if actual, finite := initial.Deadline(); !finite || !actual.Equal(deadline) {
		t.Fatal("handoff renewed original initial deadline")
	}
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("undelivered initial failure outlived original deadline")
	}
	if err := join(); !errors.Is(err, io.ErrUnexpectedEOF) || time.Now().Before(deadline) || calls.Load() != 1 {
		t.Fatalf("wrong handoff closure: calls=%d err=%v", calls.Load(), err)
	}
}

func TestOwnerInvalidInitialClassificationClosesWithoutHandoff(t *testing.T) {
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{
		recoverSaved: func(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
			return Disposition{}, io.ErrUnexpectedEOF
		},
		classify: func(error) PreparationFailure { return PreparationFailure{Causes: PreparationCauses(1 << 31)} },
	}, ownerServerFixtureSelection{initial: InitialRecoverSaved})
	_, done, join := startOwnerRuntimeTest(t, f)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("invalid classification entered terminal handoff")
	}
	if err := join(); !errors.Is(err, ErrIdentity) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("invalid classification lost source graph: %v", err)
	}
}

func TestOwnerRetainedInitialBusyClosesAndRetainsActualGraph(t *testing.T) {
	var retained *retainedPreparationTestError
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{recoverSaved: func(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
		return Disposition{}, retained
	}}, ownerServerFixtureSelection{initial: InitialRecoverSaved})
	retained = newRetainedPreparationTestError(t, f.root)
	retained.cause = task.ErrLockBusy
	_, done, join := startOwnerRuntimeTest(t, f)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retained busy entered terminal handoff")
	}
	err := join()
	var actual *retainedPreparationTestError
	if !errors.As(err, &actual) || actual != retained {
		t.Fatalf("runtime discarded actual retained owner: %v", err)
	}
	exclusive, acquireErr := retained.store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	if exclusive != nil {
		if releaseErr := exclusive.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}
	if !errors.Is(acquireErr, task.ErrLockBusy) {
		t.Fatalf("retained root SH disappeared: %v", acquireErr)
	}
}
