package guardian

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// composedLaunchCount exceeds the former 64-launch namespace.
const composedLaunchCount = 72

// A composed capacity scenario runs under one immutable deadline,
// lifetimeGuardianScenarioDeadline(t, lifetimeGuardianScenarioBudget). Every
// started transition persists through a real durable control store that
// re-verifies the whole launch prefix, so no single RPC has its own budget;
// production worker calls likewise use the immutable task deadline.

// guardianCapacityDeadlineProofBudget is the immutable deadline of the
// blocked-persistence proof. It admits real-store setup on a loaded host and is
// the wall-clock interval that proof waits out.
const guardianCapacityDeadlineProofBudget = 5 * time.Second

// guardianCapacityExpiryJoinBudget bounds the proof's joins after its deadline
// has expired: the released write, the worker side close and the Run join.
const guardianCapacityExpiryJoinBudget = 2 * time.Minute

// The real guardian acknowledges, persists and finalizes more launches than
// the former limit through a durable control store, and the termination binds
// the complete persisted set.
func TestLifetimeGuardianPersistsLaunchesBeyondFormerLimitThroughControlStore(t *testing.T) {
	f, operation, deadline, transport := newGuardianCapacityFixture(t, lifetimeGuardianScenarioBudget, nil)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	startGuardianCapacityWorker(t, ctx, f)
	for sequence := uint64(1); sequence <= composedLaunchCount; sequence++ {
		callGuardianCapacity(t, ctx, f, f.message(lifetimeMessageBegin, sequence, 0))
		callGuardianCapacity(t, ctx, f, f.message(lifetimeMessageStarted, sequence, 1000+int(sequence)))
		callGuardianCapacity(t, ctx, f, f.message(lifetimeMessageFinish, sequence, 0))
	}
	callGuardianCapacity(t, ctx, f, f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := waitGuardianCapacityRun(t, f, deadline); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertGuardianCapacityTermination(t, operation, composedLaunchCount)
	// hello, three transitions per launch, and close.
	assertGuardianCapacityClientDeadlines(t, transport, deadline, 2+3*composedLaunchCount)
}

// Started composes with a real control-store write held at the record port:
// nothing is persisted while the write is held, and after release the accepted
// reply and the durable record agree. Reply-after-record ordering is proven
// deterministically by TestLifetimeGuardianLaunchRecordFailureCleansBeforeRefusal,
// whose failed record write must turn started into a refusal.
func TestLifetimeGuardianStartedCompletesThroughHeldControlStoreWrite(t *testing.T) {
	gate := newGuardianCapacityLaunchGate(1)
	f, operation, deadline, transport := newGuardianCapacityFixture(t, lifetimeGuardianScenarioBudget, gate)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	startGuardianCapacityWorker(t, ctx, f)
	callGuardianCapacity(t, ctx, f, f.message(lifetimeMessageBegin, 1, 0))
	startedDone := make(chan error, 1)
	go func() { startedDone <- guardianCapacityCall(ctx, f, f.message(lifetimeMessageStarted, 1, 1001)) }()
	awaitGuardianCapacityGate(t, ctx, gate)
	assertGuardianCapacityLaunchCount(t, operation, 0)
	gate.open()
	if err := <-startedDone; err != nil {
		t.Fatalf("started sequence 1: %v", err)
	}
	if err := <-gate.result; err != nil {
		t.Fatalf("durable launch write: %v", err)
	}
	launches := assertGuardianCapacityLaunchCount(t, operation, 1)
	if launches[0].Sequence != 1 || launches[0].PID != 1001 {
		t.Fatalf("persisted launch = sequence %d PID %d, want sequence 1 PID 1001", launches[0].Sequence, launches[0].PID)
	}
	callGuardianCapacity(t, ctx, f, f.message(lifetimeMessageFinish, 1, 0))
	callGuardianCapacity(t, ctx, f, f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := waitGuardianCapacityRun(t, f, deadline); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertGuardianCapacityTermination(t, operation, 1)
	// hello, begin, started, finish and close.
	assertGuardianCapacityClientDeadlines(t, transport, deadline, 5)
}

// A started call whose durable write outlasts the immutable deadline is armed
// with that deadline alone and ends there, never earlier; the real store then
// refuses the expired write instead of persisting it.
func TestLifetimeGuardianStartedExpiresAtImmutableDeadlineWhilePersistenceBlocks(t *testing.T) {
	gate := newGuardianCapacityLaunchGate(1)
	f, operation, deadline, transport := newGuardianCapacityFixture(t, guardianCapacityDeadlineProofBudget, gate)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	startGuardianCapacityWorker(t, ctx, f)
	callGuardianCapacity(t, ctx, f, f.message(lifetimeMessageBegin, 1, 0))
	startedDone := make(chan guardianCapacityCallResult, 1)
	go func() {
		err := guardianCapacityCall(ctx, f, f.message(lifetimeMessageStarted, 1, 1001))
		startedDone <- guardianCapacityCallResult{err: err, returned: time.Now()}
	}()
	awaitGuardianCapacityGate(t, ctx, gate)
	joinBy := lifetimeGuardianScenarioDeadline(t, guardianCapacityExpiryJoinBudget)
	var started guardianCapacityCallResult
	select {
	case started = <-startedDone:
	case <-time.After(time.Until(joinBy)):
		t.Fatal("started did not return after its immutable deadline")
	}
	// At the deadline the client's own stream deadline and the guardian's
	// deadline-bound reader, which closes the stream, race; either ends the call.
	if !errors.Is(started.err, os.ErrDeadlineExceeded) && !errors.Is(started.err, io.EOF) {
		t.Fatalf("started while persistence blocks = %v, want deadline exceeded or guardian close", started.err)
	}
	// The armed transport deadlines are the oracle: they are recorded inside
	// each call, so they do not depend on when this goroutine observes it.
	assertGuardianCapacityClientDeadlines(t, transport, deadline, 3)
	if started.returned.Before(deadline) {
		t.Fatalf("started returned at %v, before the immutable deadline %v", started.returned, deadline)
	}
	openGuardianCapacityGateAfterRecordExpiry(t, gate, joinBy)
	select {
	case persistErr := <-gate.result:
		if !errors.Is(persistErr, context.DeadlineExceeded) {
			t.Fatalf("expired durable launch write = %v, want deadline exceeded", persistErr)
		}
	case <-time.After(time.Until(joinBy)):
		t.Fatal("released durable launch write did not return")
	}
	// The timed-out protocol stream is not reused; the worker side closes it.
	if err := f.client.Close(); err != nil {
		t.Fatalf("close timed-out client: %v", err)
	}
	f.worker.release()
	if err := waitGuardianCapacityRun(t, f, joinBy); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run after the started deadline = %v, want the expired launch write", err)
	}
	termination := assertGuardianCapacityTermination(t, operation, 0)
	if termination.Cause != task.LifetimeTerminationRecovery || !termination.ScopeEmpty {
		t.Fatalf("termination = cause %q scope empty %t, want recovery with an empty scope", termination.Cause, termination.ScopeEmpty)
	}
}

// guardianCapacityLaunchGate holds one real durable launch write at the
// guardian's record port. Every other record operation, serialization and the
// create-once write itself stay with the real control lifetime.
type guardianCapacityLaunchGate struct {
	*taskdir.ControlLifetime
	sequence uint64
	// recordCtx is the held write's own context. It is set before entered
	// closes, so readers that received from entered observe it.
	recordCtx   context.Context
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	result      chan error
}

func newGuardianCapacityLaunchGate(sequence uint64) *guardianCapacityLaunchGate {
	return &guardianCapacityLaunchGate{
		sequence: sequence,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		result:   make(chan error, 1),
	}
}

func (g *guardianCapacityLaunchGate) RecordLifetimeLaunchContext(ctx context.Context, facts taskdir.LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error) {
	if facts.Sequence != g.sequence {
		return g.ControlLifetime.RecordLifetimeLaunchContext(ctx, facts)
	}
	g.recordCtx = ctx
	close(g.entered)
	<-g.release
	record, err := g.ControlLifetime.RecordLifetimeLaunchContext(ctx, facts)
	g.result <- err
	return record, err
}

func (g *guardianCapacityLaunchGate) open() {
	g.releaseOnce.Do(func() { close(g.release) })
}

// openGuardianCapacityGateAfterRecordExpiry releases the held write only after
// its own context has expired. The client's transport deadline and the
// guardian's work context run separate timers, so a timed-out client call does
// not prove the store already observes the expiry.
func openGuardianCapacityGateAfterRecordExpiry(t *testing.T, gate *guardianCapacityLaunchGate, joinBy time.Time) {
	t.Helper()
	select {
	case <-gate.recordCtx.Done():
	case <-time.After(time.Until(joinBy)):
		t.Fatal("held durable launch write context did not expire")
	}
	if err := gate.recordCtx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("held durable launch write context = %v, want deadline exceeded", err)
	}
	gate.open()
}

