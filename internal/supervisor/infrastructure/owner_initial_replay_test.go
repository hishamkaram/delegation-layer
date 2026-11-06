package infrastructure

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnerInitialDispatcherAndExplicitReplay(t *testing.T) {
	for _, kind := range []InitialOperationKind{InitialPrepareContinuation, InitialRecoverSaved, InitialMigrateOwn} {
		t.Run(string(kind), func(t *testing.T) { exerciseOwnerInitialReplay(t, kind) })
	}
}

func exerciseOwnerInitialReplay(t *testing.T, kind InitialOperationKind) {
	t.Helper()
	var calls, own atomic.Int32
	var expected LaunchContext
	var expectedSource SavedBindingSource
	check := func(selected LaunchContext, executed InitialOperationKind) {
		if executed != kind {
			t.Errorf("initial %s dispatched %s", kind, executed)
		}
		calls.Add(1)
		if !slices.Equal(selected.ControlEnvironment, expected.ControlEnvironment) || selected.Root != expected.Root {
			t.Error("initial operation changed accepted context")
		}
	}
	host := &ownerServerTestHost{
		ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
			own.Add(1)
			return Disposition{}, errors.New("non-Own initial operation dispatched Own")
		},
		continuation: func(_ context.Context, _ Operation, c LaunchContext, source SavedBindingSource, _ ResidentGuards, _ DaemonWaitOwner) (Disposition, error) {
			check(c, InitialPrepareContinuation)
			if source != expectedSource {
				t.Error("continuation changed the accepted source")
			}
			return Disposition{Kind: DispositionWarmReuse}, nil
		},
		recoverSaved: func(_ context.Context, _ Operation, c LaunchContext, _ SavedBindingSource, _ ResidentGuards, _ DaemonWaitOwner) (Disposition, error) {
			check(c, InitialRecoverSaved)
			return Disposition{Kind: DispositionWarmReuse}, nil
		},
		manual: func(_ context.Context, _ Operation, c LaunchContext, _ ResidentGuards, _ DaemonWaitOwner) (Disposition, ManualMigrationReport, error) {
			check(c, InitialMigrateOwn)
			return Disposition{}, ManualMigrationReport{State: ManualMigrationNotInitialized, Legacy: ManualMigrationLegacyNotPresent, Active: ManualMigrationActiveNone}, nil
		},
	}
	f := newOwnerServerFixtureWithSelection(t, host, ownerServerFixtureSelection{initial: kind})
	expected = f.birthContext(t)
	expectedSource = f.entry.operation.Intent().Inputs.Initial.Source
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.start()
	expectedOwner := f.request(t, ownerStatus).Owner
	request := PreparationRequest{Operation: f.entry.operation.Intent().Inputs.Initial, InitialResultFor: expectedOwner}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var first PreparationOutcome
	for _, selected := range []LaunchContextReference{{}, f.entry.operation.defaultContextReference()} {
		request.Context = selected
		outcome, err := RequestBoundPreparation(ctx, contextPassiveReader(t, f.entry.journal), expectedOwner, request)
		if err != nil || outcome.Condition != PreparationOK || outcome.Startup != expectedOwner {
			t.Fatalf("initial replay lost outcome: %v %v", outcome, err)
		}
		if first == (PreparationOutcome{}) {
			first = outcome
		} else if first != outcome {
			t.Fatal("zero and explicit default did not select the same immutable result")
		}
	}
	if calls.Load() != 1 || own.Load() != 0 {
		t.Fatalf("initial operation repeated or called Own: calls=%d own=%d", calls.Load(), own.Load())
	}
}

