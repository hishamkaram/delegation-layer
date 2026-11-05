package infrastructure

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func differentOwnerRequirements(requirements ArtifactRequirements, member string) ArtifactRequirements {
	different := task.ComputeSHA256([]byte("different requested artifact"))
	switch member {
	case "runtime":
		requirements.RequiredRuntimeSHA256 = different
	case "client":
		requirements.RequiredClientSHA256 = different
	case "daemon":
		requirements.RequiredDaemonSHA256 = different
	}
	return requirements
}

func TestOwnerBoundRequirementsRefusedBeforeDial(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for _, requirements := range []ArtifactRequirements{{}, {RequiredRuntimeSHA256: ownerWireDigest}} {
		_, err := RequestBoundPreparation(ctx, f.reader, f.auth.request.Owner, PreparationRequest{Requirements: requirements})
		if err == nil {
			t.Fatal("zero/partial Bound requirements accepted")
		}
		if requirements == (ArtifactRequirements{}) && !errors.Is(err, os.ErrInvalid) {
			t.Fatalf("zero requirements lost invalid-input cause: %v", err)
		}
		requireNoOwnerContactConnection(t, f.auth.listener)
	}
}

func TestOwnerFreshRequirementsMismatchNeverReachesHost(t *testing.T) {
	var calls atomic.Int32
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	f.start()
	owner := f.request(t, ownerStatus).Owner
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reader := contextPassiveReader(t, f.entry.journal)
	for _, member := range []string{"runtime", "client", "daemon"} {
		selection := PreparationRequest{Requirements: differentOwnerRequirements(warmOwnerRequirements(f.entry.operation.Artifacts()), member)}
		outcome, err := RequestBoundPreparation(ctx, reader, owner, selection)
		if err != nil || outcome.Condition != PreparationIncompatible || calls.Load() != 0 || f.server.diagnostic() != nil {
			t.Fatalf("%s mismatch reached host/diagnostic: outcome=%v calls=%d err=%v", member, outcome, calls.Load(), err)
		}
	}
}

func TestOwnerReplayRequirementsCheckedBeforeWaitAndSubject(t *testing.T) {
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
	reader := contextPassiveReader(t, f.entry.journal)
	for _, member := range []string{"runtime", "client", "daemon"} {
		selection.Requirements = differentOwnerRequirements(warmOwnerRequirements(f.entry.operation.Artifacts()), member)
		selection.Operation.Source.OwnerID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		outcome, err := RequestBoundPreparation(ctx, reader, owner, selection)
		if err != nil || outcome.Condition != PreparationIncompatible || calls.Load() != 1 {
			t.Fatalf("mismatch waited on initial or disclosed subject: outcome=%v calls=%d err=%v", outcome, calls.Load(), err)
		}
	}
	selection.Requirements = warmOwnerRequirements(f.entry.operation.Artifacts())
	outcome, err := RequestBoundPreparation(ctx, reader, owner, selection)
	if err != nil || outcome.Condition != PreparationRefused {
		t.Fatalf("matching requirements bypassed selector check: %v %v", outcome, err)
	}
	unblock()
	if err = join(); err != nil {
		t.Fatal(err)
	}
	selection.Operation = f.entry.operation.Intent().Inputs.Initial
	outcome, err = RequestBoundPreparation(ctx, reader, owner, selection)
	if err != nil || outcome.Condition != PreparationOK || calls.Load() != 1 {
		t.Fatalf("matching replay failed or repeated host: %v calls=%d err=%v", outcome, calls.Load(), err)
	}
}

func TestOwnerTerminalMismatchDoesNotConsumeInitialFailure(t *testing.T) {
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{recoverSaved: func(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error) {
		return Disposition{}, context.Canceled
	}}, ownerServerFixtureSelection{initial: InitialRecoverSaved})
	if _, err := f.server.prepareInitial(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.start()
	owner := f.request(t, ownerStatus).Owner
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reader := contextPassiveReader(t, f.entry.journal)
	selection := PreparationRequest{Operation: f.entry.operation.Intent().Inputs.Initial, InitialResultFor: owner, Requirements: differentOwnerRequirements(warmOwnerRequirements(f.entry.operation.Artifacts()), "daemon")}
	outcome, err := RequestBoundPreparation(ctx, reader, owner, selection)
	if err != nil || outcome.Condition != PreparationIncompatible {
		t.Fatalf("terminal mismatch did not outrank busy: %v %v", outcome, err)
	}
	select {
	case <-f.server.initialDelivered:
		t.Fatal("incompatibility consumed the initial failure")
	default:
	}
	selection.Requirements = warmOwnerRequirements(f.entry.operation.Artifacts())
	outcome, err = RequestBoundPreparation(ctx, reader, owner, selection)
	if err != nil || outcome.Condition != PreparationFault || outcome.Failure.Causes&PreparationCauseCanceled == 0 {
		t.Fatalf("matching cached failure lost: %v %v", outcome, err)
	}
}