// newGuardianCapacityFixture runs the guardian on the real clock so one
// immutable deadline, computed before the fixture exists and passed to the
// fixture and to Options.Deadline alike, governs the guardian, the durable
// store and every client call.
func newGuardianCapacityFixture(
	t *testing.T,
	budget time.Duration,
	gate *guardianCapacityLaunchGate,
) (*lifetimeGuardianTestFixture, *taskdir.ControlLifetime, time.Time, *lifetimeWireDeadlineHistoryConn) {
	t.Helper()
	var operation *taskdir.ControlLifetime
	deadline := lifetimeGuardianScenarioDeadline(t, budget)
	f := newLifetimeGuardianTestFixtureWithOptionsAndContext(t, t.Context(), deadline, nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative, nil, nil,
		func(options *Options) {
			options.Deadline = deadline
			options.Now = time.Now
			// Cleanup after an expired deadline writes through the real store, so it
			// keeps the production cleanup bound rather than the fake-store second.
			options.CleanupTimeout = defaultLifetimeGuardianCleanupTimeout
			operation = newGuardianCapacityControlLifetime(t, options)
			if gate != nil {
				gate.ControlLifetime = operation
				options.Records = gate
			}
		})
	if gate != nil {
		// Registered after the fixture so it runs first and the guardian can
		// always leave a held write before the fixture joins Run.
		t.Cleanup(gate.open)
	}
	// Before any call, the client codec is rebuilt over the same pipe so every
	// deadline it arms is recorded. The fixture cleanup closes this codec.
	transport := &lifetimeWireDeadlineHistoryConn{Conn: f.clientConn}
	client, err := newLifetimeCodec(transport)
	if err != nil {
		t.Fatal(err)
	}
	f.client = client
	return f, operation, deadline, transport
}

