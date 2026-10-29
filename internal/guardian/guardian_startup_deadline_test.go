package guardian

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	// startupDeadlineStoreFallback mirrors taskdir's bound for a persistence
	// call whose context carries no deadline.
	startupDeadlineStoreFallback = 5 * time.Second
	// startupDeadlineControlHold keeps a competing control transaction past
	// that fallback while staying far inside the scenario's business deadline.
	startupDeadlineControlHold = startupDeadlineStoreFallback + time.Second
	// startupDeadlineGuard ends a held registration write on the fake clock
	// when its context never expires, so a missing deadline fails an assertion
	// instead of hanging the bubble.
	startupDeadlineGuard = time.Hour
)

var (
	errStartupDeadlineNotApplied = errors.New("startup registration context never reached its deadline")
	errStartupCompetitorNotHeld  = errors.New("competing control transaction ended before holding its lock")
)

// A background Run still persists intent, selects placement and registers
// under the immutable business deadline.
func TestLifetimeGuardianStartupUsesImmutableDeadlineUnderBackgroundRun(t *testing.T) {
	var selectedDeadline time.Time
	var selectedBounded bool
	f := newLifetimeGuardianTestFixtureWithOptionsAndContext(
		t, context.Background(), lifetimeGuardianScenarioDeadline(t, lifetimeGuardianScenarioBudget),
		nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative, nil, nil,
		func(options *Options) {
			options.SelectPlacement = func(ctx context.Context) (*task.LifetimeCustodyPlacement, error) {
				selectedDeadline, selectedBounded = ctx.Deadline()
				return lifetimeGuardianTestPlacement("a"), nil
			}
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	deadline := f.guardian.options.Deadline
	intentContext, registrationContext, _, _ := f.records.boundaryWriteContexts()
	requireStartupContextDeadline(t, "intent", intentContext, deadline)
	requireStartupContextDeadline(t, "registration", registrationContext, deadline)
	if !selectedBounded || !selectedDeadline.Equal(deadline) {
		t.Fatalf("placement selection deadline = %v bounded=%t, want %v", selectedDeadline, selectedBounded, deadline)
	}
}

// An earlier caller deadline still bounds startup persistence.
func TestLifetimeGuardianStartupKeepsEarlierCallerDeadline(t *testing.T) {
	earlier := time.Now().Add(30 * time.Minute)
	runContext, cancel := context.WithDeadline(t.Context(), earlier)
	defer cancel()
	f := newLifetimeGuardianTestFixtureWithOptionsAndContext(
		t, runContext, lifetimeGuardianScenarioDeadline(t, lifetimeGuardianScenarioBudget),
		nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative, nil, nil, nil,
	)
	if !earlier.Before(f.guardian.options.Deadline) {
		t.Fatalf("caller deadline %v is not earlier than business deadline %v", earlier, f.guardian.options.Deadline)
	}
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	intentContext, registrationContext, _, _ := f.records.boundaryWriteContexts()
	requireStartupContextDeadline(t, "intent", intentContext, earlier)
	requireStartupContextDeadline(t, "registration", registrationContext, earlier)
}

// The business deadline expires while registration is held. No worker starts,
// and the unregistered scope is disposed under an alive, independent recovery
// context before the owner is released.
func TestLifetimeGuardianStartupDeadlineExpiresDuringRegistration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(time.Minute)
		var gate *startupDeadlineRegistrationGate
		f := newLifetimeGuardianTestFixtureWithOptionsAndContext(
			t, context.Background(), deadline.Add(2*startupDeadlineGuard),
			nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative, nil, nil,
			func(options *Options) {
				options.Deadline = deadline
				options.Now = time.Now
				gate = &startupDeadlineRegistrationGate{lifetimeGuardianTestRecords: lifetimeGuardianTestRecordsFromOptions(t, options)}
				options.Records = gate
			},
		)
		runErr := f.waitRun(t)
		if !errors.Is(runErr, context.DeadlineExceeded) {
			t.Fatalf("run = %v, want the expired startup deadline", runErr)
		}
		gate.requireExpiredWrite(t, deadline)
		gate.requireAliveRecoveryRead(t, deadline)
		requireStartupExpiryDisposal(t, f)
	})
}

