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
	store   *taskdir.Store
	lease   *taskdir.LifetimeOwnerLease
	control *taskdir.ControlDir
	cause   error
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
	retained.assertLeaseHeld(t, ctx)
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
		if owner.control != nil {
			if closeErr := owner.control.Close(); closeErr != nil {
				t.Error(closeErr)
				return
			}
		}
		if closeErr := owner.store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	owner.control, err = store.OpenInfrastructureContext(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	owner.lease, err = owner.control.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

// The generic control lifetime owner is an actual native resource. This checks
// retained custody without reinstating the rejected all-writer root fence.
func (e *retainedPreparationTestError) assertLeaseHeld(t *testing.T, ctx context.Context) {
	t.Helper()
	peer, err := e.store.OpenInfrastructureContext(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := peer.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	candidate, acquireErr := peer.TryAcquireLifetimeOwnerContext(ctx)
	if candidate != nil {
		if releaseErr := candidate.Release(); releaseErr != nil {
			t.Fatal(releaseErr)
		}
	}
	if !errors.Is(acquireErr, task.ErrLockBusy) {
		t.Fatalf("retained native lifetime custody disappeared: %v", acquireErr)
	}
}
