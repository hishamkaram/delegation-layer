package guardian

import (
	"context"
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type lifetimeTerminationRecorderProbe struct {
	ctx   context.Context
	calls int
}

type lifetimeTestContextKey struct{}

func (p *lifetimeTerminationRecorderProbe) RecordLifetimeTerminationContext(ctx context.Context, cause string) (*task.LifetimeTerminationRecord, error) {
	p.ctx = ctx
	p.calls++
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &task.LifetimeTerminationRecord{Cause: cause, ScopeEmpty: true}, nil
}

func TestRecordTaskLifetimeTerminationContextPreservesCallerAndNilGuard(t *testing.T) {
	probe := &lifetimeTerminationRecorderProbe{}
	var nilContext context.Context
	if _, err := recordTaskLifetimeTerminationContext(nilContext, probe, task.LifetimeTerminationRecovery); !errors.Is(err, context.Canceled) {
		t.Fatalf("nil context error = %v, want context canceled", err)
	}
	if probe.calls != 0 {
		t.Fatalf("nil context invoked recorder %d times, want 0", probe.calls)
	}

	marker := &struct{}{}
	ctx := context.WithValue(t.Context(), lifetimeTestContextKey{}, marker)
	if _, err := recordTaskLifetimeTerminationContext(ctx, probe, task.LifetimeTerminationRecovery); err != nil {
		t.Fatalf("record with caller context: %v", err)
	}
	if probe.ctx != ctx {
		t.Fatal("record did not receive the exact caller context")
	}
	if probe.ctx.Value(lifetimeTestContextKey{}) != marker {
		t.Fatal("record lost caller context value")
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := recordTaskLifetimeTerminationContext(canceled, probe, task.LifetimeTerminationRecovery); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v, want context canceled", err)
	}
	if probe.ctx != canceled {
		t.Fatal("canceled caller context was replaced")
	}
}

func TestRecoverLifetimeScopeAcceptsConcurrentCleanupOnlyAfterEmptyRecheck(t *testing.T) {
	scope := &lifetimeGuardianTestScope{
		timeline:     &lifetimeGuardianTestTimeline{},
		emptyResults: []bool{false, true},
		terminateErr: lifetime.ErrUnverifiableLaunch,
	}
	if err := recoverLifetimeScope(t.Context(), scope, nil, task.LifetimeAssuranceExact); err != nil {
		t.Fatalf("recover after concurrent cleanup: %v", err)
	}
	empty, terminate, _ := scope.counts()
	if empty != 2 || terminate != 1 {
		t.Fatalf("scope calls empty=%d terminate=%d, want 2/1", empty, terminate)
	}
}

func TestLifetimeFinalizationRequiresDurableWorkerWait(t *testing.T) {
	if err := requireDurableLifetimeWorkerWait(nil); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("missing worker Wait = %v, want evidence fault", err)
	}
	if err := requireDurableLifetimeWorkerWait(&task.LifetimeWorkerWaitRecord{}); err != nil {
		t.Fatalf("present validated worker Wait was rejected: %v", err)
	}
}

func TestDisposeUnregisteredCooperativeScopeUsesCommitOrder(t *testing.T) {
	scope := &lifetimeGuardianTestScope{}

	if err := disposeUnregisteredLifetimeScope(t.Context(), scope, lifetime.AssuranceCooperative); err != nil {
		t.Fatalf("dispose unregistered cooperative scope: %v", err)
	}
	empty, _, _ := scope.counts()
	destroy, _ := scope.disposalCounts()
	if empty != 0 {
		t.Fatalf("cooperative intent-only cleanup called EmptyContext %d times, want 0", empty)
	}
	if destroy != 1 {
		t.Fatalf("cooperative intent-only cleanup destroyed %d times, want 1", destroy)
	}
}

func TestDisposeUnregisteredExactScopeProvesBeforeDestroy(t *testing.T) {
	scope := &lifetimeGuardianTestScope{emptyResults: []bool{true}}

	if err := disposeUnregisteredLifetimeScope(t.Context(), scope, lifetime.AssuranceExact); err != nil {
		t.Fatalf("dispose unregistered exact scope: %v", err)
	}
	empty, _, _ := scope.counts()
	destroy, _ := scope.disposalCounts()
	if empty != 1 || destroy != 1 {
		t.Fatalf("exact intent-only cleanup calls empty=%d destroy=%d, want 1/1", empty, destroy)
	}
}

func TestCooperativeLifetimeRecoveryPreservesJoinedCleanupErrors(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	scope := &lifetimeGuardianTestScope{
		terminateErr: errors.Join(lifetime.ErrUnverifiableLaunch, cleanupErr),
	}
	launchRef, err := lifetime.NewLaunchRef(1, "token", 1)
	if err != nil {
		t.Fatal(err)
	}
	err = recoverRegisteredLifetime(t.Context(), scope, []lifetime.LaunchRef{launchRef}, task.LifetimeAssuranceCooperative, true, nil)
	if !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("recovery error = %v, want ErrUnverifiableLaunch", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("recovery error = %v, lost cleanup error", err)
	}
	empty, terminate, _ := scope.counts()
	if terminate != 1 || empty != 0 {
		t.Fatalf("cooperative cleanup calls empty=%d terminate=%d, want 0/1", empty, terminate)
	}
}

func TestRecoverLifetimeScopeCooperativeCleanupTerminatesAndPreservesErrors(t *testing.T) {
	cleanupErr := errors.New("terminate failed")
	scope := &lifetimeGuardianTestScope{
		terminateErr: errors.Join(lifetime.ErrUnverifiableLaunch, cleanupErr),
	}
	launchRef, err := lifetime.NewLaunchRef(1, "token", 1)
	if err != nil {
		t.Fatal(err)
	}

	err = recoverLifetimeScope(t.Context(), scope, []lifetime.LaunchRef{launchRef}, task.LifetimeAssuranceCooperative)
	if !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("cooperative recovery error = %v, want ErrUnverifiableLaunch", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("cooperative recovery error = %v, lost terminate error", err)
	}
	empty, terminate, _ := scope.counts()
	if terminate != 1 || empty != 0 {
		t.Fatalf("cooperative cleanup calls empty=%d terminate=%d, want 0/1", empty, terminate)
	}
}
