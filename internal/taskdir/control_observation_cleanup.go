package taskdir

import (
	"errors"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ControlObservationCleanupError retains only failed storage cleanup. Release
// retries the exact borrowed leases/handle; it never authorizes record mutation
// or another operation. Its original work and release failures remain visible.
type ControlObservationCleanupError struct {
	mu              sync.Mutex
	control         *ControlDir
	operationHeld   bool
	maintenanceHeld bool
	closeControl    bool
	ancestors       []*connectorAncestors
	cause           error
}

func (e *ControlObservationCleanupError) Error() string { return e.cause.Error() }
func (e *ControlObservationCleanupError) Unwrap() error { return e.cause }

func (e *ControlObservationCleanupError) RetainedStore() *Store {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending() {
		return e.control.store
	}
	return nil
}

func (e *ControlObservationCleanupError) pending() bool {
	return e.operationHeld || e.maintenanceHeld || e.closeControl || len(e.ancestors) != 0
}

func (e *ControlObservationCleanupError) retainAncestor(ancestor *connectorAncestors) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ancestors = append(e.ancestors, ancestor)
}

func (e *ControlObservationCleanupError) requestClose(control *ControlDir) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.control != control || !e.pending() {
		return false
	}
	e.closeControl = true
	return true
}

// Release retries only retained cleanup. An external retry that requests
// ControlDir.Close must run after the original transaction has returned;
// calling it while owning a transaction on that same control would self-wait.
func (e *ControlObservationCleanupError) Release() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closeControl {
		if err := e.control.Close(); err != nil {
			return err
		}
		e.closeControl, e.operationHeld = false, false
	}
	if e.operationHeld {
		// This transaction owns the operation lease until its deferred end.
		// Close may already be waiting for that end, so release our own
		// lease directly; LockFile serializes this with an external close.
		if err := e.control.lock.Unlock(); err != nil {
			return err
		}
		e.operationHeld = false
	}
	if e.maintenanceHeld {
		if err := e.control.store.maintLock.Unlock(); err != nil {
			return err
		}
		e.maintenanceHeld = false
	}
	for len(e.ancestors) != 0 {
		if err := e.ancestors[0].release(); err != nil {
			return err
		}
		e.ancestors = e.ancestors[1:]
	}
	return nil
}

func (e *ControlObservationCleanupError) releaseWithError(cause error) error {
	if err := e.Release(); err != nil {
		e.cause = errors.Join(cause, err)
		return e
	}
	return cause
}

// CloseWithError closes this owned observer, preserving an existing cleanup
// transfer without implicitly retrying it. A new close fault retains this exact
// handle and its Store until the returned cleanup owner's Release succeeds.
func (d *ControlDir) CloseWithError(cause error) error {
	retained := task.AnyErrorNode(cause, func(node error) bool {
		var cleanup *ControlObservationCleanupError
		return errors.As(node, &cleanup) && cleanup != nil && cleanup.requestClose(d)
	})
	if retained {
		return cause
	}
	if err := d.Close(); err != nil {
		return &ControlObservationCleanupError{control: d, closeControl: true, cause: errors.Join(cause, err)}
	}
	return cause
}
