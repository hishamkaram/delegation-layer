package control

import (
	"errors"
	"fmt"
	"log"
	"sync"
)

// BootstrapCleanupError owns bootstrap cleanup until pending commands settle.
// Done denotes a completed close attempt, not proven native release. A release
// fault requires fail-stop handling even when CleanupPending is false.
type BootstrapCleanupError struct {
	mu         sync.Mutex
	lock       *bootstrapLock
	done       chan struct{}
	commandErr error
	releaseErr error
	complete   bool
}

func (e *BootstrapCleanupError) Error() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.complete {
		return "private supervisor bootstrap cleanup pending"
	}
	if e.releaseErr != nil {
		return fmt.Sprintf("private supervisor bootstrap cleanup completed with release fault: %v", e.releaseErr)
	}
	return "private supervisor bootstrap cleanup completed"
}

func (e *BootstrapCleanupError) Done() <-chan struct{} { return e.done }

// CleanupPending reports custody of a usable Go descriptor. False does not
// prove native closure: Err must still be checked for a release fault.
func (e *BootstrapCleanupError) CleanupPending() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lock != nil
}

// Err exposes both command diagnostics and release faults to the cleanup owner.
// Neither callback return nor cancellation completes pending work.
func (e *BootstrapCleanupError) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.complete {
		return errors.Join(ErrInFlight, e.commandErr, e.releaseErr)
	}
	return errors.Join(e.commandErr, e.releaseErr)
}

// Unwrap preserves legacy supervisor uncertainty. Command outcomes are available
// only through Err; only a release fault changes the callback error policy.
func (e *BootstrapCleanupError) Unwrap() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.releaseErr != nil {
		return e.releaseErr
	}
	return ErrInFlight
}

// Close is nonwaiting cleanup status. Natural cleanup owns the sole close
// attempt; an attempted os.File.Close cannot be retried as a usable handle.
func (e *BootstrapCleanupError) Close() error { return e.Err() }

func retainBootstrapLock(lock **bootstrapLock, pending []*Pending, report func(error)) *BootstrapCleanupError {
	results := newBootstrapPendingResults(pending)
	results.collectSettled()
	owner := &BootstrapCleanupError{done: make(chan struct{}), commandErr: results.err}
	if lock != nil && *lock != nil {
		retained := *lock
		retained.mu.Lock()
		retained.retired = true
		retained.mu.Unlock()
		owner.lock, *lock = retained, nil
	}
	if len(results.pending) == 0 {
		owner.finish(results.err, report)
		if owner.Err() == nil {
			return nil
		}
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
	e.commandErr, e.releaseErr = commandErr, releaseErr
	// A completed close attempt retires the Go handle, including on failure.
	// The release fault remains authoritative; no native-success claim follows.
	e.lock = nil
	e.complete = true
	close(e.done)
	e.mu.Unlock()
	if releaseErr != nil && report != nil {
		report(releaseErr)
	}
}

func finishPrivateBootstrap(lock **bootstrapLock, resultErr *error) {
	if owner := retainBootstrapLock(lock, pendingHandles(*resultErr), reportBootstrapCleanup); owner != nil {
		*resultErr = errors.Join(*resultErr, owner)
	}
}

func reportBootstrapCleanup(err error) {
	log.Printf("delegate: retained supervisor lock cleanup failed: %v", err)
}
