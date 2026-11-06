package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func finishResidentRuntime(ctx context.Context, entry *ownedResidentEntry, waits *daemonWaitOwner, cause error) error {
	pending, completed, cleanupErr := joinResidentCleanup(ctx, cause)
	cause = errors.Join(cause, cleanupErr)
	if !completed {
		return retainedResidentCompletion(entry, waits, cause)
	}
	// Callback drain alone cannot close registration: a canonical worker can
	// still finish a successful Start after its callback deadline has expired.
	waits.closeRegistration()
	if pending {
		return retainedResidentCompletion(entry, waits, errors.Join(cause, waits.join(ctx)))
	}
	outstanding, err := waits.pendingAfterDrain()
	if err != nil {
		return retainedResidentCompletion(entry, waits, errors.Join(cause, err))
	}
	if !outstanding {
		return closeResidentEntry(ctx, entry, cause)
	}
	// Serving resources can close while this same process retains the daemon's
	// sole Wait. Its durable release fence permits a successor to reuse Pueue.
	released, releaseErr := entry.releaseServingContext(ctx)
	cause = errors.Join(cause, releaseErr)
	waitErr := waits.join(ctx)
	if !released || waitErr != nil {
		return retainedResidentCompletion(entry, waits, errors.Join(cause, waitErr))
	}
	return cause
}

func retainedResidentCompletion(entry *ownedResidentEntry, waits *daemonWaitOwner, cause error) error {
	return &residentEntryCleanupError{entry: entry, waits: waits, cause: errors.Join(cause, ErrOwnerRequest)}
}

func joinResidentCleanup(ctx context.Context, cause error) (pending, completed bool, resultErr error) {
	if ctx == nil {
		return true, false, fmt.Errorf("%w: resident cleanup requires a context", ErrIdentity)
	}
	owners, invalid := residentCleanupCompletions(cause)
	resultErr, pending = invalid, invalid != nil
	completed = invalid == nil
	for _, owner := range owners {
		done := owner.CleanupDone()
		if done == nil {
			resultErr = errors.Join(resultErr, invalidResidentCleanup())
			pending = true
			completed = false
			continue
		}
		if !awaitResidentCleanup(ctx, done) {
			resultErr = errors.Join(resultErr, ctx.Err())
			pending = true
			completed = false
			continue
		}
		resultErr = errors.Join(resultErr, owner.CleanupErr())
		pending = pending || owner.CleanupPending()
	}
	return pending, completed, resultErr
}

func residentCleanupCompletions(cause error) ([]ResidentCleanupCompletion, error) {
	var owners []ResidentCleanupCompletion
	var invalid error
	seen := make(map[ResidentCleanupCompletion]struct{})
	task.AnyErrorNode(cause, func(node error) bool {
		var marker interface{ RetainsResidentCleanup() }
		if !errors.As(node, &marker) {
			return false
		}
		var owner ResidentCleanupCompletion
		if _, direct := node.(interface{ RetainsResidentCleanup() }); direct {
			if !errors.As(node, &owner) || !sameResidentCleanupNode(node, owner) {
				invalid = errors.Join(invalid, invalidResidentCleanup())
				return false
			}
		} else if !errors.As(node, &owner) {
			invalid = errors.Join(invalid, invalidResidentCleanup())
			return false
		}
		value := reflect.ValueOf(owner)
		if !value.IsValid() || !value.Comparable() || value.Kind() == reflect.Pointer && value.IsNil() {
			invalid = errors.Join(invalid, invalidResidentCleanup())
			return false
		}
		if _, duplicate := seen[owner]; !duplicate {
			seen[owner] = struct{}{}
			owners = append(owners, owner)
		}
		return false
	})
	return owners, invalid
}

// A direct marker must implement the full port itself. A nested completion
// cannot account for the outer node's unknown resources. Comparable error nodes
// use the same identity semantics as the collection's map keys.
func sameResidentCleanupNode(node error, owner ResidentCleanupCompletion) bool {
	left, right := reflect.ValueOf(node), reflect.ValueOf(owner)
	return left.IsValid() && right.IsValid() && left.Comparable() && right.Comparable() && left.Equal(right)
}

func invalidResidentCleanup() error {
	return fmt.Errorf("%w: retained resident cleanup has no valid completion owner", ErrIdentity)
}

func awaitResidentCleanup(ctx context.Context, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
	}
	select {
	case <-done:
		return true
	case <-ctx.Done():
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}
