package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The outer coordinator owns newly acquired admission through all journal,
// capture and pending-control work. Nested coordinators borrow that exact
// resource and never release it. The store must outlive its admission owner.
func acquireWorkspaceRootAdmission(ctx context.Context, store *taskdir.Store, deps OperationOptions) (*taskdir.RootAdmissionLease, error) {
	return supervisorcore.AcquireOrBorrowRootAdmissionContext(ctx, store, deps.RootAdmission,
		pueue.PrivateControlTimeout(pueue.Options{ObservationTimeout: deps.ObservationTimeout}))
}

// A child already under a resource use owner cannot acquire the outer root
// lease. Legacy embeddings retain their existing caller hold. Managed clients
// receive the actual borrowed lease only when they transport control owners.
func workspaceControlAdmissionContext(ctx context.Context, store *taskdir.Store, client *pueue.Client, deps OperationOptions) (context.Context, error) {
	if err := checkWorkspaceRootAdmission(ctx, store, deps.RootAdmission); err != nil {
		return ctx, err
	}
	if deps.RootAdmission != nil && client.UsesControlOwner() {
		ctx = pueue.WithControlLease(ctx, deps.RootAdmission)
	}
	return ctx, nil
}

func checkWorkspaceRootAdmission(ctx context.Context, store *taskdir.Store, lease *taskdir.RootAdmissionLease) error {
	if lease == nil {
		return nil
	}
	if _, err := lease.PinRootContext(ctx); err != nil {
		return err
	}
	return lease.CheckRootContext(ctx, store)
}

func workspaceRootAdmissionError(cause error, lease, borrowed *taskdir.RootAdmissionLease) error {
	if lease == nil || lease == borrowed {
		return cause
	}
	var child *AdmissionCleanupError
	if errors.As(cause, &child) && child != nil && child.Admission().CleanupPending() {
		return supervisorcore.RetainOwnedRootAdmission(lease, cause)
	}
	return supervisorcore.ReleaseOwnedRootAdmission(lease, cause)
}
