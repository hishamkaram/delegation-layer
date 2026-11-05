package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestWarmOwnerOtherRootUsesRequestContextWithoutNewBirth(t *testing.T) {
	var calls atomic.Int32
	var expected LaunchContext
	var expectedBirth LaunchContextReference
	var expectedDeadline time.Time
	b := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(ctx context.Context, op Operation, selected LaunchContext, _ DaemonStartGuard, _ DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		if !slices.Equal(selected.ControlEnvironment, expected.ControlEnvironment) || selected.Root != expected.Root || selected.RootID != expected.RootID {
			t.Error("B did not receive C2 from this request")
		}
		if deadline, finite := ctx.Deadline(); !finite || !deadline.Equal(expectedDeadline) {
			t.Error("B contact replaced the authenticated deadline")
		}
		if op.Intent().Inputs.Context != expectedBirth {
			t.Error("B rewrote its immutable C1 birth selection")
		}
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	expectedBirth = b.entry.operation.defaultContextReference()
	expected = b.entry.operation.context.Clone()
	expected.ControlEnvironment = []string{"WARM_OWNER_CONTEXT=C2"}
	selected, _, err := b.entry.journal.stageContext(t.Context(), expected)
	if err != nil {
		t.Fatal(err)
	}
	before := producerBudgetTree(t, filepath.Join(b.root, "infrastructure"))
	b.start()
	a := newOwnerServerFixture(t, &ownerServerTestHost{})
	a.start()
	target, err := NewOrigin(b.root, expected.RootID)
	if err != nil {
		t.Fatal(err)
	}
	expectedDeadline = time.Now().Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(t.Context(), expectedDeadline)
	defer cancel()
	// The real public client validates A's complete wire reply, including the
	// nonzero exact B startup, through validateOwnerReplyBinding.
	established, err := RequestOtherEstablishment(ctx, contextPassiveReader(t, a.entry.journal), a.request(t, ownerStatus).Owner, target, selected, warmOwnerRequirements(b.entry.operation.Artifacts()), InitialOperation{})
	if err != nil {
		t.Fatal(err)
	}
	assertEstablishedOnly(t, established, b.request(t, ownerStatus).Owner, calls.Load())
	outcome, err := RequestBoundPreparation(ctx, contextPassiveReader(t, b.entry.journal), established.Owner, PreparationRequest{Requirements: warmOwnerRequirements(b.entry.operation.Artifacts()), Context: selected})
	reference, disposition := outcome.Startup, outcome.Disposition
	if err != nil || reference != b.request(t, ownerStatus).Owner || disposition.Kind != DispositionWarmReuse || calls.Load() != 1 {
		t.Fatalf("warm B did not prepare C2: calls=%d reference=%v disposition=%v err=%v", calls.Load(), reference, disposition, err)
	}
	if !maps.Equal(before, producerBudgetTree(t, filepath.Join(b.root, "infrastructure"))) {
		t.Fatal("warm contact changed B's stored birth/context evidence or created another startup")
	}
}

func warmOwnerRequirements(artifacts Artifacts) ArtifactRequirements {
	return ArtifactRequirements{RequiredRuntimeSHA256: artifacts.RunnerSHA256, RequiredClientSHA256: artifacts.ClientSHA256, RequiredDaemonSHA256: artifacts.DaemonSHA256}
}

func TestWarmOwnerOtherRootPendingContextsPreserveBirth(t *testing.T) {
	for _, assumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "assumed-unpublished"}[assumed], func(t *testing.T) {
			exerciseWarmOwnerPendingContextReplies(t, assumed)
		})
	}
}

func exerciseWarmOwnerPendingContextReplies(t *testing.T, assumed bool) {
	t.Helper()
	b := producerFixture(t)
	intent := seedProducerIntent(t, b, 1, false)
	if assumed {
		assumption, _ := evidenceForIntent(t, intent)
		if created, err := b.journal.putAssumption(t.Context(), b.lease, assumption); err != nil || !created {
			t.Fatalf("unpublished B assumption: %t %v", created, err)
		}
	}
	selected := b.request.Context.Clone()
	selected.ControlEnvironment = []string{"WARM_OWNER_CONTEXT=C2"}
	c2, _, err := b.journal.stageContext(t.Context(), selected)
	if err != nil {
		t.Fatal(err)
	}
	before := producerBudgetTree(t, filepath.Join(b.journal.pin.Root(), "infrastructure"))
	a := newOwnerServerFixture(t, &ownerServerTestHost{})
	writes := make(chan ownerReply, 2)
	a.server.writeReply = func(connection *net.UnixConn, reply ownerReply) error {
		a.server.mu.Lock()
		admitted := a.server.connectionCount(ownerConnectionOperation)
		a.server.mu.Unlock()
		if admitted != 1 {
			t.Errorf("A released capacity before completing B observation: %d", admitted)
		}
		writes <- reply
		return writeOwnerFrame(connection, reply)
	}
	a.start()
	for _, test := range []struct {
		selected LaunchContextReference
		code     ownerReplyCode
	}{{intent.Inputs.Context, ownerTimeout}, {c2, ownerBusy}} {
		request := a.request(t, ownerEnsureOther)
		request.Target = originRecord{Root: intent.Inputs.Root, RootID: intent.Inputs.RootID}
		request.Context, request.Requirements = test.selected, b.request.Requirements
		request.Deadline = time.Now().Add(150 * time.Millisecond).UTC().Format(time.RFC3339Nano)
		assertWarmPendingContextReply(t, a, request, test.code, writes)
	}
	if !maps.Equal(before, producerBudgetTree(t, filepath.Join(b.journal.pin.Root(), "infrastructure"))) {
		t.Fatal("pending context requests renewed B's deadline or changed its startup/context evidence")
	}
}

func assertEstablishedOnly(t *testing.T, established EstablishedOwner, expected StartupReference, calls int32) {
	t.Helper()
	if established.Owner != expected || established.InitialResultFor != (StartupReference{}) || calls != 0 {
		t.Fatalf("A did not return established-only B: owner=%v calls=%d", established, calls)
	}
}

func assertWarmPendingContextReply(t *testing.T, a *ownerServerFixture, request ownerRequest, expected ownerReplyCode, writes <-chan ownerReply) {
	t.Helper()

	connection := a.dial(t)
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	reply, readErr := readOwnerFrame[ownerReply](bufio.NewReader(connection))
	if expected == ownerTimeout {
		if readErr != nil && !errors.Is(readErr, io.EOF) || readErr == nil && reply.Code != ownerTimeout {
			t.Fatalf("pending context did not wait for original deadline: %s", reply.Code)
		}
	} else if readErr != nil || reply.Code != expected || reply.Disposition != (Disposition{}) {
		t.Fatalf("pending context reply=%s want=%s disposition=%v err=%v", reply.Code, expected, reply.Disposition, readErr)
	}
	select {
	case written := <-writes:
		if written.Code != expected {
			t.Fatalf("A did not retain same-attempt wait until deadline: %s want %s", written.Code, expected)
		}
	case <-time.After(time.Second):
		t.Fatal("A did not finish bounded observation")
	}
}
