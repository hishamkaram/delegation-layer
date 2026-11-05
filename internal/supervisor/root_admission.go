package supervisor

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// AcquireOrBorrowRootAdmissionContext acquires the outer ordinary-admission SH
// resource, or validates the exact caller-owned resource without reacquiring SH.
// The caller owns nonnil newly acquired results even on error; borrowed leases
// remain owned by the caller that supplied them. Store must outlive that owner.
// The finite acquisition context is never used as the later actuation context.
func AcquireOrBorrowRootAdmissionContext(ctx context.Context, store *taskdir.Store, borrowed *taskdir.RootAdmissionLease, controlTimeout time.Duration) (*taskdir.RootAdmissionLease, error) {
	if ctx == nil || store == nil || controlTimeout <= 0 {
		return nil, os.ErrInvalid
	}
	if borrowed != nil {
		if _, err := borrowed.PinRootContext(ctx); err != nil {
			return nil, err
		}
		if err := borrowed.CheckRootContext(ctx, store); err != nil {
			return nil, err
		}
		return borrowed, nil
	}
	acquisition, cancel := context.WithTimeout(ctx, controlTimeout)
	lease, err := store.AcquireRootAdmissionContext(acquisition)
	cancel()
	if err != nil {
		return lease, err
	}
	if _, err = lease.PinRootContext(ctx); err != nil {
		closeErr := lease.Release()
		if closeErr != nil {
			return lease, errors.Join(err, closeErr)
		}
		return nil, err
	}
	return lease, nil
}

// RootAdmissionCleanupError owns a newly acquired lease whose Release failed.
// Its original borrowed Store must stay open until Lease().Release succeeds.
// The error confers no admission permission; it preserves cleanup ownership.
type RootAdmissionCleanupError struct {
	lease *taskdir.RootAdmissionLease
	cause error
}

func (e *RootAdmissionCleanupError) Error() string {
	if e == nil || e.cause == nil {
		return "root admission cleanup unresolved"
	}
	return "root admission cleanup unresolved: " + e.cause.Error()
}

func (e *RootAdmissionCleanupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *RootAdmissionCleanupError) Lease() *taskdir.RootAdmissionLease {
	if e == nil {
		return nil
	}
	return e.lease
}

// ReleaseOwnedRootAdmission releases only a newly acquired caller-owned lease.
// Borrowers must not call it. Any failed-release handle remains reachable from
// the returned error, alongside all original work and native release causes.
// An unresolved child keeps the parent held until explicit child-first cleanup.
func ReleaseOwnedRootAdmission(lease *taskdir.RootAdmissionLease, cause error) error {
	if retainsRootAdmission(cause, lease) {
		return cause
	}
	if lease != nil && lease.CleanupPending() && CleanupHasRetainedStore(cause) {
		return RetainOwnedRootAdmission(lease, cause)
	}
	if lease != nil && lease.CleanupPending() && task.AnyErrorNode(cause, func(node error) bool {
		var child *taskdir.LifetimeOwnerCleanupError
		return errors.As(node, &child) && child.Lease().CleanupPending()
	}) {
		return RetainOwnedRootAdmission(lease, cause)
	}
	if releaseErr := lease.Release(); releaseErr != nil {
		return &RootAdmissionCleanupError{lease: lease, cause: errors.Join(cause, releaseErr)}
	}
	return cause
}

// RetainOwnedRootAdmission preserves a caller-owned parent behind a failed
// child cleanup without retrying native release. The caller settles the child,
// then explicitly releases this lease before closing its original Store.
// Borrowed leases remain with their existing owner and must not be passed here.
func RetainOwnedRootAdmission(lease *taskdir.RootAdmissionLease, cause error) error {
	if lease == nil {
		return cause
	}
	if retainsRootAdmission(cause, lease) {
		return cause
	}
	if cause == nil {
		cause = os.ErrInvalid
	}
	return &RootAdmissionCleanupError{lease: lease, cause: cause}
}

func retainsRootAdmission(cause error, lease *taskdir.RootAdmissionLease) bool {
	return task.AnyErrorNode(cause, func(node error) bool {
		var retained *RootAdmissionCleanupError
		return errors.As(node, &retained) && retained != nil && retained.lease == lease
	})
}
