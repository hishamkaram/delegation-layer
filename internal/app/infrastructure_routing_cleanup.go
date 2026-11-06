package app

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This invocation owns parent references only. Closing them cannot stop a
// resident or certify native closure. The caller continues owning target.
type infrastructureRoutingResources struct {
	target  *taskdir.Store
	reader  *taskdir.ReadOnlyStore
	origin  *taskdir.ReadOnlyStore
	attempt *infrastructure.StartupAttempt
}

func (r *infrastructureRoutingResources) finishPreparation(cause, probeErr error) error {
	result := r.finish(cause)
	if result != nil && probeErr != nil {
		return &infrastructureRoutingProbeError{probe: probeErr, cause: result}
	}
	return result
}

// An unselected status probe is diagnostic history. Its cause must not alter
// the pending/uncertain classification of the canonical establishment result.
type infrastructureRoutingProbeError struct {
	probe error
	cause error
}

func (e *infrastructureRoutingProbeError) Error() string {
	return e.cause.Error() + "; initial owner probe: " + e.probe.Error()
}

func (e *infrastructureRoutingProbeError) Unwrap() error { return e.cause }

func infrastructureRoutingRetainsCleanup(cause error) bool {
	return task.AnyErrorNode(cause, func(node error) bool {
		if _, retained := node.(interface{ RetainsResidentCleanup() }); retained {
			return true
		}
		retained, ok := node.(interface{ RetainedStore() *taskdir.Store })
		return ok && retained.RetainedStore() != nil
	})
}

func (r *infrastructureRoutingResources) finish(cause error) error {
	var cleanupErr error
	if r.attempt != nil {
		cleanupErr = errors.Join(cleanupErr, r.attempt.Close())
	}
	if r.origin != nil {
		cleanupErr = errors.Join(cleanupErr, r.origin.Close())
	}
	if r.reader != nil {
		cleanupErr = errors.Join(cleanupErr, r.reader.Close())
	}
	if cleanupErr != nil {
		return &infrastructureRoutingCleanupError{resources: r, cause: errors.Join(cause, cleanupErr)}
	}
	return cause
}

// Preserve the actual borrowed Store and partial cleanup graph. No automatic
// Close retry or native lifecycle authority is created by this error view.
type infrastructureRoutingCleanupError struct {
	resources *infrastructureRoutingResources
	cause     error
}

func (*infrastructureRoutingCleanupError) Error() string {
	return "infrastructure routing cleanup unresolved"
}

func (e *infrastructureRoutingCleanupError) Unwrap() error { return e.cause }

func (e *infrastructureRoutingCleanupError) RetainedStore() *taskdir.Store {
	if e == nil || e.resources == nil {
		return nil
	}
	return e.resources.target
}
