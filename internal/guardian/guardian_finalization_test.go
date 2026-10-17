package guardian

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestLifetimeGuardianProofBatchResidesBetweenBatches(t *testing.T) {
	f := newLifetimeGuardianProofBatchFixture(t)
	result := make(chan error, 1)
	go func() { result <- f.guardian.persistTermination(t.Context()) }()
	select {
	case err := <-result:
		t.Fatalf("proof retry escaped before the next batch: %v", err)
	case <-f.backoffEntered:
	}
	empty, terminate, _ := f.scope.counts()
	if empty != 6 || terminate != 3 {
		t.Fatalf("first proof batch calls empty=%d terminate=%d, want 6/3", empty, terminate)
	}
	select {
	case err := <-result:
		t.Fatalf("proof retry busy-looped while backoff was held: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if delays := f.timeline.backoffSnapshot(); len(delays) != 1 || delays[0] != 100*time.Millisecond {
		t.Fatalf("backoff delays = %v, want [100ms]", delays)
	}
	close(f.backoffRelease)
	if err := <-result; err != nil {
		t.Fatalf("proof retry after backoff: %v", err)
	}
	empty, terminate, _ = f.scope.counts()
	if empty != 7 || terminate != 3 {
		t.Fatalf("next proof batch calls empty=%d terminate=%d, want 7/3", empty, terminate)
	}
}

type lifetimeGuardianProofBatchFixture struct {
	guardian       *Guardian
	scope          *lifetimeGuardianTestScope
	timeline       *lifetimeGuardianTestTimeline
	backoffEntered chan struct{}
	backoffRelease chan struct{}
}

func newLifetimeGuardianProofBatchFixture(t *testing.T) *lifetimeGuardianProofBatchFixture {
	t.Helper()
	timeline := &lifetimeGuardianTestTimeline{}
	records := &lifetimeGuardianTestRecords{timeline: timeline, ownerKind: task.LifetimeOwnerTask}
	scope := &lifetimeGuardianTestScope{
		timeline:     timeline,
		emptyResults: []bool{false, false, false, false, false, false, true},
	}
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		if err := serverConn.Close(); err != nil {
			t.Error(err)
		}
		if err := clientConn.Close(); err != nil {
			t.Error(err)
		}
	})
	backoffEntered := make(chan struct{})
	backoffRelease := make(chan struct{})
	var backoffOnce sync.Once
	guardian, err := New(Options{
		RootID:    lifetimeGuardianTestRootID,
		TaskID:    lifetimeGuardianTestTaskID,
		OwnerKind: task.LifetimeOwnerTask,
		Deadline:  time.Now().Add(time.Hour),
		Records:   records,
		AcquireOwner: func(context.Context) (Owner, error) {
			return &lifetimeGuardianTestOwner{timeline: timeline}, nil
		},
		ReconcileOwned: func(context.Context) error { return nil },
		Worker:         exec.Command("unused-lifetime-worker"),
		Connection:     serverConn,
		ScopeFactory: func(lifetime.Plan) (Scope, error) {
			return scope, nil
		},
		FinalizationBackoff: func(ctx context.Context, delay time.Duration) error {
			timeline.addBackoff(delay)
			backoffOnce.Do(func() { close(backoffEntered) })
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-backoffRelease:
				return nil
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	guardian.scope = scope
	guardian.registrationRecorded = true
	guardian.terminationCause = task.LifetimeTerminationRecovery
	return &lifetimeGuardianProofBatchFixture{
		guardian:       guardian,
		scope:          scope,
		timeline:       timeline,
		backoffEntered: backoffEntered,
		backoffRelease: backoffRelease,
	}
}

func TestLifetimeGuardianResidentProofRetryHoldsAuthority(t *testing.T) {
	backoffEntered := make(chan struct{})
	backoffRelease := make(chan struct{})
	var backoffOnce sync.Once
	f := newLifetimeGuardianTestFixtureWithOptions(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		func(scope *lifetimeGuardianTestScope) {
			scope.emptyResults = []bool{false, false, false, false, false, false, false, false, false, false}
		},
		func(options *Options) {
			options.FinalizationBackoff = func(ctx context.Context, _ time.Duration) error {
				backoffOnce.Do(func() { close(backoffEntered) })
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-backoffRelease:
					return nil
				}
			}
		},
	)
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	if err := f.call(t, f.message(lifetimeMessageClose, 0, 0)); err == nil {
		t.Fatal("close succeeded despite a nonempty proof")
	}
	f.worker.release()
	select {
	case err := <-f.runDone:
		t.Fatalf("guardian returned while the next proof batch was blocked: %v", err)
	case <-backoffEntered:
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 0 || release != 0 {
		t.Fatalf("resident proof retry released scope: destroy=%d release=%d", destroy, release)
	}
	if timelineIndex(f.timeline.snapshot(), "release") >= 0 {
		t.Fatal("resident proof retry released the owner")
	}
	close(backoffRelease)
	if err := f.waitRun(t); err == nil {
		t.Fatal("Run lost the original proof failure")
	}
	if destroy, release := f.scope.disposalCounts(); destroy != 1 || release != 1 {
		t.Fatalf("resident proof retry disposal = destroy %d release %d, want 1/1", destroy, release)
	}
}

func TestLifetimeGuardianRetriesDestroyReleaseAndOwnerPhases(t *testing.T) {
	destroyErr := errors.New("transient destroy failure")
	scopeReleaseErr := errors.New("transient scope release failure")
	ownerReleaseErr := errors.New("transient owner release failure")
	f := newLifetimeGuardianTestFixtureWithScopeSetup(
		t,
		nil,
		lifetime.KindProcessGroup,
		lifetime.AssuranceCooperative,
		nil,
		func(scope *lifetimeGuardianTestScope) {
			scope.destroyErrors = []error{destroyErr, nil}
			scope.releaseErrors = []error{scopeReleaseErr, nil}
		},
	)
	f.owner.setReleaseErrors([]error{ownerReleaseErr, nil})
	f.callMust(t, "hello", f.message(lifetimeMessageHello, 0, 0))
	f.callMust(t, "close", f.message(lifetimeMessageClose, 0, 0))
	f.worker.release()
	if err := f.waitRun(t); err != nil {
		t.Fatalf("Run with transient cleanup failures: %v", err)
	}
	if destroy, scopeRelease := f.scope.disposalCounts(); destroy != 2 || scopeRelease != 2 {
		t.Fatalf("cleanup retries = destroy %d scope-release %d, want 2/2", destroy, scopeRelease)
	}
	f.owner.mu.Lock()
	ownerReleases := f.owner.releases
	f.owner.mu.Unlock()
	if ownerReleases != 2 {
		t.Fatalf("owner release calls = %d, want 2", ownerReleases)
	}
	events := f.timeline.snapshot()
	if timelineIndex(events, "scope-destroy") >= timelineIndex(events, "scope-release") || timelineIndex(events, "scope-release") >= timelineIndex(events, "release") {
		t.Fatalf("cleanup phase order = %v", events)
	}
}
