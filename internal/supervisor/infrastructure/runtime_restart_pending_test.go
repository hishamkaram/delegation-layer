package infrastructure

import (
	"context"
	"errors"
	"maps"
	"os"
	"sync/atomic"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The host owns classification of the domain leaf. These Unix RPC tests check
// its typed boundary without importing the supervisor's implementation owner.
func TestRuntimeRestartPendingOwnPreparationIsRetryableOnlyForPureCause(t *testing.T) {
	pending := errors.New("host runtime restart pending")
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pure", true: "joined storage fault"}[mixed], func(t *testing.T) {
			failure := PreparationFailure{Causes: PreparationCauseRuntimeRestartPending, IndependentFault: mixed}
			cause := pending
			want := ownerPending
			if mixed {
				cause = errors.Join(cause, os.ErrPermission)
				want = ownerFault
			}
			var calls atomic.Int32
			f := newOwnerServerFixture(t, &ownerServerTestHost{
				ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
					calls.Add(1)
					return Disposition{}, cause
				},
				classify: func(error) PreparationFailure { return failure },
			})
			before := producerBudgetTree(t, f.root)
			request := f.request(t, ownerEnsureOwn)
			f.start()
			connection := f.dial(t)
			if err := writeOwnerFrame(connection, request); err != nil {
				t.Fatal(err)
			}
			reply := readServerReply(t, connection)
			if reply.Code != want || reply.Failure != failure || reply.Startup != request.Owner || reply.Disposition != (Disposition{}) || calls.Load() != 1 {
				t.Fatalf("pending cause lost or joined fault hidden: calls=%d reply=%+v", calls.Load(), reply)
			}
			if !maps.Equal(before, producerBudgetTree(t, f.root)) {
				t.Fatal("refused own preparation changed retained state")
			}
		})
	}
}

func TestRuntimeRestartPendingOtherRootRefusesBeforeStartupAcceptance(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pure", true: "joined storage fault"}[mixed], func(t *testing.T) {
			exerciseRuntimeRestartOtherRootRefusal(t, mixed)
		})
	}
}

func exerciseRuntimeRestartOtherRootRefusal(t *testing.T, mixed bool) {
	t.Helper()
	target := producerFixture(t)
	selected, _, err := target.journal.stageContext(t.Context(), target.request.Context)
	if err != nil {
		t.Fatal(err)
	}
	releaseProducerFixtureLease(t, target)
	before := supervisorfixture.SnapshotStore(t, target.journal.pin.Root())
	cause := errors.New("host runtime restart pending")
	want := ownerBusy
	if mixed {
		cause = errors.Join(cause, os.ErrPermission)
		want = ownerFault
	}
	var callbacks atomic.Int32
	f := newOwnerServerFixture(t, &ownerServerTestHost{
		establish: func(_ context.Context, store *taskdir.Store, initial InitialOperation) error {
			callbacks.Add(1)
			if store.Root != target.journal.pin.Root() || initial != (InitialOperation{}) {
				t.Error("producer fence received a different target or initial purpose")
			}
			return cause
		},
		classify: func(error) PreparationFailure {
			return PreparationFailure{Causes: PreparationCauseRuntimeRestartPending, IndependentFault: mixed}
		},
	})
	request := f.request(t, ownerEnsureOther)
	request.Target = originRecord{Root: target.journal.pin.Root(), RootID: target.journal.pin.RootID()}
	request.Context = selected
	f.start()
	connection := f.dial(t)
	if err = writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	reply := readServerReply(t, connection)
	if reply.Code != want || reply.Startup != (StartupReference{}) || reply.InitialResultFor != (StartupReference{}) || callbacks.Load() != 1 {
		t.Fatalf("preacceptance refusal invented startup or lost busy: calls=%d reply=%+v", callbacks.Load(), reply)
	}
	after := supervisorfixture.SnapshotStore(t, target.journal.pin.Root())
	if !maps.Equal(before, after) {
		t.Fatal("refused other-root establishment staged an intent or changed target state")
	}
}
