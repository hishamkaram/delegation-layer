package infrastructure

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnerReplayRefusesDifferentSubjectWithoutRepeatingHost(t *testing.T) {
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls atomic.Int32
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{recoverSaved: func(ctx context.Context, _ Operation, _ LaunchContext, _ SavedBindingSource, _ ResidentGuards, _ DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		entered <- ctx
		<-release
		return Disposition{Kind: DispositionWarmReuse}, nil
	}}, ownerServerFixtureSelection{initial: InitialRecoverSaved})
	join := startOwnerInitialTest(t, f, unblock)
	awaitOwnerServerHost(t, entered)
	f.start()
	owner := f.request(t, ownerStatus).Owner
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	selection := PreparationRequest{Operation: f.entry.operation.Intent().Inputs.Initial, InitialResultFor: owner}
	selection.Operation.Source.OwnerID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	reader := contextPassiveReader(t, f.entry.journal)
	outcome, err := RequestBoundPreparation(ctx, reader, owner, selection)
	if err != nil || outcome.Condition != PreparationRefused || calls.Load() != 1 {
		t.Fatalf("wrong subject consumed initial work: outcome=%v calls=%d err=%v", outcome, calls.Load(), err)
	}
	unblock()
	if err = join(); err != nil {
		t.Fatal(err)
	}
	selection.Operation = f.entry.operation.Intent().Inputs.Initial
	outcome, err = RequestBoundPreparation(ctx, reader, owner, selection)
	if err != nil || outcome.Condition != PreparationOK || calls.Load() != 1 {
		t.Fatalf("matching replay repeated host or lost result: %v calls=%d err=%v", outcome, calls.Load(), err)
	}
}

func TestOwnerInitialFailureContextMismatchDoesNotConsumeResult(t *testing.T) {
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{recoverSaved: func(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
		return Disposition{}, context.Canceled
	}}, ownerServerFixtureSelection{initial: InitialRecoverSaved})
	if _, err := f.server.prepareInitial(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	changed := f.birthContext(t)
	changed.ControlEnvironment = append(changed.ControlEnvironment, "FIXTURE_SELECTED_CONTEXT=other")
	reference, err := StageLaunchContext(t.Context(), f.entry.journal.store, changed)
	if err != nil {
		t.Fatal(err)
	}
	f.start()
	owner := f.request(t, ownerStatus).Owner
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	selection := PreparationRequest{Operation: f.entry.operation.Intent().Inputs.Initial, InitialResultFor: owner, Context: reference}
	reader := contextPassiveReader(t, f.entry.journal)
	outcome, err := RequestBoundPreparation(ctx, reader, owner, selection)
	if err != nil || outcome.Condition != PreparationRefused {
		t.Fatalf("different context did not refuse: %v %v", outcome, err)
	}
	select {
	case <-f.server.initialDelivered:
		t.Fatal("wrong context consumed initial failure")
	default:
	}
	selection.Context = f.entry.operation.defaultContextReference()
	outcome, err = RequestBoundPreparation(ctx, reader, owner, selection)
	if err != nil || outcome.Condition != PreparationFault || outcome.Failure.Causes&PreparationCauseCanceled == 0 {
		t.Fatalf("matching cached failure lost: %v %v", outcome, err)
	}
}

func TestOwnerRequestAuthenticationPrecedesHost(t *testing.T) {
	var calls atomic.Int32
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	f.start()
	request := f.request(t, ownerEnsureOwn)
	request.Client.PID++
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	if reply := readServerReply(t, connection); reply.Code != ownerRefused || calls.Load() != 0 {
		t.Fatalf("unauthenticated request reached host: %v", reply)
	}
}

func TestOwnerEstablishmentReusesIndependentTarget(t *testing.T) {
	var calls atomic.Int32
	b := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	b.start()
	// Different accepted component data must not force an already serving B to
	// match A's installation. This does not claim native runner eligibility.
	different := b.entry.operation.Artifacts()
	different.Runner = filepath.Join(b.root, "another-installed-runner")
	a := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{}, ownerServerFixtureSelection{artifacts: different})
	a.start()
	target, err := NewOrigin(b.root, b.entry.operation.Intent().Inputs.RootID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	established, err := RequestOtherEstablishment(ctx, contextPassiveReader(t, a.entry.journal), a.request(t, ownerStatus).Owner, target, b.entry.operation.defaultContextReference(), InitialOperation{})
	if err != nil || established.Owner != b.request(t, ownerStatus).Owner || calls.Load() != 0 {
		t.Fatalf("A replaced or prepared serving B: owner=%v calls=%d err=%v", established, calls.Load(), err)
	}
	outcome, err := RequestBoundPreparation(ctx, contextPassiveReader(t, b.entry.journal), established.Owner, PreparationRequest{})
	if err != nil || outcome.Condition != PreparationOK || calls.Load() != 1 {
		t.Fatalf("B preparation did not run once: %v calls=%d err=%v", outcome, calls.Load(), err)
	}
}
