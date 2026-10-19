package control

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// errCallCompletedBeforeEntry reports a call that returned before reaching
// the boundary its test was waiting for; the call's exact result stays
// readable on the call.
var errCallCompletedBeforeEntry = errors.New("call completed before entry")

// testCallScope owns the goroutines, contexts and release barriers one test
// starts against a supervisor fixture. Calls share one absolute operation
// cutoff; cleanup has one final deadline that reserves an observation bound
// and is never renewed. Cleanup is registered at construction, so it runs
// before the earlier-registered fixture cleanups: it cancels every context,
// then releases every barrier, then joins every call in start order and
// settles any pending command a call returned.
type testCallScope struct {
	t      *testing.T
	cutoff time.Time
	final  time.Time

	cancels  []context.CancelFunc
	releases []func()
	joins    []func()
}

// pendingResult is a call result that may carry a command its call left in
// flight; cleanup waits for that command's natural completion.
type pendingResult interface {
	pendingCommand() *Pending
}

// testCall is one call started by a scope. result is written once before
// done closes, so any number of consumers may read it after done.
type testCall[T pendingResult] struct {
	done   chan struct{}
	result T
}

// lockCallResult is the result of a call that returns only an error.
type lockCallResult struct{ err error }

func (lockCallResult) pendingCommand() *Pending { return nil }

func newTestCallScope(t *testing.T, options Options) *testCallScope {
	t.Helper()
	cutoff := time.Now().Add(PrivateControlTimeout(options))
	scope := &testCallScope{t: t, cutoff: cutoff, final: cutoff.Add(observationBound(options))}
	t.Cleanup(scope.cleanup)
	return scope
}

func observationBound(options Options) time.Duration {
	if options.ObservationTimeout > 0 {
		return options.ObservationTimeout
	}
	return DefaultObservationTimeout
}

// context returns a call context bounded by the operation cutoff and
// canceled first during cleanup.
func (s *testCallScope) context() context.Context {
	ctx, _ := s.cancelableContext()
	return ctx
}

// cancelableContext is context for a test that also cancels it itself.
func (s *testCallScope) cancelableContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithDeadline(context.Background(), s.cutoff)
	s.cancels = append(s.cancels, cancel)
	return ctx, cancel
}

// barrier returns a channel closed once by release; cleanup releases it
// after canceling every context.
func (s *testCallScope) barrier() (<-chan struct{}, func()) {
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	s.onRelease(release)
	return released, release
}

// onRelease registers an idempotent fixture release for cleanup.
func (s *testCallScope) onRelease(release func()) {
	s.releases = append(s.releases, release)
}

func (s *testCallScope) cleanup() {
	for _, cancel := range s.cancels {
		cancel()
	}
	for _, release := range s.releases {
		release()
	}
	for _, join := range s.joins {
		join()
	}
}

// startTestCall runs run on its own goroutine under scope's cleanup.
func startTestCall[T pendingResult](s *testCallScope, run func() T) *testCall[T] {
	call := &testCall[T]{done: make(chan struct{})}
	s.joins = append(s.joins, func() {
		if !waitUntil(call.done, s.final) {
			s.t.Error("test call did not join before the cleanup deadline")
			return
		}
		if pending := call.result.pendingCommand(); pending != nil && !awaitPendingNaturally(pending, time.Until(s.final)) {
			s.t.Error("pending command did not complete before the cleanup deadline")
		}
	})
	go func() {
		call.result = run()
		close(call.done)
	}()
	return call
}

// waitForEntry waits for entered. It returns errCallCompletedBeforeEntry
// when call completed without entering, or an error at the operation cutoff.
func waitForEntry[T pendingResult](s *testCallScope, entered <-chan struct{}, call *testCall[T]) error {
	timer := time.NewTimer(time.Until(s.cutoff))
	defer timer.Stop()
	select {
	case <-entered:
		return nil
	case <-call.done:
		// A call that entered before completing still entered.
		select {
		case <-entered:
			return nil
		default:
		}
		return fmt.Errorf("%w: %+v", errCallCompletedBeforeEntry, call.result)
	case <-timer.C:
		return errors.New("no entry before the operation cutoff")
	}
}

func awaitEntry[T pendingResult](s *testCallScope, entered <-chan struct{}, call *testCall[T], message string) {
	s.t.Helper()
	if err := waitForEntry(s, entered, call); err != nil {
		s.t.Fatalf("%s: %v", message, err)
	}
}

func awaitCall[T pendingResult](s *testCallScope, call *testCall[T], what string) T {
	s.t.Helper()
	if !waitUntil(call.done, s.cutoff) {
		s.t.Fatalf("%s did not finish before the operation cutoff", what)
	}
	return call.result
}

func awaitLockCall(s *testCallScope, call *testCall[lockCallResult], what string) {
	s.t.Helper()
	if result := awaitCall(s, call, what); result.err != nil {
		s.t.Fatalf("%s: %v", what, result.err)
	}
}

// waitUntil reports whether done closed before the absolute deadline.
func waitUntil(done <-chan struct{}, deadline time.Time) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
