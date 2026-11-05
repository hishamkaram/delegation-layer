package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Component wrapper represents a failed host cleanup while retaining actual
// production resources. No finalizer or forced GC establishes lease ownership.
type retainedPreparationTestError struct {
	store *taskdir.Store
	lease *taskdir.RootAdmissionLease
	cause error
}

func (*retainedPreparationTestError) RetainsResidentCleanup() {}
func (*retainedPreparationTestError) Error() string           { return "retained preparation cleanup" }
func (e *retainedPreparationTestError) Unwrap() error         { return e.cause }

func TestOwnerServerRetainsLaterCleanupOwnerAfterOrdinaryDiagnostic(t *testing.T) {
	var retained *retainedPreparationTestError
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		return Disposition{Kind: DispositionWarmReuse}, errors.Join(errors.New("operation fault"), retained)
	}})
	retained = newRetainedPreparationTestError(t, f.root)
	f.expectedCleanupFault = retained
	first := errors.New("earlier ordinary diagnostic")
	f.server.connectionFault(first)
	f.start()
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, f.request(t, ownerEnsureOwn)); err != nil {
		t.Fatal(err)
	}
	if reply := readServerReply(t, connection); reply.Code != ownerFault {
		t.Fatalf("resource failure reply: %s", reply.Code)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.server.stopAcceptAndDrain(ctx); !task.AnyErrorNode(err, func(node error) bool {
		var owner *retainedPreparationTestError
		return errors.As(node, &owner) && owner == retained
	}) {
		t.Fatalf("drain lost actual later cleanup owner: %v", err)
	}
	if !errors.Is(f.server.diagnostic(), first) {
		t.Fatal("ordinary bounded diagnostic changed")
	}
	if !f.server.isClosing() {
		t.Fatal("retained cleanup did not close admission")
	}
	exclusive, err := retained.store.TryAcquireRootAdmissionExclusiveContext(ctx)
	if exclusive != nil {
		t.Cleanup(func() {
			if releaseErr := exclusive.Release(); releaseErr != nil {
				t.Error(releaseErr)
			}
		})
	}
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("retained root SH barrier disappeared: %v", err)
	}
}

func newRetainedPreparationTestError(t *testing.T, root string) *retainedPreparationTestError {
	t.Helper()
	store, err := taskdir.OpenStoreWithPredicatesContext(t.Context(), root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	owner := &retainedPreparationTestError{store: store, cause: errors.New("injected host cleanup result")}
	t.Cleanup(func() {
		if owner.lease != nil {
			if releaseErr := owner.lease.Release(); releaseErr != nil {
				t.Error(releaseErr)
				return
			}
		}
		if closeErr := owner.store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	owner.lease, err = store.AcquireRootAdmissionContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return owner
}
