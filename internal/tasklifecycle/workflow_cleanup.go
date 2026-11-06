package tasklifecycle

import (
	"errors"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// CleanupUnresolved reports active cleanup owners in this invocation's causal
// graph. It confers no actuation or retry permission and differs from the
// historical presence of a cleanup fault whose owner has already settled.
func CleanupUnresolved(cause error) bool {
	return pendingAdmissionCleanup(cause) || supervisorcore.CleanupHasRetainedStore(cause)
}

func closeAdmissionStore(store *taskdir.Store, cause error) (*taskdir.Store, error) {
	if pendingAdmissionCleanup(cause) || supervisorcore.CleanupRetainsStore(cause, store) {
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
		return false
	})
}

func admissionCleanupFault(cause error) bool {
	var lifetime *taskdir.LifetimeOwnerCleanupError
	var admission *workspaces.AdmissionCleanupError
	return errors.As(cause, &lifetime) || errors.As(cause, &admission) || task.AnyErrorNode(cause, func(node error) bool {
		var retained interface{ RetainedStore() *taskdir.Store }
		return errors.As(node, &retained) && retained != nil
	})
}