func newGuardianCapacityControlLifetime(t *testing.T, options *Options) *taskdir.ControlLifetime {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})
	control, err := store.OpenWorkspaceOperation(t.Context(), options.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Errorf("close control: %v", closeErr)
		}
	})
	operation, err := taskdir.NewControlLifetime(control, store.RootID, options.TaskID, task.LifetimeOwnerWorkspace,
		task.ComputeSHA256([]byte("guardian-capacity-owner")), options.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	options.RootID = store.RootID
	options.OwnerKind = task.LifetimeOwnerWorkspace
	options.Records = operation
	options.AcquireOwner = func(ctx context.Context) (Owner, error) {
		return operation.AcquireLifetimeOwner(ctx)
	}
	return operation
}

// guardianCapacityCall is the scenario's only client call; it is bounded by
// the scenario's immutable deadline, never by a shorter per-RPC budget.
func guardianCapacityCall(ctx context.Context, f *lifetimeGuardianTestFixture, message lifetimeMessage) error {
	return f.client.callContext(ctx, message)
}

type guardianCapacityCallResult struct {
	err      error
	returned time.Time
}

// assertGuardianCapacityClientDeadlines requires that each client call armed
// the transport exactly once, with the scenario's immutable deadline.
func assertGuardianCapacityClientDeadlines(t *testing.T, transport *lifetimeWireDeadlineHistoryConn, deadline time.Time, calls int) {
	t.Helper()
	armed := transport.snapshot()
	if len(armed) != calls {
		t.Fatalf("client armed %d transport deadlines, want %d", len(armed), calls)
	}
	for index, got := range armed {
		if !got.Equal(deadline) {
			t.Fatalf("client call %d armed deadline %v, want immutable deadline %v", index+1, got, deadline)
		}
	}
}

func callGuardianCapacity(t *testing.T, ctx context.Context, f *lifetimeGuardianTestFixture, message lifetimeMessage) {
	t.Helper()
	if err := guardianCapacityCall(ctx, f, message); err != nil {
		t.Fatalf("%s sequence %d: %v", message.Type, message.Sequence, err)
	}
}

func startGuardianCapacityWorker(t *testing.T, ctx context.Context, f *lifetimeGuardianTestFixture) {
	t.Helper()
	callGuardianCapacity(t, ctx, f, f.message(lifetimeMessageHello, 0, 0))
	select {
	case <-f.worker.waitEntered:
	case <-ctx.Done():
		t.Fatalf("worker wait was not entered: %v", ctx.Err())
	}
}

func awaitGuardianCapacityGate(t *testing.T, ctx context.Context, gate *guardianCapacityLaunchGate) {
	t.Helper()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatalf("started sequence %d did not reach the durable launch write: %v", gate.sequence, ctx.Err())
	}
}

// waitGuardianCapacityRun joins Run within the scenario's finite bound; the
// generic fixture cleanup still owns the final join on failure.
func waitGuardianCapacityRun(t *testing.T, f *lifetimeGuardianTestFixture, until time.Time) error {
	t.Helper()
	select {
	case err := <-f.runDone:
		return err
	case <-time.After(time.Until(until)):
		t.Fatalf("guardian did not finish by %v", until)
		return nil
	}
}

func assertGuardianCapacityLaunchCount(t *testing.T, operation *taskdir.ControlLifetime, want int) []task.LifetimeLaunchRecord {
	t.Helper()
	launches, err := operation.ReadLifetimeLaunchesContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(launches) != want {
		t.Fatalf("persisted launches = %d, want %d", len(launches), want)
	}
	return launches
}

func assertGuardianCapacityTermination(t *testing.T, operation *taskdir.ControlLifetime, want int) *task.LifetimeTerminationRecord {
	t.Helper()
	launches := assertGuardianCapacityLaunchCount(t, operation, want)
	termination, err := operation.ReadLifetimeTerminationContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := task.LifetimeLaunchSetDigest(launches)
	if err != nil {
		t.Fatal(err)
	}
	if termination.LaunchCount != uint64(want) || termination.LaunchSetSHA256 != digest {
		t.Fatalf("termination binds %d launches %s, want %d %s", termination.LaunchCount, termination.LaunchSetSHA256, want, digest)
	}
	return termination
}