func TestOwnerFreshSavedAfterFailureRunsAgain(t *testing.T) {
	var calls atomic.Int32
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{recoverSaved: func(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
		if calls.Add(1) == 2 {
			return Disposition{}, io.ErrUnexpectedEOF
		}
		return Disposition{Kind: DispositionWarmReuse}, nil
	}}, ownerServerFixtureSelection{initial: InitialRecoverSaved})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.start()
	owner := f.request(t, ownerStatus).Owner
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request := PreparationRequest{Operation: f.entry.operation.Intent().Inputs.Initial}
	for _, condition := range []PreparationCondition{PreparationFault, PreparationOK} {
		outcome, err := RequestBoundPreparation(ctx, contextPassiveReader(t, f.entry.journal), owner, request)
		if err != nil || outcome.Condition != condition {
			t.Fatalf("fresh request reused an old result: outcome=%v err=%v", outcome, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("fresh identical subjects did not rerun: %d", calls.Load())
	}
}

func TestOwnerReplaySelectorRefusedBeforeDialOrHost(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	wrong := f.auth.request.Owner
	wrong.Sequence++
	if _, err := RequestBoundPreparation(ctx, f.reader, f.auth.request.Owner, PreparationRequest{InitialResultFor: wrong}); !errors.Is(err, ErrIdentity) {
		t.Fatalf("wrong selector not refused: %v", err)
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
	var calls atomic.Int32
	b := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{recoverSaved: func(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		return Disposition{Kind: DispositionWarmReuse}, nil
	}}, ownerServerFixtureSelection{initial: InitialRecoverSaved})
	b.start()
	owner := b.request(t, ownerStatus).Owner
	selection := PreparationRequest{Operation: b.entry.operation.Intent().Inputs.Initial, InitialResultFor: owner}
	selection.Operation.Source.OwnerID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	outcome, err := RequestBoundPreparation(ctx, contextPassiveReader(t, b.entry.journal), owner, selection)
	if err != nil || outcome.Condition != PreparationRefused || calls.Load() != 0 {
		t.Fatalf("wrong replay subject reached host: outcome=%v calls=%d err=%v", outcome, calls.Load(), err)
	}
}

func TestOwnerEarlyInitialExitClosesReplayCompletion(t *testing.T) {
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{}, ownerServerFixtureSelection{initial: InitialRecoverSaved})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.server.prepareInitial(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("early cancellation lost: %v", err)
	}
	select {
	case <-f.server.initialDone:
	default:
		t.Fatal("pre-registration exit left replay completion open")
	}
	f.start()
	owner := f.request(t, ownerStatus).Owner
	replay, cancelReplay := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelReplay()
	outcome, err := RequestBoundPreparation(replay, contextPassiveReader(t, f.entry.journal), owner, PreparationRequest{Operation: f.entry.operation.Intent().Inputs.Initial, InitialResultFor: owner})
	if err != nil || outcome.Condition != PreparationFault || outcome.Failure.Causes&PreparationCauseCanceled == 0 {
		t.Fatalf("early error lost from retained outcome: %v %v", outcome, err)
	}
}

func startOwnerInitialTest(t *testing.T, f *ownerServerFixture, release func()) func() error {
	t.Helper()
	done := make(chan error, 1)
	var once sync.Once
	var result error
	join := func() error {
		once.Do(func() {
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				f.retain = true
				t.Error("initial actor did not join")
				result = context.DeadlineExceeded
			}
		})
		return result
	}
	t.Cleanup(func() {
		if release != nil {
			release()
		}
		if err := join(); err != nil {
			t.Logf("initial outcome: %v", err)
		}
	})
	go func() { _, err := f.server.prepareInitial(t.Context()); done <- err }()
	return join
}

func TestOwnerReplayTimeoutDoesNotCancelInitial(t *testing.T) {
	entered := make(chan context.Context, 1)
	expired := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls atomic.Int32
	host := &ownerServerTestHost{recoverSaved: func(ctx context.Context, _ Operation, _ LaunchContext, _ SavedBindingSource, _ ResidentGuards, _ DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		entered <- ctx
		<-release
		return Disposition{Kind: DispositionWarmReuse}, nil
	}}
	host.classify = func(err error) PreparationFailure {
		if errors.Is(err, context.DeadlineExceeded) {
			select {
			case expired <- struct{}{}:
			default:
			}
		}
		return host.componentEntryHost.ClassifyPreparation(err)
	}
	f := newOwnerServerFixtureWithSelection(t, host, ownerServerFixtureSelection{initial: InitialRecoverSaved})
	join := startOwnerInitialTest(t, f, unblock)
	initial := awaitOwnerServerHost(t, entered)
	f.start()
	owner := f.request(t, ownerStatus).Owner
	request := PreparationRequest{Operation: f.entry.operation.Intent().Inputs.Initial, InitialResultFor: owner}
	short, cancelShort := context.WithTimeout(t.Context(), 500*time.Millisecond)
	outcome, err := RequestBoundPreparation(short, contextPassiveReader(t, f.entry.journal), owner, request)
	cancelShort()
	if !errors.Is(err, context.DeadlineExceeded) && outcome.Condition != PreparationTimeout {
		t.Fatalf("replay wait did not time out: outcome=%v err=%v", outcome, err)
	}
	select {
	case <-expired:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach the initial-outcome wait boundary")
	}
	if initial.Err() != nil || calls.Load() != 1 {
		t.Fatalf("request timeout canceled or reran initial: ctx=%v calls=%d", initial.Err(), calls.Load())
	}
	unblock()
	if err = join(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	outcome, err = RequestBoundPreparation(ctx, contextPassiveReader(t, f.entry.journal), owner, request)
	if err != nil || outcome.Condition != PreparationOK || calls.Load() != 1 {
		t.Fatalf("initial outcome lost after timed-out waiter: outcome=%v calls=%d err=%v", outcome, calls.Load(), err)
	}
}

func TestOwnerOwnInitialSuccessSelectorRetainsLegacyRerun(t *testing.T) {
	var calls atomic.Int32
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.start()
	owner := f.request(t, ownerStatus).Owner
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	outcome, err := RequestBoundPreparation(ctx, contextPassiveReader(t, f.entry.journal), owner, PreparationRequest{InitialResultFor: owner})
	if err != nil || outcome.Condition != PreparationOK || calls.Load() != 2 {
		t.Fatalf("Own selector changed legacy success behavior: calls=%d outcome=%v err=%v", calls.Load(), outcome, err)
	}
}
