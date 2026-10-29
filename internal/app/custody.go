package app

// Lifetime custody composition. Ownership:
//   - verifiedexec.Pool owns images, views and the pool identity marker.
//   - The guardian chooses one placement under its owner lease, and the
//     create-once lifetime registration persists it. The registration is the
//     only placement authority afterwards: no reader selects again.
//   - This file composes the selection, the recorded binder and the
//     closure-to-release step. guardian.ReadClosureProof alone authorizes a
//     release; a registration without a placement predates custody and is never
//     released or bound here.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

const (
	// lifetimeCustodyDirectory is the preferred pool below the canonical state root.
	lifetimeCustodyDirectory = ".custody"
	// lifetimeCustodyTemporaryPrefix names the per-root fallback pool below the
	// temporary root, used only when the state root's filesystem is noexec.
	lifetimeCustodyTemporaryPrefix = "delegate-custody-"
	// lifetimeCustodyReleaseTimeout bounds one release or pending check made
	// after the lifetime has closed, when no business deadline applies anymore.
	lifetimeCustodyReleaseTimeout = 10 * time.Second
)

// errLifetimeCustodyUnplaced refuses a task or inspection registration without
// a custody placement. Such a registration predates custody, so binding it
// through a private copy would leave views no closure owner releases.
var errLifetimeCustodyUnplaced = fmt.Errorf("%w: lifetime registration has no custody placement", task.ErrEvidenceFault)

// lifetimeCustodySelector chooses the custody pool for one task or inspection
// guardian run. The guardian calls it at most once, after it has confirmed that
// no registration exists. Its inputs are captured once when the guardian is
// composed and never reread.
type lifetimeCustodySelector struct {
	stateRoot   string
	rootID      string
	environment []string
	cwd         string
	cwdErr      error
	deadline    time.Time
	// prepare is the pool identity owner's Prepare; tests replace it.
	prepare func(ctx context.Context, root string) (string, error)
}

// newLifetimeCustodySelector captures the guardian's environment and working
// directory once. stateRoot is the canonical state root.
func newLifetimeCustodySelector(stateRoot, rootID string, environment []string, deadline time.Time) lifetimeCustodySelector {
	cwd, cwdErr := os.Getwd()
	return lifetimeCustodySelector{
		stateRoot: stateRoot, rootID: rootID, environment: slices.Clone(environment),
		cwd: cwd, cwdErr: cwdErr, deadline: deadline, prepare: prepareLifetimeCustodyPool,
	}
}

// selectPlacement prepares the state-root pool and falls back to the temporary
// root only when every cause of that refusal is a noexec mount. Every Prepare
// is bounded by the run's immutable deadline. When neither location is usable
// the refusal names both causes.
func (s lifetimeCustodySelector) selectPlacement(ctx context.Context) (*task.LifetimeCustodyPlacement, error) {
	ctx, cancel := context.WithDeadline(ctx, s.deadline)
	defer cancel()
	preferred := filepath.Join(s.stateRoot, lifetimeCustodyDirectory)
	placement, err := s.preparePlacement(ctx, preferred)
	if err == nil || !task.AllErrorCauses(err, isPoolNoExec) {
		return placement, err
	}
	fallback, rootErr := s.temporaryPoolRoot()
	if rootErr != nil {
		return nil, errors.Join(err, rootErr)
	}
	placement, fallbackErr := s.preparePlacement(ctx, fallback)
	if fallbackErr != nil {
		return nil, errors.Join(err, fallbackErr)
	}
	return placement, nil
}

func (s lifetimeCustodySelector) preparePlacement(ctx context.Context, root string) (*task.LifetimeCustodyPlacement, error) {
	id, err := s.prepare(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("prepare custody pool %s: %w", root, err)
	}
	return &task.LifetimeCustodyPlacement{Version: task.LifetimeCustodyPlacementVersion, PoolRoot: root, PoolID: id}, nil
}

