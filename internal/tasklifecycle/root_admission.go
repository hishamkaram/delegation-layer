package tasklifecycle

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/workspaces"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The outer creator owns Release. Legacy embedding retains its in-process
// caller hold; only configured managed control transports inherited references.
// An incoming attached lease is never removed to permit legacy fallback.
func ordinaryRootAdmission(ctx context.Context, store *taskdir.Store, borrowed *taskdir.RootAdmissionLease, options pueue.Options) (context.Context, *taskdir.RootAdmissionLease, error) {
	lease, err := supervisorcore.AcquireOrBorrowRootAdmissionContext(ctx, store, borrowed, pueue.PrivateControlTimeout(options))
	if err != nil {
		return ctx, lease, err
	}
	if options.ControlOwnerExecutable != "" {
		ctx = pueue.WithControlLease(ctx, lease)
	}
	return ctx, lease, nil
}

// These finalizers are called only by the invocation that acquired ownership.
// Failed release retains the exact resource; Store cannot close behind it.
func releaseRootAdmission(lease *taskdir.RootAdmissionLease, cause error) (*taskdir.RootAdmissionLease, error) {
	if lease != nil && lease.CleanupPending() && CleanupUnresolved(cause) {
		return lease, nil
	}
	if err := lease.Release(); err != nil {
		return lease, err
	}
	return nil, nil
}

// CleanupUnresolved reports active cleanup owners in this invocation's causal
// graph. It confers no actuation or retry permission and differs from the
// historical presence of a cleanup fault whose owner has already settled.
func CleanupUnresolved(cause error) bool {
	return pendingAdmissionCleanup(cause) || supervisorcore.CleanupHasRetainedStore(cause)
}

func closeAdmissionStore(store *taskdir.Store, unresolved *taskdir.RootAdmissionLease, cause error) (*taskdir.Store, error) {
	if unresolved != nil || pendingAdmissionCleanup(cause) || supervisorcore.CleanupRetainsStore(cause, store) {
		return store, nil
	}
	if err := store.Close(); err != nil {
		return store, err
	}
	return nil, nil
}

func pendingAdmissionCleanup(cause error) bool {
	return task.AnyErrorNode(cause, func(node error) bool {
		var lifetime *taskdir.LifetimeOwnerCleanupError
		if errors.As(node, &lifetime) && lifetime.Lease().CleanupPending() {
			return true
		}
		var admission *workspaces.AdmissionCleanupError
		if errors.As(node, &admission) && admission != nil && admission.Admission().CleanupPending() {
			return true
		}
		var root *supervisorcore.RootAdmissionCleanupError
		return errors.As(node, &root) && root.Lease().CleanupPending()
	})
}

func continuationWorkError(result ContinuationResult) error {
	if result.Dispatch != nil {
		return errors.Join(result.Failure, result.Dispatch.Err())
	}
	return result.Failure
}

func admissionCleanupFault(cause error) bool {
	var lifetime *taskdir.LifetimeOwnerCleanupError
	var admission *workspaces.AdmissionCleanupError
	var root *supervisorcore.RootAdmissionCleanupError
	return errors.As(cause, &lifetime) || errors.As(cause, &admission) || errors.As(cause, &root) || task.AnyErrorNode(cause, func(node error) bool {
		var retained interface{ RetainedStore() *taskdir.Store }
		return errors.As(node, &retained) && retained != nil
	})
}
