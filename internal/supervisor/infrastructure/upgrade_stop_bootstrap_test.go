package infrastructure

import (
	"context"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestUpgradeStopWhileOperationWaitsForBootstrapIsBusyThenProgresses(t *testing.T) {
	host := &ownerServerTestHost{}
	f := newOwnerServerFixture(t, host)
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	entered, progressed := make(chan struct{}), make(chan struct{})
	host.ensure = func(ctx context.Context, _ Operation, _ LaunchContext, _ DaemonStartGuard, _ DaemonWaitOwner) (Disposition, error) {
		close(entered)
		err := pueue.WithExistingBootstrapLockContext(ctx, f.root, func(context.Context) error {
			close(progressed)
			return nil
		})
		return Disposition{Kind: DispositionWarmReuse}, err
	}
	reader := contextPassiveReader(t, f.entry.journal)
	request := f.request(t, ownerEnsureOwn)
	request.Context = f.entry.operation.defaultContextReference()
	selected := request.Owner
	f.start()
	connection := f.dial(t)
	err := pueue.WithBootstrapLockContext(ctx, f.root, func(context.Context) error {
		if err := writeOwnerFrame(connection, request); err != nil {
			return err
		}
		select {
		case <-entered:
		case <-ctx.Done():
			return ctx.Err()
		}
		result, err := StopOwnerServingAt(ctx, reader, selected)
		if err != nil || result.State != OwnerStopBusy || result.Startup != selected || f.server.isClosing() {
			t.Errorf("admitted bootstrap waiter did not refuse serving release: %+v %v", result, err)
		}
		select {
		case <-progressed:
			t.Error("operation crossed the held bootstrap fence")
		default:
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	reply := readServerReply(t, connection)
	if reply.Code != ownerOK || reply.Disposition.Kind != DispositionWarmReuse {
		t.Fatalf("operation did not progress after fence unlock: %+v", reply)
	}
	select {
	case <-progressed:
	default:
		t.Fatal("successful reply preceded bootstrap progress")
	}
}

func TestUpgradeStopReleasesServingUnderBootstrapBeforeActualChildWait(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Reuse the existing finite native component child and its sole cmd.Wait.
	// This proves serving versus Wait ownership, not Pueue daemon placement.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	child, attempt := newUpgradeWaitChild(t)
	if err := f.server.waits.RegisterDaemonWait(f.root, child.command.Process.Pid, attempt.NativeCompletion()); err != nil {
		t.Fatal(err)
	}
	reader := contextPassiveReader(t, f.entry.journal)
	selected, err := ReferenceStartup(f.entry.operation.Intent())
	if err != nil {
		t.Fatal(err)
	}
	joinUpgradeServingRelease(t, f, ctx)
	f.start()
	err = pueue.WithBootstrapLockContext(ctx, f.root, func(context.Context) error {
		result, stopErr := StopOwnerServingAt(ctx, reader, selected)
		if stopErr != nil || result.State != OwnerServingReleased || result.ProcessClosure != OwnerProcessPending || result.Startup != selected {
			t.Errorf("serving release acquired bootstrap or claimed process closure: %+v %v", result, stopErr)
		}
		select {
		case <-attempt.NativeCompletion():
			t.Error("serving release ended the owned child before its input closed")
		default:
		}
		f.server.waits.mu.Lock()
		registration, retained := f.server.waits.active[attempt.NativeCompletion()]
		f.server.waits.mu.Unlock()
		if !retained || registration.pid != child.command.Process.Pid {
			t.Error("serving release discarded the actual child Wait")
		}
		return stopErr
	})
	if err != nil {
		t.Fatal(err)
	}
	finishUpgradeWaitChild(t, ctx, child, attempt)
}

func newUpgradeWaitChild(t *testing.T) (*producerNativeFixture, *StartupAttempt) {
	t.Helper()
	child := newProducerNativeFixture(t)
	attempt, err := producerInvocation(t).establishOutside(t.Context(), child.store, child.request, child.host, child.native(t, nil))
	if attempt != nil {
		child.attempts = append(child.attempts, attempt)
	}
	if err != nil || attempt == nil || attempt.NativeCompletion() == nil {
		t.Fatalf("finite native child establishment: %v", err)
	}
	child.ready(t, attempt)
	return child, attempt
}

func finishUpgradeWaitChild(t *testing.T, ctx context.Context, child *producerNativeFixture, attempt *StartupAttempt) {
	t.Helper()
	if err := child.stop.Close(); err != nil {
		t.Fatal(err)
	}
	child.stop = nil
	select {
	case <-attempt.NativeCompletion():
		if waitErr, complete := attempt.NativeWaitError(); !complete || waitErr != nil {
			t.Fatalf("actual child Wait: complete=%t err=%v", complete, waitErr)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func joinUpgradeServingRelease(t *testing.T, f *ownerServerFixture, ctx context.Context) {
	t.Helper()
	released := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() {
		cancel()
		if err := <-released; err != nil {
			t.Error(err)
			return
		}
		if err := f.server.fault(); err != nil {
			t.Error(err)
			return
		}
		// The actual server is drained and its endpoint/lease are released.
		// The fixture's separate served channel still joins its serving loop.
		f.server = nil
	})
	go func() {
		select {
		case <-f.server.drained:
			_, err := f.entry.releaseServingContext(ctx)
			released <- err
		case <-ctx.Done():
			released <- ctx.Err()
		}
	}()
}
