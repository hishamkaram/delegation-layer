package control

import (
	"errors"
	"log"
	"sync"
)

// BootstrapCleanupError owns the actual bootstrap descriptor until every
// pending command completes and canonical release succeeds. It grants no work.
// Done denotes the natural cleanup attempt, not successful descriptor release.
type BootstrapCleanupError struct {
	mu       sync.Mutex
	lock     *bootstrapLock
	done     chan struct{}
	err      error
	complete bool
}

func (e *BootstrapCleanupError) Error() string {
	return "private supervisor bootstrap cleanup retained"
}

func (e *BootstrapCleanupError) Done() <-chan struct{} { return e.done }

// CleanupPending reports actual descriptor custody, independently of historical
// command/release faults. It grants no operation or retry permission.
func (e *BootstrapCleanupError) CleanupPending() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lock != nil
}

// Err reports collected command/release faults. Before Done it also reports
// ErrInFlight; neither cancellation nor callback return completes cleanup.
func (e *BootstrapCleanupError) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.complete {
		return errors.Join(ErrInFlight, e.err)
	}
	return e.err
}

// Unwrap preserves the original in-flight classification when cleanup has no
// fault. Completed successful cleanup is observed through Done and Err.
func (e *BootstrapCleanupError) Unwrap() error {
	if err := e.Err(); err != nil {
		return err
	}
	return ErrInFlight
}

// Close only retries canonical descriptor cleanup after natural completion.
// It never waits for commands or admits a new operation.
func (e *BootstrapCleanupError) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.complete {
		return ErrInFlight
	}
	releaseErr := closeBootstrapLock(e.lock)
	e.err = errors.Join(e.err, releaseErr)
	e.clearReleasedLock()
	return e.err
}

// The caller holds e.mu or has not published e. The canonical lock owner alone
// records successful File.Close; an unlock fault can coexist with that release.
func (e *BootstrapCleanupError) clearReleasedLock() {
	if e.lock == nil {
		return
	}
	lock := e.lock
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		e.lock = nil
	}
}

func retainBootstrapLock(lock **bootstrapLock, pending []*Pending, report func(error)) *BootstrapCleanupError {
	if lock == nil || *lock == nil || len(pending) == 0 {
		return nil
	}
	owner := &BootstrapCleanupError{lock: *lock, done: make(chan struct{})}
	*lock = nil
	results := newBootstrapPendingResults(pending)
	results.collectSettled()
	return retainBootstrapResults(owner, results, report)
}

func retainBootstrapResults(owner *BootstrapCleanupError, results *bootstrapPendingResults, report func(error)) *BootstrapCleanupError {
	if len(results.pending) == 0 {
		owner.finish(results.err, report)
	} else {
		go func() {
			results.wait()
			owner.finish(results.err, report)
		}()
	}
	return owner
}

func (e *BootstrapCleanupError) finish(commandErr error, report func(error)) {
	e.mu.Lock()
	releaseErr := closeBootstrapLock(e.lock)
	e.err = errors.Join(commandErr, releaseErr)
	e.clearReleasedLock()
	e.complete = true
	close(e.done)
	e.mu.Unlock()
	if releaseErr != nil && report != nil {
		report(releaseErr)
	}
}

func finishPrivateBootstrap(lock **bootstrapLock, resultErr *error) {
	results := newBootstrapPendingResults(pendingHandles(*resultErr))
	results.collectSettled()
	*resultErr = errors.Join(*resultErr, results.err)
	if lock != nil && *lock != nil && len(results.pending) > 0 {
		owner := &BootstrapCleanupError{lock: *lock, done: make(chan struct{})}
		*lock = nil
		*resultErr = errors.Join(*resultErr, retainBootstrapResults(owner, results, reportBootstrapCleanup))
		return
	}
	if releaseErr := closeBootstrapLock(*lock); releaseErr != nil {
		owner := &BootstrapCleanupError{lock: *lock, done: make(chan struct{}), err: releaseErr, complete: true}
		owner.clearReleasedLock()
		close(owner.done)
		*lock = nil
		*resultErr = errors.Join(*resultErr, owner)
		reportBootstrapCleanup(releaseErr)
	}
}

func reportBootstrapCleanup(err error) {
	log.Printf("delegate: retained supervisor lock cleanup failed: %v", err)
}
