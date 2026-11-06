package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

// The native child owns a real sole Wait. The retained completion models the
// preparation worker that outlives its callback's deadline, paused between
// successful Start and registration. This is not a Pueue placement proof.
func TestOwnerRuntimeRegistersLateWorkerWaitBeforeServingRelease(t *testing.T) {
	host := &ownerServerTestHost{}
	f := newOwnerServerFixture(t, host)
	waits := f.server.waits
	child := newOwnerServerOwnedChild(t, f)
	completion := &residentCompletionFixtureError{done: make(chan struct{})}
	registration := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(registration) }) }
	host.ensure = func(ctx context.Context, operation Operation, _ LaunchContext, guard DaemonStartGuard, waits DaemonWaitOwner) (Disposition, error) {
		started := make(chan error, 1)
		go func() {
			startErr := guard.GuardDaemonStart(ctx, operation.Intent().Inputs.Root, child.start)
			started <- startErr
			if startErr == nil {
				<-registration
				startErr = waits.RegisterDaemonWait(operation.Intent().Inputs.Root, child.pid(), child.done)
			}
			completion.err = startErr
			close(completion.done)
		}()
		if err := <-started; err != nil {
			return Disposition{}, errors.Join(err, completion)
		}
		return Disposition{}, errors.Join(context.DeadlineExceeded, completion)
	}
	cancel, done, join := startRuntimeFixtureExpecting(t, f, context.DeadlineExceeded)
	t.Cleanup(func() {
		cancel()
		unblock()
		if err := child.gateWrite.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error(err)
		}
		join()
	})
	if ready, err := bufio.NewReader(child.output).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("finite child readiness: %q %v", ready, err)
	}
	cancel()
	awaitResidentFixture(t, f.server.drained)
	unblock()
	awaitResidentFixture(t, completion.done)
	if completion.err != nil {
		t.Fatalf("late actual child Wait registration was rejected after callback drain: %v", completion.err)
	}
	awaitServingResourceRelease(t, f.entry, done)
	select {
	case <-done:
		t.Fatal("runtime returned before the late child's sole Wait")
	default:
	}
	if err := child.gateWrite.Close(); err != nil {
		t.Fatal(err)
	}
	awaitResidentFixture(t, child.done)
	join()
	child.verifyReaped(t, t.Context(), waits)
}
