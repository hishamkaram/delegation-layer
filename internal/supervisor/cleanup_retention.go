package supervisor

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// CleanupRetainsStore observes cleanup ownership throughout the whole error
// graph. RetainedStore names a borrowed Store still needed by a caller-owned
// cleanup handle; it grants no retry, lifecycle or mutation authority.
func CleanupRetainsStore(cause error, store *taskdir.Store) bool {
	if store == nil {
		return false
	}
	return task.AnyErrorNode(cause, func(node error) bool {
		var retained interface{ RetainedStore() *taskdir.Store }
		if errors.As(node, &retained) && retained != nil && retained.RetainedStore() == store {
			return true
		}
		var root *RootAdmissionCleanupError
		if errors.As(node, &root) && root != nil && root.Lease().RetainsStore(store) {
			return true
		}
		var lifetime *taskdir.LifetimeOwnerCleanupError
		return errors.As(node, &lifetime) && lifetime != nil && lifetime.Lease().RetainsStore(store)
	})
}

// CleanupHasRetainedStore reports an active dependent cleanup in this
// invocation's error graph. Its ancestor lease must stay held even when the
// child borrowed another Store for the same root. This is dependency custody;
// CleanupRetainsStore separately decides exact Store handle custody.
func CleanupHasRetainedStore(cause error) bool {
	return task.AnyErrorNode(cause, func(node error) bool {
		var retained interface{ RetainedStore() *taskdir.Store }
		return errors.As(node, &retained) && retained != nil && retained.RetainedStore() != nil
	})
}
