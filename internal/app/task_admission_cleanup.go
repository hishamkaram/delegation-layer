package app

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var errTaskAdmissionCleanupUnresolved = errors.New("task admission cleanup remains unresolved")

// Projection transfers the workflow's exact unresolved cleanup handles to the
// command error. It neither releases them nor creates another resource owner.
type taskAdmissionCleanupError struct {
	cause error
	store *taskdir.Store
}

func (e *taskAdmissionCleanupError) Error() string { return e.cause.Error() }

func (e *taskAdmissionCleanupError) Unwrap() error { return e.cause }

func retainTaskAdmissionCleanup(result *commandResult, store *taskdir.Store) {
	if store == nil {
		return
	}
	if result.err == nil {
		result.err = errTaskAdmissionCleanupUnresolved
		result.code = 1
		result.response.Failure = nil
	}
	result.err = &taskAdmissionCleanupError{cause: result.err, store: store}
	if continuation := result.response.Continuation; continuation != nil {
		continuation.Resumable = false
		continuation.ContinueCommand = ""
	}
	result.response.setError(result.err)
}

func cleanupRetainsStore(cause error, store *taskdir.Store) bool {
	if taskdir.CleanupRetainsStore(cause, store) {
		return true
	}
	// Keep the application's existing virtual workspace-owner error view.
	return task.AnyErrorNode(cause, func(node error) bool {
		var workspaceCleanup *workspaceCommandCleanupError
		return errors.As(node, &workspaceCleanup) && workspaceCleanup != nil && workspaceCleanup.RetainedStore() == store
	})
}

func closeObservationStore(result *commandResult, store *taskdir.Store) {
	if !cleanupRetainsStore(result.err, store) {
		mergeCommandClose(result, store.Close)
	}
}
