package infrastructure

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestWarmOwnerWireClassifiesOnlyCleanBusyAndIncompatible(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		code  ownerReplyCode
	}{
		{"local-busy", task.ErrLockBusy, ownerBusy},
		{"remote-busy", ErrOwnerBusy, ownerBusy},
		{"joined-busy", errors.Join(task.ErrLockBusy, ErrOwnerBusy), ownerBusy},
		{"local-incompatible", ErrOwnerIncompatible, ownerIncompatible},
		{"remote-incompatible", ErrOwnerIncompatible, ownerIncompatible},
		{"joined-incompatible", errors.Join(ErrOwnerIncompatible, ErrOwnerIncompatible), ownerIncompatible},
		{"busy-with-storage-fault", errors.Join(task.ErrLockBusy, errors.New("storage read failed")), ownerFault},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
				calls.Add(1)
				return Disposition{}, test.cause
			}})
			f.start()
			connection := f.dial(t)
			if err := writeOwnerFrame(connection, f.request(t, ownerEnsureOwn)); err != nil {
				t.Fatal(err)
			}
			if reply := readServerReply(t, connection); reply.Code != test.code || reply.Disposition != (Disposition{}) || calls.Load() != 1 {
				t.Fatalf("wire classification lost cause: code=%s want=%s calls=%d disposition=%v", reply.Code, test.code, calls.Load(), reply.Disposition)
			}
		})
	}
}

func TestWarmOwnerWireRetainsCleanupBusyAsFault(t *testing.T) {
	var retained *retainedPreparationTestError
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		return Disposition{}, retained
	}})
	retained = newRetainedPreparationTestError(t, f.root)
	retained.cause = task.ErrLockBusy
	f.expectedCleanupFault = retained
	f.start()
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, f.request(t, ownerEnsureOwn)); err != nil {
		t.Fatal(err)
	}
	if reply := readServerReply(t, connection); reply.Code != ownerFault {
		t.Fatalf("cleanup busy misclassified as clean retryable busy: %s", reply.Code)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := f.server.stopAcceptAndDrain(ctx)
	var actual *retainedPreparationTestError
	if !errors.As(err, &actual) || actual != retained || !f.server.isClosing() {
		t.Fatalf("wire failure lost actual retained cleanup graph: %v", err)
	}
	retained.assertLeaseHeld(t, ctx)
}

func TestWarmOwnerCleanBusyDoesNotMaskLaterFault(t *testing.T) {
	var calls atomic.Int32
	fault := errors.New("later preparation storage fault")
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		if calls.Add(1) == 1 {
			return Disposition{}, errors.Join(task.ErrLockBusy, ErrOwnerBusy)
		}
		return Disposition{}, fault
	}})
	f.start()
	for _, expected := range []ownerReplyCode{ownerBusy, ownerFault} {
		connection := f.dial(t)
		if err := writeOwnerFrame(connection, f.request(t, ownerEnsureOwn)); err != nil {
			t.Fatal(err)
		}
		if reply := readServerReply(t, connection); reply.Code != expected {
			t.Fatalf("wrong preparation code: got=%s want=%s", reply.Code, expected)
		}
		if expected == ownerBusy && f.server.diagnostic() != nil {
			t.Fatalf("clean busy consumed the first diagnostic slot: %v", f.server.diagnostic())
		}
	}
	if !errors.Is(f.server.diagnostic(), fault) || calls.Load() != 2 {
		t.Fatalf("later fault lost after clean busy: calls=%d diagnostic=%v", calls.Load(), f.server.diagnostic())
	}
}
