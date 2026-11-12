package infrastructure

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// This component test protects the server's serving/initial-result boundary.
// Controlled completion handles here are not native Start/Wait evidence.
func TestRuntimeRestartInitialPendingKeepsExactReconciliationCallable(t *testing.T) {
	var calls, ordinary atomic.Int32
	pending := errors.New("fixture pending restart")
	done := make(chan struct{})
	host := &ownerServerTestHost{
		restart: func(_ context.Context, operation Operation, _ LaunchContext, from StartupReference, _ ResidentGuards, waits DaemonWaitOwner) (Disposition, RuntimeRestartReport, error) {
			current, err := ReferenceStartup(operation.Intent())
			if err != nil {
				return Disposition{}, RuntimeRestartReport{}, err
			}
			if calls.Add(1) == 1 {
				if err = waits.RegisterDaemonWait(operation.Intent().Inputs.Root, 101, done); err != nil {
					return Disposition{}, RuntimeRestartReport{}, err
				}
				return Disposition{}, RuntimeRestartReport{From: from, Phase: RuntimeRestartSuccessorAccepted, Successor: current}, errors.Join(pending, context.DeadlineExceeded)
			}
			return Disposition{Kind: DispositionWarmReuse}, RuntimeRestartReport{From: from, Phase: RuntimeRestartReady, Successor: current}, nil
		},
		ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
			ordinary.Add(1)
			return Disposition{Kind: DispositionWarmReuse}, nil
		},
		classify: func(err error) PreparationFailure {
			if err != nil {
				return PreparationFailure{Causes: PreparationCauseRuntimeRestartPending | PreparationCauseDeadline}
			}
			return PreparationFailure{}
		},
	}
	f := newOwnerServerFixtureWithSelection(t, host, ownerServerFixtureSelection{initial: InitialRuntimeRestart})
	cancel, _, join := startRuntimeFixture(t, f)
	t.Cleanup(func() { close(done); cancel(); join() })
	select {
	case <-f.server.initialDone:
	case <-time.After(5 * time.Second):
		t.Fatal("initial pending result did not settle")
	}
	assertRuntimeRestartReconciliation(t, f, &calls, &ordinary)
}

func assertRuntimeRestartReconciliation(t *testing.T, f *ownerServerFixture, calls, ordinary *atomic.Int32) {
	t.Helper()
	initial := f.server.initialOutcome
	if !f.server.keepsRuntimeReconciliation() || initial.RuntimeRestart.Phase != RuntimeRestartSuccessorAccepted || calls.Load() != 1 {
		t.Fatalf("initial expiry retired the matching owner: initial=%+v calls=%d", initial, calls.Load())
	}
	assertRuntimeRestartOrdinaryDenied(t, f, ordinary)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	owner := f.request(t, ownerStatus).Owner
	reader := contextPassiveReader(t, f.entry.journal)
	prepared, err := RequestBoundPreparation(ctx, reader, owner, PreparationRequest{Operation: f.entry.operation.Intent().Inputs.Initial})
	if err != nil || prepared.RuntimeRestart.Phase != RuntimeRestartReady || prepared.Disposition.Kind != DispositionWarmReuse || calls.Load() != 2 {
		t.Fatalf("same owner could not reconcile without replaying initial work: %+v calls=%d err=%v", prepared, calls.Load(), err)
	}
	if f.server.initialOutcome != initial || f.server.keepsRuntimeReconciliation() {
		t.Fatal("fresh reconciliation rewrote immutable initial result or retained ready gate")
	}
}

func assertRuntimeRestartOrdinaryDenied(t *testing.T, f *ownerServerFixture, ordinary *atomic.Int32) {
	t.Helper()
	if stop := f.server.commitOwnerStop(); stop != ownerStopBusy {
		t.Fatalf("pending live Wait custody was abandoned: %s", stop)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	owner := f.request(t, ownerStatus).Owner
	reader := contextPassiveReader(t, f.entry.journal)
	if observed, err := ProbeOwner(ctx, reader); err != nil || observed != owner {
		t.Fatalf("matching resident stopped serving after initial expiry: %v", err)
	}
	blocked, err := RequestBoundPreparation(ctx, reader, owner, PreparationRequest{})
	if err != nil || blocked.Condition != PreparationBusy || ordinary.Load() != 0 {
		t.Fatalf("ordinary mutation crossed the restart gate: %+v calls=%d err=%v", blocked, ordinary.Load(), err)
	}
}

func TestRuntimeRestartJoinedFaultDoesNotRetainRetryAuthority(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	from := f.request(t, ownerStatus).Owner
	accepted := PreparationOutcome{Condition: PreparationPending, RuntimeRestart: RuntimeRestartReport{From: from, Phase: RuntimeRestartDaemonClosed}}
	f.server.observeRuntimePreparation(from, accepted, nil)
	if !f.server.keepsRuntimeReconciliation() {
		t.Fatal("valid accepted observation did not gate admission")
	}
	accepted.Failure = PreparationFailure{Causes: PreparationCauseRuntimeRestartPending, IndependentFault: true}
	f.server.observeRuntimePreparation(from, accepted, errors.Join(context.DeadlineExceeded, os.ErrPermission))
	if f.server.keepsRuntimeReconciliation() || !f.server.isClosing() {
		t.Fatal("joined storage fault was hidden by the pending report")
	}
}

func TestRuntimeRestartDaemonClosedWithoutActiveWaitCanRetire(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	from := f.request(t, ownerStatus).Owner
	f.server.observeRuntimePreparation(from, PreparationOutcome{Condition: PreparationOK, RuntimeRestart: RuntimeRestartReport{From: from, Phase: RuntimeRestartDaemonClosed}}, nil)
	if stop := f.server.commitOwnerStop(); stop != ownerStopAccepted {
		t.Fatalf("closed predecessor without active Wait could not release serving: %s", stop)
	}
}