// temporaryPoolRoot resolves the captured TMPDIR, or the native default,
// against the captured working directory and canonicalizes it once.
func (s lifetimeCustodySelector) temporaryPoolRoot() (string, error) {
	temporary := verifiedexec.TemporaryRoot(s.environment)
	if !filepath.IsAbs(temporary) {
		if s.cwdErr != nil {
			return "", fmt.Errorf("resolve relative temporary root %q: %w", temporary, s.cwdErr)
		}
		temporary = filepath.Join(s.cwd, temporary)
	}
	canonical, err := config.CanonicalizePath(temporary)
	if err != nil {
		return "", fmt.Errorf("canonicalize temporary root: %w", err)
	}
	return filepath.Join(canonical, lifetimeCustodyTemporaryPrefix+s.rootID), nil
}

func prepareLifetimeCustodyPool(ctx context.Context, root string) (string, error) {
	pool, err := verifiedexec.NewPool(root)
	if err != nil {
		return "", err
	}
	return pool.Prepare(ctx)
}

func isPoolNoExec(err error) bool { return errors.Is(err, verifiedexec.ErrPoolNoExec) }

// lifetimeCustody is one registration's recorded pool and owner namespace.
type lifetimeCustody struct {
	pool      *verifiedexec.Pool
	namespace verifiedexec.Namespace
}

// recordedLifetimeCustody binds the exact recorded pool and the owner's
// namespace. The family readers already bind RootID, TaskID and OwnerKind to
// this record owner. A nil result is a registration without a placement, which
// is legal only for workspace owners and for registrations that predate custody.
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

// lifetimeCustodyBinder binds every command of one registered lifetime into the
// recorded pool. It never falls back to a private copy.
type lifetimeCustodyBinder struct {
	custody  *lifetimeCustody
	deadline time.Time
}

// lifetimeChildBinder is the worker's binding decision, made before any scope
// is opened or Hello is sent. Task and inspection registrations require a
// placement; workspace registrations require none and get no binder.
func lifetimeChildBinder(registration *task.LifetimeRegistrationRecord, deadline time.Time) (*lifetimeCustodyBinder, error) {
	custody, err := recordedLifetimeCustody(registration)
	if err != nil {
		return nil, err
	}
	if custody == nil {
		if registration.OwnerKind == task.LifetimeOwnerWorkspace {
			return nil, nil
		}
		return nil, errLifetimeCustodyUnplaced
	}
	return &lifetimeCustodyBinder{custody: custody, deadline: deadline}, nil
}

// BindCommand bounds the caller's context by the registration deadline, keeping
// any earlier deadline or cancellation the caller already has.
func (b *lifetimeCustodyBinder) BindCommand(ctx context.Context, request verifiedexec.CommandRequest) (*verifiedexec.Command, error) {
	if ctx == nil {
		return nil, errors.New("lifetime custody binding requires a context")
	}
	ctx, cancel := context.WithDeadline(ctx, b.deadline)
	defer cancel()
	return b.custody.pool.Bind(ctx, b.custody.namespace, request)
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
func releaseLifetimeCustody(ctx context.Context, records guardian.ClosureRecords) error {
	custody, closed, err := closedLifetimeCustody(ctx, records)
	if err != nil || !closed {
		return err
	}
	return custody.pool.ReleaseOwner(ctx, custody.namespace)
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

// reportLifetimeCustodyRelease releases after an operation that must not change
// its result for a custody fault. The fault is logged and retried by the next
// observer of the same records.
func reportLifetimeCustodyRelease(records guardian.ClosureRecords) {
	ctx, cancel := context.WithTimeout(context.Background(), lifetimeCustodyReleaseTimeout)
	defer cancel()
	if err := releaseLifetimeCustody(ctx, records); err != nil {
		log.Printf("delegate: lifetime custody release deferred: %v", err)
	}
}
