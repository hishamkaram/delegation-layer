package control

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// CompletedTaskCleanup is the clean disposition of one completed-task
// cleanup attempt. The zero value is no disposition and is returned only with
// an operational error.
type CompletedTaskCleanup uint8

const (
	// CompletedTaskRemoved is the exact ended row removed by this attempt.
	CompletedTaskRemoved CompletedTaskCleanup = iota + 1
	// CompletedTaskCleanupDeferred is the exact row still running; a later
	// maintenance pass retries it.
	CompletedTaskCleanupDeferred
	// CompletedTaskLogicalLabelAbsent is a verified exact-label query that
	// returned no row with the task's label. It says nothing about whether the
	// saved numeric ID is free or which actor removed the row.
	CompletedTaskLogicalLabelAbsent
)

// completedTaskCleanupAttempt is the single attempt's disposition beside its
// operational error, kept together so the legacy projection retains the
// disposition's marker even when a lock release fault is joined.
type completedTaskCleanupAttempt struct {
	disposition CompletedTaskCleanup
	err         error
}

// result masks the disposition whenever an operational error occurred.
func (attempt completedTaskCleanupAttempt) result() (CompletedTaskCleanup, error) {
	if attempt.err != nil {
		return 0, attempt.err
	}
	return attempt.disposition, nil
}

// legacyError projects the attempt to RemoveCompletedTask's error contract:
// removed is nil, deferred is ErrCleanupDeferred, and an absent label is
// ErrUnknown. An operational error is joined beside the marker.
func (attempt completedTaskCleanupAttempt) legacyError() error {
	var marker error
	switch attempt.disposition {
	case CompletedTaskRemoved:
	case CompletedTaskCleanupDeferred:
		marker = ErrCleanupDeferred
	case CompletedTaskLogicalLabelAbsent:
		marker = ErrUnknown
	}
	if attempt.err == nil {
		return marker
	}
	if marker == nil {
		return attempt.err
	}
	return errors.Join(marker, attempt.err)
}

// RemoveCompletedTask removes one exact terminal task row from the private
// supervisor. Delegate task records remain the durable history; the Pueue row
// is only an operational handle and must not be retained indefinitely.
//
// The caller must have already published a terminal outcome and provide the
// complete task identity from that task's supervisor receipt. A failed or
// uncertain removal is returned to the caller so it can retain the task for a
// later bounded cleanup attempt. It is the error projection of
// AttemptCompletedTaskCleanup.
func (c *Client) RemoveCompletedTask(ctx context.Context, identity Identity) error {
	return c.attemptCompletedTaskCleanup(ctx, identity).legacyError()
}

// AttemptCompletedTaskCleanup performs the same single cleanup attempt as
// RemoveCompletedTask and reports its clean disposition separately from any
// operational error.
func (c *Client) AttemptCompletedTaskCleanup(ctx context.Context, identity Identity) (CompletedTaskCleanup, error) {
	return c.attemptCompletedTaskCleanup(ctx, identity).result()
}

func (c *Client) attemptCompletedTaskCleanup(ctx context.Context, identity Identity) completedTaskCleanupAttempt {
	identity = snapshotIdentity(identity)
	if c == nil || !isPrivateClient(c) {
		return completedTaskCleanupAttempt{err: ErrConfiguration}
	}
	if identity.NumericTaskID == nil || *identity.NumericTaskID < 0 {
		return completedTaskCleanupAttempt{err: ErrUnknown}
	}
	controlContext, cancel, err := c.privateControlContext(ctx)
	if err != nil {
		return completedTaskCleanupAttempt{err: err}
	}
	defer cancel()
	// Pueue can reuse numeric IDs after removal. Exclude submission and other
	// removers from the identity observation through completion of removal.
	var attempt completedTaskCleanupAttempt
	attempt.err = c.withPrivateBootstrapLock(controlContext, func(lockedContext context.Context) error {
		var lockedErr error
		attempt.disposition, lockedErr = c.removeCompletedTaskLocked(lockedContext, identity)
		return lockedErr
	})
	return attempt
}

func (c *Client) removeCompletedTaskLocked(ctx context.Context, identity Identity) (CompletedTaskCleanup, error) {
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return 0, ErrConfiguration
	}
	if err := c.verify(ctx); err != nil {
		return 0, err
	}
	if err := c.authorizePrivateMutationWithLock(stateRoot); err != nil {
		return 0, err
	}
	selection, err := c.selectLabel(ctx, identity)
	if err != nil {
		return 0, err
	}
	if selection.rows == 0 {
		return CompletedTaskLogicalLabelAbsent, nil
	}
	if selection.err != nil {
		return 0, selection.err
	}
	numericTaskID := *identity.NumericTaskID
	if disposition, removable, err := completedTaskRemoval(selection.observation, numericTaskID); err != nil || !removable {
		return disposition, err
	}
	result, commandErr := c.command(ctx, nil, "remove", strconv.FormatInt(numericTaskID, 10))
	if pending := pendingFrom(commandErr); pending != nil {
		settled, waitErr := awaitPending(ctx, pending)
		result = settled
		commandErr = errors.Join(commandErr, waitErr)
	}
	if commandErr != nil {
		return 0, errors.Join(commandErr, result.Err)
	}
	if result.ExitCode != 0 {
		return 0, fmt.Errorf("%w: remove completed supervisor task %d exited with %d", ErrBinding, numericTaskID, result.ExitCode)
	}
	return CompletedTaskRemoved, nil
}

// completedTaskRemoval decides whether the exact matched row may be removed.
// A running row defers; any state other than ended stays unknown.
func completedTaskRemoval(observation Observation, numericTaskID int64) (CompletedTaskCleanup, bool, error) {
	if !observation.Matched || observation.NumericTaskID != numericTaskID {
		return 0, false, ErrUnknown
	}
	// The runner writes Delegate's terminal outcome before it exits. The
	// authoritative outcome can therefore be available while Pueue still
	// reports this exact row as running. Do not remove a live row; later
	// private maintenance will retry after the row reaches its terminal state.
	if observation.State == StateRunning {
		return CompletedTaskCleanupDeferred, false, nil
	}
	if observation.State != StateEnded {
		return 0, false, ErrUnknown
	}
	return 0, true, nil
}
