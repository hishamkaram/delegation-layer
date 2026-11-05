package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// workspaceCommandResources owns cleanup for one coordinator invocation. The
// store's native pin remains authoritative; options and contexts only borrow
// the acquired root resource. Close stops at a failed prerequisite and retains
// every dependent handle. A callback's borrowed store is never closed here.
type workspaceCommandResources struct {
	store         *taskdir.Store
	ownsStore     bool
	rootAdmission *taskdir.RootAdmissionLease
	resource      *workspace.Resource
	childOwner    *taskdir.LifetimeOwnerLease
	useOwner      *taskdir.LifetimeOwnerLease
	admission     *workspacescore.Admission
}

func (r *workspaceCommandResources) acquire(ctx context.Context, borrowed *taskdir.RootAdmissionLease, options pueue.Options) (context.Context, *taskdir.RootAdmissionLease, error) {
	lease, err := supervisorcore.AcquireOrBorrowRootAdmissionContext(ctx, r.store, borrowed, pueue.PrivateControlTimeout(options))
	if lease != nil && lease != borrowed {
		r.rootAdmission = lease
	}
	if err != nil {
		return ctx, lease, err
	}
	if options.ControlOwnerExecutable != "" {
		ctx = pueue.WithControlLease(ctx, lease)
	}
	return ctx, lease, nil
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
	if r.rootAdmission != nil {
		if err := r.rootAdmission.Release(); err != nil {
			return err
		}
		r.rootAdmission = nil
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
	if owner.admission.CleanupPending() || owner.childOwner.CleanupPending() || owner.useOwner.CleanupPending() || owner.resource != nil || owner.rootAdmission.CleanupPending() || (owner.ownsStore && owner.store != nil) {
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
		var root *supervisorcore.RootAdmissionCleanupError
		return errors.As(node, &root) && root.Lease().RetainsStore(owner.store)
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