// A competing production control transaction holds the operation lock past the
// store's no-deadline fallback; the intent write waits under the business
// deadline and the run completes with its normal records and closure.
func TestLifetimeGuardianStartupIntentWaitsPastStoreFallback(t *testing.T) {
	deadline := lifetimeGuardianScenarioDeadline(t, lifetimeGuardianScenarioBudget)
	var gate *startupDeadlineIntentGate
	f := newLifetimeGuardianTestFixtureWithOptionsAndContext(
		t, t.Context(), deadline, nil, lifetime.KindProcessGroup, lifetime.AssuranceCooperative, nil, nil,
		func(options *Options) {
			options.Deadline = deadline
			options.Now = time.Now
			options.CleanupTimeout = defaultLifetimeGuardianCleanupTimeout
			gate = newStartupDeadlineIntentGate(t, options)
			options.Records = gate
		},
	)
	gate.requireIntentWaitedPastFallback(t, deadline)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("run after held control transaction: %v", err)
	}
	if err := <-gate.holderDone; err != nil {
		t.Fatalf("competing control transaction: %v", err)
	}
	assertGuardianCapacityTermination(t, gate.ControlLifetime, 0)
}

func requireStartupContextDeadline(t *testing.T, name string, ctx context.Context, want time.Time) {
	t.Helper()
	if ctx == nil {
		t.Fatalf("%s record received no context", name)
	}
	if got, bounded := ctx.Deadline(); !bounded || !got.Equal(want) {
		t.Fatalf("%s context deadline = %v bounded=%t, want %v", name, got, bounded, want)
	}
}

func requireStartupExpiryDisposal(t *testing.T, f *lifetimeGuardianTestFixture) {
	t.Helper()
	if start, _ := f.worker.counts(); start != 0 {
		t.Fatalf("worker starts after expired startup = %d, want 0", start)
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("unregistered scope disposal = destroy %d release %d, want 1/1", destroy, release)
	}
	events := f.timeline.snapshot()
	for _, forbidden := range []string{"registration", "worker-start"} {
		if timelineIndex(events, forbidden) >= 0 {
			t.Fatalf("expired startup recorded %q: %v", forbidden, events)
		}
	}
	assertTimelineOrder(t, events, "acquire", "intent", "scope", "scope-bind-owner", "scope-destroy", "scope-release", "release")
}

// startupDeadlineRegistrationGate holds the registration write until its
// context ends and records every registration read's context state.
type startupDeadlineRegistrationGate struct {
	*lifetimeGuardianTestRecords

	mu          sync.Mutex
	writeErr    error
	writeEnded  time.Time
	readStates  []startupDeadlineReadState
	writeCalled bool
}

type startupDeadlineReadState struct {
	err      error
	deadline time.Time
	bounded  bool
}

func (g *startupDeadlineRegistrationGate) RecordLifetimeRegistrationContext(ctx context.Context, facts taskdir.LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error) {
	var record *task.LifetimeRegistrationRecord
	var err error
	select {
	case <-ctx.Done():
		record, err = g.lifetimeGuardianTestRecords.RecordLifetimeRegistrationContext(ctx, facts)
	case <-time.After(startupDeadlineGuard):
		err = errStartupDeadlineNotApplied
	}
	g.mu.Lock()
	g.writeCalled, g.writeErr, g.writeEnded = true, err, time.Now()
	g.mu.Unlock()
	return record, err
}

func (g *startupDeadlineRegistrationGate) ReadLifetimeRegistrationContext(ctx context.Context) (*task.LifetimeRegistrationRecord, error) {
	deadline, bounded := ctx.Deadline()
	g.mu.Lock()
	g.readStates = append(g.readStates, startupDeadlineReadState{err: ctx.Err(), deadline: deadline, bounded: bounded})
	g.mu.Unlock()
	return g.lifetimeGuardianTestRecords.ReadLifetimeRegistrationContext(ctx)
}

