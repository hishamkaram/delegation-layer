package app

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// workspaceCommandResources owns cleanup for one coordinator invocation. The
// Store's native pin remains authoritative. Close stops at a failed
// prerequisite and retains
// every dependent handle. A callback's borrowed store is never closed here.
type workspaceCommandResources struct {
	store      *taskdir.Store
	ownsStore  bool
	resource   *workspace.Resource
	childOwner *taskdir.LifetimeOwnerLease
	useOwner   *taskdir.LifetimeOwnerLease
	admission  *workspacescore.Admission
}

func (r *workspaceCommandResources) closeAfter(cause error) error {
	if task.AnyErrorNode(cause, func(node error) bool {
		var child *workspacescore.AdmissionCleanupError
		if !errors.As(node, &child) || child == nil || !child.Admission().CleanupPending() {
			return false
		}
		r.admission = child.Admission()
		return true
	}) {
		return &workspaceCommandCleanupError{owner: r, cause: cause}
	}
	if task.AnyErrorNode(cause, func(node error) bool {
		var child *taskdir.LifetimeOwnerCleanupError
		if !errors.As(node, &child) || !child.Lease().RetainsStore(r.store) {
			return false
		}
		if lease := child.Lease(); lease != r.useOwner {
			r.childOwner = lease
		}
		return true
	}) {
		return &workspaceCommandCleanupError{owner: r, cause: cause}
	}
	if err := r.close(); err != nil {
		return &workspaceCommandCleanupError{owner: r, cause: err}
	}
	return nil
}

func (r *workspaceCommandResources) close() error {
	if r.admission != nil {
		if err := r.admission.Close(); err != nil {
			return err
		}
		r.admission = nil
	}
	if r.childOwner != nil {
		if err := r.childOwner.ReleaseWithError(nil); err != nil {
			return err
		}
		r.childOwner = nil
	}
	if r.useOwner != nil {
		if err := r.useOwner.ReleaseWithError(nil); err != nil {
			return err
		}
		r.useOwner = nil
	}
	if r.resource != nil {
		if err := r.resource.Close(); err != nil {
			return err
		}
		r.resource = nil
	}

	if r.ownsStore && r.store != nil {
		if err := r.store.Close(); err != nil {
			return err
		}
		r.store = nil
	}
	return nil
}

// A failed native close can be sticky. This error retains the exact handles
// and borrowed store without promising that a later cleanup attempt succeeds.
type workspaceCommandCleanupError struct {
	owner *workspaceCommandResources
	cause error
}

func (e *workspaceCommandCleanupError) Error() string {
	return "workspace command cleanup unresolved: " + e.cause.Error()
}

func (e *workspaceCommandCleanupError) Unwrap() error { return e.cause }

func (e *workspaceCommandCleanupError) RetainedStore() *taskdir.Store {
	owner := e.owner
	if owner.admission.CleanupPending() || owner.childOwner.CleanupPending() || owner.useOwner.CleanupPending() || owner.resource != nil || (owner.ownsStore && owner.store != nil) {
		return owner.store
	}
	if task.AnyErrorNode(e.cause, func(node error) bool {
		var lifetime *taskdir.LifetimeOwnerCleanupError
		if errors.As(node, &lifetime) && lifetime.Lease().RetainsStore(owner.store) {
			return true
		}
		var admission *workspacescore.AdmissionCleanupError
		if errors.As(node, &admission) && admission != nil && admission.Admission().CleanupPending() {
			return true
		}
		return false
	}) {
		return owner.store
	}
	return nil
}

// Cleanup explicitly settles every retained child before the coordinator's
// parent resources. Successful native settlement deduplicates repeated handles;
// a failure stops this call before another attempt or any parent release.
func (e *workspaceCommandCleanupError) Cleanup() error {
	for {
		var child *taskdir.LifetimeOwnerLease
		task.AnyErrorNode(e.cause, func(node error) bool {
			var cleanup *taskdir.LifetimeOwnerCleanupError
			if !errors.As(node, &cleanup) || !cleanup.Lease().RetainsStore(e.owner.store) {
				return false
			}
			child = cleanup.Lease()
			return true
		})
		if child == nil {
			break
		}
		if err := child.ReleaseWithError(nil); err != nil {
			return &workspaceCommandCleanupError{owner: e.owner, cause: errors.Join(e.cause, err)}
		}
	}
	for {
		var child *workspacescore.Admission
		task.AnyErrorNode(e.cause, func(node error) bool {
			var cleanup *workspacescore.AdmissionCleanupError
			if !errors.As(node, &cleanup) || cleanup == nil || !cleanup.Admission().CleanupPending() {
				return false
			}
			child = cleanup.Admission()
			return true
		})
		if child == nil {
			break
		}
		if err := child.Close(); err != nil {
			return &workspaceCommandCleanupError{owner: e.owner, cause: errors.Join(e.cause, err)}
		}
	}
	if err := e.owner.close(); err != nil {
		return &workspaceCommandCleanupError{owner: e.owner, cause: errors.Join(e.cause, err)}
	}
	return nil
}
