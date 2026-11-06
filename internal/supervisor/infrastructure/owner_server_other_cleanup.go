package infrastructure

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// One accepted other-root operation owns its parent resources. Failure to close
// a borrower retains every dependency and transfers that actual owner graph in
// the error to the resident server; a reply/slot release never closes it by GC.
type otherPreparationResources struct {
	reader  *taskdir.ReadOnlyStore
	target  *taskdir.Store
	attempt *StartupAttempt
}

func (r *otherPreparationResources) finish(operationErr error) error {
	if cleanupErr := r.close(); cleanupErr != nil {
		done := make(chan struct{})
		close(done) // The synchronous Close attempt above has actually returned.
		var retained ResidentCleanupCompletion = &otherPreparationCleanupError{resources: r, cause: cleanupErr, done: done}
		return errors.Join(operationErr, retained)
	}
	return operationErr
}

func (r *otherPreparationResources) close() error {
	if r.attempt != nil {
		if err := r.attempt.Close(); err != nil {
			return err
		}
		r.attempt = nil
	}
	if r.reader != nil {
		if err := r.reader.Close(); err != nil {
			return err
		}
		r.reader = nil
	}
	if r.target != nil {
		if err := r.target.Close(); err != nil {
			return err
		}
		r.target = nil
	}
	return nil
}

type otherPreparationCleanupError struct {
	resources *otherPreparationResources
	cause     error
	done      <-chan struct{}
}

func (*otherPreparationCleanupError) Error() string {
	return "infrastructure target cleanup unresolved"
}
func (e *otherPreparationCleanupError) Unwrap() error         { return e.cause }
func (*otherPreparationCleanupError) RetainsResidentCleanup() {}
func (e *otherPreparationCleanupError) CleanupDone() <-chan struct{} {
	return e.done
}
func (e *otherPreparationCleanupError) CleanupErr() error { return e.cause }
func (e *otherPreparationCleanupError) CleanupPending() bool {
	return e.resources.attempt != nil || e.resources.reader != nil || e.resources.target != nil
}