func TestOwnerRequirementsCheckFollowsAuthentication(t *testing.T) {
	var calls atomic.Int32
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	f.start()
	request := f.request(t, ownerEnsureOwn)
	request.Requirements = differentOwnerRequirements(warmOwnerRequirements(f.entry.operation.Artifacts()), "runtime")
	request.Client.PID++
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	if reply := readServerReply(t, connection); reply.Code != ownerRefused || calls.Load() != 0 {
		t.Fatalf("unauthenticated client learned compatibility: %v", reply)
	}
}

func TestOwnerReplyRequirementsEchoIsBound(t *testing.T) {
	f := newOwnerContactFixture(t)
	expected := warmOwnerRequirements(f.auth.intent.Inputs.Artifacts)
	join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
		if request.Requirements != expected {
			return ErrIdentity
		}
		reply := ownerServerReply(request, ownerOK)
		reply.Target = originRecord{Root: f.journal.pin.Root(), RootID: request.RootID}
		reply.Startup = request.Owner
		reply.Disposition = Disposition{Kind: DispositionWarmReuse}
		reply.Requirements = differentOwnerRequirements(expected, "client")
		return writeOwnerFrame(connection, reply)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	outcome, err := RequestBoundPreparation(ctx, f.reader, f.auth.request.Owner, PreparationRequest{Requirements: expected})
	if !errors.Is(err, ErrIdentity) || outcome != (PreparationOutcome{}) {
		t.Fatalf("changed requirements reply accepted: %v %v", outcome, err)
	}
	requireOwnerContactPhase(t, err, ownerContactMayHaveWritten, true)
	if err = join(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerUpgradeRequirementsUseCanonicalSuccessorSelection(t *testing.T) {
	f, body, intent := lifecycleUpgradeFixture(t)
	selected, err := f.journal.readContext(t.Context(), body.SuccessorContext)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := newUpgradeOperation(intent, selected, body)
	if err != nil {
		t.Fatal(err)
	}
	request := ownerRequest{Action: ownerMigrateOwn, Requirements: warmOwnerRequirements(body.SuccessorArtifacts)}
	if err = checkOwnRequirements(request, operation); err != nil {
		t.Fatalf("Upgrade successor artifacts refused: %v", err)
	}
	request.Requirements = differentOwnerRequirements(request.Requirements, "runtime")
	if err = checkOwnRequirements(request, operation); !errors.Is(err, errOwnerRequirementsIncompatible) {
		t.Fatalf("Upgrade mismatch lost canonical selection: %v", err)
	}
}

func TestOwnerEstablishmentDoesNotCompareBRequirementsAgainstA(t *testing.T) {
	var calls atomic.Int32
	b := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	b.start()
	// Accepted component data deliberately differs across A/B; native peers and
	// record ownership remain canonical. This is no managed-image eligibility claim.
	different := b.entry.operation.Artifacts()
	different.RunnerSHA256 = task.ComputeSHA256([]byte("A runtime"))
	different.ClientSHA256 = task.ComputeSHA256([]byte("A client"))
	different.DaemonSHA256 = task.ComputeSHA256([]byte("A daemon"))
	a := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{}, ownerServerFixtureSelection{artifacts: different})
	a.start()
	target, err := NewOrigin(b.root, b.entry.operation.Intent().Inputs.RootID)
	if err != nil {
		t.Fatal(err)
	}
	requirements := warmOwnerRequirements(b.entry.operation.Artifacts())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	established, err := RequestOtherEstablishment(ctx, contextPassiveReader(t, a.entry.journal), a.request(t, ownerStatus).Owner, target, b.entry.operation.defaultContextReference(), requirements, InitialOperation{})
	if err != nil || established.Owner != b.request(t, ownerStatus).Owner || calls.Load() != 0 {
		t.Fatalf("A compared its artifacts to B or prepared B: owner=%v calls=%d err=%v", established, calls.Load(), err)
	}
	outcome, err := RequestBoundPreparation(ctx, contextPassiveReader(t, b.entry.journal), established.Owner, PreparationRequest{Requirements: requirements})
	if err != nil || outcome.Condition != PreparationOK || calls.Load() != 1 {
		t.Fatalf("B bound requirements recheck failed: %v calls=%d err=%v", outcome, calls.Load(), err)
	}
}
