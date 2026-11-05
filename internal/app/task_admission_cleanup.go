package app

import (
	"errors"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var errTaskAdmissionCleanupUnresolved = errors.New("task admission cleanup remains unresolved")

// Projection transfers the workflow's exact unresolved cleanup handles to the
// command error. It neither releases them nor creates another resource owner.
type taskAdmissionCleanupError struct {
	cause error
	lease *taskdir.RootAdmissionLease
	store *taskdir.Store
}

func (e *taskAdmissionCleanupError) Error() string { return e.cause.Error() }

func (e *taskAdmissionCleanupError) Unwrap() error { return e.cause }

func retainTaskAdmissionCleanup(result *commandResult, lease *taskdir.RootAdmissionLease, store *taskdir.Store, releaseErr error) {
	if releaseErr != nil {
		if !errors.Is(result.err, releaseErr) {
			result.err = errors.Join(result.err, releaseErr)
		}
		result.code = 1
		result.response.Failure = nil
		result.response.setError(result.err)
	}
	if lease == nil && store == nil && releaseErr == nil {
		return
	}
	if result.err == nil {
		result.err = errTaskAdmissionCleanupUnresolved
		result.code = 1
		result.response.Failure = nil
	}
	if lease != nil || store != nil {
		result.err = &taskAdmissionCleanupError{cause: result.err, lease: lease, store: store}
	}
	if continuation := result.response.Continuation; continuation != nil {
		continuation.Resumable = false
		continuation.ContinueCommand = ""
	}
	result.response.setError(result.err)
}

func cleanupRetainsStore(cause error, store *taskdir.Store) bool {
	if supervisorcore.CleanupRetainsStore(cause, store) {
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