func (g *startupDeadlineRegistrationGate) requireExpiredWrite(t *testing.T, deadline time.Time) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.writeCalled || !errors.Is(g.writeErr, context.DeadlineExceeded) || g.writeEnded.Before(deadline) {
		t.Fatalf("registration write = called %t err %v at %v, want deadline exceeded at or after %v", g.writeCalled, g.writeErr, g.writeEnded, deadline)
	}
}

// requireAliveRecoveryRead requires the post-failure registration read to run
// under a live context whose bound is independent of the expired deadline.
func (g *startupDeadlineRegistrationGate) requireAliveRecoveryRead(t *testing.T, deadline time.Time) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.readStates) < 2 {
		t.Fatalf("registration reads = %d, want placement read and recovery read", len(g.readStates))
	}
	recovery := g.readStates[len(g.readStates)-1]
	if recovery.err != nil || !recovery.bounded || !recovery.deadline.After(deadline) {
		t.Fatalf("recovery read context = err %v deadline %v bounded %t, want live and bounded after %v", recovery.err, recovery.deadline, recovery.bounded, deadline)
	}
}

// startupDeadlineIntentGate starts one competing production control
// transaction when the guardian's intent write arrives. The intent write then
// runs unchanged through the real control lifetime and waits on its lock.
type startupDeadlineIntentGate struct {
	*taskdir.ControlLifetime
	competitor *taskdir.ControlDir
	holderDone chan error
	// intentDone closes after intentErr and intentWait are set.
	intentDone chan struct{}
	intentErr  error
	intentWait time.Duration
}

// requireIntentWaitedPastFallback waits for the held intent write before any
// client call, so a store fallback expiry fails here rather than in the
// protocol.
func (g *startupDeadlineIntentGate) requireIntentWaitedPastFallback(t *testing.T, deadline time.Time) {
	t.Helper()
	select {
	case <-g.intentDone:
	case <-time.After(time.Until(deadline)):
		t.Fatal("held intent write did not return before the business deadline")
	}
	if g.intentErr != nil || g.intentWait <= startupDeadlineStoreFallback {
		t.Fatalf("intent write = %v after %v, want success after more than %v", g.intentErr, g.intentWait, startupDeadlineStoreFallback)
	}
}

func newStartupDeadlineIntentGate(t *testing.T, options *Options) *startupDeadlineIntentGate {
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
	control := openStartupDeadlineControl(t, store, options.TaskID, true)
	operation, err := taskdir.NewControlLifetime(control, store.RootID, options.TaskID, task.LifetimeOwnerWorkspace,
		task.ComputeSHA256([]byte("guardian-startup-deadline-owner")), options.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	options.RootID = store.RootID
	options.OwnerKind = task.LifetimeOwnerWorkspace
	options.AcquireOwner = func(ctx context.Context) (Owner, error) {
		return operation.AcquireLifetimeOwner(ctx)
	}
	return &startupDeadlineIntentGate{
		ControlLifetime: operation,
		competitor:      openStartupDeadlineControl(t, store, options.TaskID, false),
		holderDone:      make(chan error, 1),
		intentDone:      make(chan struct{}),
	}
}

func openStartupDeadlineControl(t *testing.T, store *taskdir.Store, taskID string, create bool) *taskdir.ControlDir {
	t.Helper()
	control, err := store.OpenWorkspaceOperation(t.Context(), taskID, create)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Errorf("close control: %v", closeErr)
		}
	})
	return control
}

func (g *startupDeadlineIntentGate) RecordLifetimeIntentContext(ctx context.Context, facts taskdir.LifetimeIntentFacts) (*task.LifetimeIntentRecord, error) {
	acquired := make(chan struct{})
	go func() {
		g.holderDone <- g.competitor.WithLock(context.WithoutCancel(ctx), func(*taskdir.ControlTransaction) error {
			close(acquired)
			<-time.After(startupDeadlineControlHold)
			return nil
		})
	}()
	select {
	case <-acquired:
	case holderErr := <-g.holderDone:
		g.holderDone <- holderErr
		return nil, errors.Join(errStartupCompetitorNotHeld, holderErr)
	}
	started := time.Now()
	record, err := g.ControlLifetime.RecordLifetimeIntentContext(ctx, facts)
	g.intentErr, g.intentWait = err, time.Since(started)
	close(g.intentDone)
	return record, err
}
