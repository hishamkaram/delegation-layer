package app

// Legacy lifetime custody cleanup. The registration is the sole authority for
// an old placement; guardian.ReadClosureProof alone authorizes its release.
// New lifetimes do not create pools or bind provider executables into them.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// lifetimeCustodyReleaseTimeout bounds a legacy release after the lifetime closes.
const lifetimeCustodyReleaseTimeout = 10 * time.Second

// lifetimeCustody is one registration's recorded pool and owner namespace.
type lifetimeCustody struct {
	pool      *verifiedexec.Pool
	namespace verifiedexec.Namespace
}

// recordedLifetimeCustody binds the exact recorded pool and the owner's
// namespace. The family readers already bind RootID, TaskID and OwnerKind to
// this record owner. A nil result is a registration without a placement, which
// is the normal form for new lifetimes and for registrations predating custody.
func recordedLifetimeCustody(registration *task.LifetimeRegistrationRecord) (*lifetimeCustody, error) {
	switch registration.OwnerKind {
	case task.LifetimeOwnerWorkspace:
		if registration.CustodyPlacement != nil {
			return nil, fmt.Errorf("%w: workspace lifetime registration has a custody placement", task.ErrEvidenceFault)
		}
		return nil, nil
	case task.LifetimeOwnerTask, task.LifetimeOwnerInspection:
	default:
		return nil, fmt.Errorf("%w: unknown lifetime owner kind %q", task.ErrEvidenceFault, registration.OwnerKind)
	}
	placement := registration.CustodyPlacement
	if placement == nil {
		return nil, nil
	}
	if err := task.ValidateLifetimeCustodyPlacement(placement); err != nil {
		return nil, fmt.Errorf("%w: %w", task.ErrEvidenceFault, err)
	}
	pool, err := verifiedexec.NewPoolWithIdentity(placement.PoolRoot, placement.PoolID)
	if err != nil {
		return nil, err
	}
	namespace, err := verifiedexec.NewNamespace(registration.RootID + "-" + registration.OwnerKind + "-" + registration.TaskID)
	if err != nil {
		return nil, err
	}
	return &lifetimeCustody{pool: pool, namespace: namespace}, nil
}

// closedLifetimeCustody returns the recorded custody of a canonically closed
// lifetime, or nil when there is nothing this owner may release yet: no
// registration, a registration without a placement, or closure still pending.
func closedLifetimeCustody(ctx context.Context, records guardian.ClosureRecords) (*lifetimeCustody, bool, error) {
	registration, err := guardian.OptionalRegistration(records.ReadLifetimeRegistrationContext(ctx))
	if err != nil || registration == nil {
		return nil, false, err
	}
	custody, err := recordedLifetimeCustody(registration)
	if err != nil || custody == nil {
		return nil, false, err
	}
	if _, err = guardian.ReadClosureProof(ctx, records); err != nil {
		if task.AllErrorCauses(err, isClosureProofPending) {
			return custody, false, nil
		}
		return nil, false, err
	}
	return custody, true, nil
}

func isClosureProofPending(err error) bool { return errors.Is(err, guardian.ErrProofPending) }

// releaseLifetimeCustody releases the recorded owner's views after canonical
// closure. Pending closure and unplaced registrations change nothing. A
// missing, replaced or aliased pool returns the pool's concrete cause and stays
// unresolved for the next attempt.
func releaseLifetimeCustody(ctx context.Context, records guardian.ClosureRecords, report func(error)) error {
	custody, closed, err := closedLifetimeCustody(ctx, records)
	if err != nil || !closed {
		return err
	}
	return custody.pool.WithFailureReporter(report).ReleaseOwner(ctx, custody.namespace)
}

// lifetimeCustodyPending reports whether the recorded owner may still hold
// views. Pending closure is pending. The check is read-only.
func lifetimeCustodyPending(ctx context.Context, records guardian.ClosureRecords) (bool, error) {
	custody, closed, err := closedLifetimeCustody(ctx, records)
	if err != nil || custody == nil {
		return false, err
	}
	if !closed {
		return true, nil
	}
	return custody.pool.ReleasePending(custody.namespace)
}
