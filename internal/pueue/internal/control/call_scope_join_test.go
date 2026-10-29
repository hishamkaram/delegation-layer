package control

import (
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
)

// heldScopeResult is a test call result whose returned Pending is chosen by
// the test.
type heldScopeResult struct{ pending *Pending }

func (r heldScopeResult) pendingCommand() *Pending { return r.pending }

// expiredCallScope returns a scope whose final deadline has already passed.
// Its reports are recorded instead of failing the test, so the watchdog
// branch can be observed.
func expiredCallScope(t *testing.T) (*testCallScope, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var reports []string
	report := func(message string) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, message)
	}
	now := time.Now()
	scope := &testCallScope{t: t, cutoff: now, final: now, report: report}
	return scope, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reports)
	}
}

// runScopeCleanup starts cleanup and returns a channel closed when it ends.
func runScopeCleanup(scope *testCallScope) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		scope.cleanup()
		close(done)
	}()
	return done
}

func closed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// TestCallScopeCleanupJoinsCallAfterExpiredDeadline holds a call past the
// expired cleanup deadline. Cleanup reports the expiry but cannot finish
// until the call is released and has finished. Checks before the release
// report without stopping, so the held call is always released.
func TestCallScopeCleanupJoinsCallAfterExpiredDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scope, reports := expiredCallScope(t)
		release := make(chan struct{})
		call := startTestCall(scope, func() heldScopeResult {
			<-release
			return heldScopeResult{}
		})
		cleanupDone := runScopeCleanup(scope)
		synctest.Wait()
		if got, want := reports(), []string{"test call did not join before the cleanup deadline"}; !slices.Equal(got, want) {
			t.Errorf("expired join reports = %q, want %q", got, want)
		}
		if closed(cleanupDone) {
			t.Error("cleanup finished while its expired call was still held")
		}
		close(release)
		synctest.Wait()
		if !closed(call.done) || !closed(cleanupDone) {
			t.Fatalf("released call joined %t, cleanup finished %t, want both", closed(call.done), closed(cleanupDone))
		}
	})
}

// TestCallScopeCleanupJoinsPendingAfterExpiredDeadline returns a Pending
// that is still unfinished at the expired cleanup deadline. Cleanup reports
// the expiry but cannot finish until that Pending completes. Checks before
// completion report without stopping, so the Pending always completes.
func TestCallScopeCleanupJoinsPendingAfterExpiredDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scope, reports := expiredCallScope(t)
		pending := transport.NewPending(nil)
		call := startTestCall(scope, func() heldScopeResult { return heldScopeResult{pending: pending} })
		<-call.done
		cleanupDone := runScopeCleanup(scope)
		synctest.Wait()
		if got, want := reports(), []string{"pending command did not complete before the cleanup deadline"}; !slices.Equal(got, want) {
			t.Errorf("expired pending reports = %q, want %q", got, want)
		}
		if closed(cleanupDone) {
			t.Error("cleanup finished while its returned Pending was unfinished")
		}
		transport.CompletePending(pending, CommandResult{ExitCode: 0})
		synctest.Wait()
		if !closed(cleanupDone) {
			t.Fatal("cleanup did not finish after its returned Pending completed")
		}
	})
}

// TestCallScopeCleanupAcceptsCompletedWorkAfterExpiredDeadline runs a call
// that has finished, with and without an already completed Pending, under an
// expired cleanup deadline. Visible completion wins: cleanup reports nothing
// and finishes.
func TestCallScopeCleanupAcceptsCompletedWorkAfterExpiredDeadline(t *testing.T) {
	cases := map[string]*Pending{
		"no pending":        nil,
		"completed pending": transport.NewCompletedPending(nil, CommandResult{ExitCode: 0}),
	}
	for name, pending := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				scope, reports := expiredCallScope(t)
				call := startTestCall(scope, func() heldScopeResult { return heldScopeResult{pending: pending} })
				<-call.done
				cleanupDone := runScopeCleanup(scope)
				synctest.Wait()
				if got := reports(); len(got) != 0 {
					t.Errorf("completed work reports = %q, want none", got)
				}
				if !closed(cleanupDone) {
					t.Error("cleanup did not finish for completed work")
				}
			})
		})
	}
}
