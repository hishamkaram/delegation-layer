package workspaces

import (
	"errors"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The producer owns the failed unwind. Its returned lease is cleanup-only;
// readers may retain it but cannot retry it as part of ordinary finalization.
func pendingLifetimeCleanup(cause error, store *taskdir.Store) *taskdir.LifetimeOwnerLease {
	var pending *taskdir.LifetimeOwnerLease
	task.AnyErrorNode(cause, func(node error) bool {
		var cleanup *taskdir.LifetimeOwnerCleanupError
		if !errors.As(node, &cleanup) || !cleanup.Lease().RetainsStore(store) {
			return false
		}
		pending = cleanup.Lease()
		return true
	})
	return pending
}

// Admission owns the retained references; each exact lease remains the owner
// of its native pending state. Only explicit Close mutates those resources.
func (m *Admission) retainLifetimeCleanup(store *taskdir.Store, cause error) error {
	retained := false
	task.AnyErrorNode(cause, func(node error) bool {
		var child interface{ RetainedStore() *taskdir.Store }
		if errors.As(node, &child) && child != nil && child.RetainedStore() == store {
			retained = true
			m.storeCleanup = errors.Join(m.storeCleanup, node)
		}
		var cleanup *taskdir.LifetimeOwnerCleanupError
		if errors.As(node, &cleanup) && cleanup.Lease().RetainsStore(store) {
			retained = true
			child := cleanup.Lease()
			if child != m.owner && !slices.Contains(m.childOwners, child) {
				m.childOwners = append(m.childOwners, child)
			}
		}
		return false
	})
	if !retained {
		return nil
	}
	return &AdmissionCleanupError{admission: m, cause: cause}
}

func (m *Admission) storeCleanupPending() bool {
	return m != nil && task.AnyErrorNode(m.storeCleanup, func(node error) bool {
		var child interface{ RetainedStore() *taskdir.Store }
		return errors.As(node, &child) && child != nil && child.RetainedStore() != nil
	})
}

func (m *Admission) closeAfter(store *taskdir.Store, cause error) error {
	if retained := m.retainLifetimeCleanup(store, cause); retained != nil {
		return retained
	}
	return m.Close()
}
