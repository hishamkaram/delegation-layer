package workspaces

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// RootQuiescence owns only borrowed resource handles and existing use-owner
// leases. Resource journals remain authoritative for work and settlement.
// Close releases leases before their resources; failed cleanup stays reachable
// through this guard. A native ControlDir.Close fault can be terminal and sticky,
// so retaining its handle does not promise that another Close will repair it.
type RootQuiescence struct {
	mu        sync.Mutex
	resources []*rootQuiescenceResource
}

type rootQuiescenceResource struct {
	resource *workspace.Resource
	owner    *taskdir.LifetimeOwnerLease
}

// AcquireRootQuiescence observes resource and checkpoint settlement without
// canceling, fencing, recovering, submitting, deleting or writing any record.
// The caller must already hold RootAdmission EX and Generation EX, in that
// order, before this function takes use-owner leases (which hold maintenance
// SH). It must retain this guard through the closing commit, release it and
// Generation before draining handlers, and keep the borrowed store open until
// cleanup resolves. Separate native task/auxiliary, queue and pending-transition
// checks remain required; this guard grants no native Stop or cleanup authority.
// The app's native auxiliary census must independently cover root-level helper
// operations unlinked from any resource, including unfinished non-checkpoint
// helpers. The checkpoint metadata scan here does not establish their closure.
//
// A crashed admission with only a durable binding and no reservation may
// quiesce. Creator/retry integration must acquire RootAdmission SH and check
// closing BEFORE PrepareDispatch calls openDispatchResource, not merely before
// Reserve; otherwise a retry could create resource state after closing.
//
// Inventory commits before resource directory/request creation. An interrupted
// creation can therefore remain incomplete and block this gate until separately
// resolved. Mismatches name the affected IDs; this guard neither repairs those
// records nor grants deletion authority.
//
// A nonnil guard, including alongside an error, remains caller-owned and must
// be closed. Acquisition errors always refuse the closing commit. A cleanly
// absent use-owner inode is accepted only with quiescent canonical state and
// the caller's root fences; it is never independently proof of closure.
func AcquireRootQuiescence(ctx context.Context, store *taskdir.Store) (guard *RootQuiescence, resultErr error) {
	if ctx == nil || store == nil {
		return nil, task.ErrInvalidPermit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids, err := rootQuiescenceResourceIDs(ctx, store)
	if err != nil {
		return nil, err
	}
	guard = &RootQuiescence{}
	defer func() {
		if resultErr != nil {
			if pendingLifetimeCleanup(resultErr, store) != nil {
				return
			}
			closeErr := guard.Close()
			resultErr = errors.Join(resultErr, closeErr)
			if closeErr == nil {
				guard = nil
			}
		}
	}()
	for _, id := range ids {
		if err = guard.acquireResource(ctx, store, id); err != nil {
			return guard, err
		}
	}
	for _, held := range guard.resources {
		if err = rootResourceQuiescent(ctx, store, held.resource); err != nil {
			return guard, fmt.Errorf("workspace %s quiescence: %w", held.resource.Request().WorkspaceID, err)
		}
	}
	if err = rootCheckpointsQuiescent(ctx, store); err != nil {
		return guard, err
	}
	return guard, ctx.Err()
}

func rootQuiescenceResourceIDs(ctx context.Context, store *taskdir.Store) ([]string, error) {
	committed, err := workspace.ResourceIDs(ctx, store)
	if err != nil {
		return nil, err
	}
	candidates, err := store.WorkspaceResourceIDs(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(committed, candidates) {
		missing := rootQuiescenceIDDifference(committed, candidates)
		orphan := rootQuiescenceIDDifference(candidates, committed)
		return nil, fmt.Errorf("%w: workspace inventory mismatch: missing directories for committed IDs %s; orphan directory IDs %s", task.ErrEvidenceFault, rootQuiescenceDiagnosticIDs(missing), rootQuiescenceDiagnosticIDs(orphan))
	}
	return committed, nil
}

// Diagnostics stay bounded even when the directory inventory is large. The
// full mismatch remains a fault; the sample never changes the census verdict.
func rootQuiescenceDiagnosticIDs(ids []string) string {
	const sampleLimit = 16
	if len(ids) <= sampleLimit {
		return fmt.Sprint(ids)
	}
	return fmt.Sprintf("%v (%d total, %d omitted)", ids[:sampleLimit], len(ids), len(ids)-sampleLimit)
}

// Both journal and physical inventories supply sorted canonical identities.
func rootQuiescenceIDDifference(source, other []string) []string {
	missing := []string{}
	for _, id := range source {
		if _, present := slices.BinarySearch(other, id); !present {
			missing = append(missing, id)
		}
	}
	return missing
}

func (g *RootQuiescence) acquireResource(ctx context.Context, store *taskdir.Store, id string) error {
	resource, err := workspace.LoadResource(ctx, store, id)
	if err != nil {
		return requiredRootQuiescenceRecord(err)
	}
	held := &rootQuiescenceResource{resource: resource}
	g.resources = append(g.resources, held)
	owner, present, err := resource.TryAcquireExistingUseOwner(ctx)
	held.owner = owner
	if useOwnerContended(err) {
		return errors.Join(workspace.ErrBusy, err)
	}
	if err != nil {
		return err
	}
	if present != (owner != nil) {
		return task.ErrInvalidPermit
	}
	return resource.ValidateInventory(ctx)
}

func (g *RootQuiescence) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var resultErr error
	for index := len(g.resources) - 1; index >= 0; index-- {
		held := g.resources[index]
		if held == nil {
			continue
		}
		if closeErr := held.close(); closeErr != nil {
			resultErr = errors.Join(resultErr, closeErr)
		} else {
			g.resources[index] = nil
		}
	}
	if resultErr == nil {
		g.resources = nil
	}
	return resultErr
}

func (h *rootQuiescenceResource) close() error {
	id := h.resource.Request().WorkspaceID
	if h.owner != nil {
		if err := h.owner.ReleaseWithError(nil); err != nil {
			return fmt.Errorf("release workspace %s use owner: %w", id, err)
		}
		h.owner = nil
	}
	if err := h.resource.Close(); err != nil {
		return fmt.Errorf("close workspace %s resource: %w", id, err)
	}
	return nil
}

func rootResourceQuiescent(ctx context.Context, store *taskdir.Store, resource *workspace.Resource) error {
	// Registration is optional after a closed failed admission or before any
	// admission. Its absence never authorizes a new provisioning attempt here.
	if _, err := resource.Registration(ctx); err != nil && !workspace.OperationAbsent(err) {
		return err
	}
	if err := rootWorkspaceUseQuiescent(ctx, store, resource); err != nil {
		return err
	}
	if err := rootWorkspaceOperationsValid(ctx, store, resource); err != nil {
		return err
	}
	if err := rootWorkspaceRemovalQuiescent(ctx, store, resource); err != nil {
		return err
	}
	return rootWorkspaceForgetQuiescent(ctx, store, resource)
}

func rootWorkspaceUseQuiescent(ctx context.Context, store *taskdir.Store, resource *workspace.Resource) error {
	latest, err := resource.LatestReservation(ctx)
	if workspace.OperationAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	closed, err := resource.ReservationClosed(ctx, latest.Sequence)
	if workspace.OperationAbsent(err) {
		return workspace.ErrBusy
	}
	if err != nil {
		return err
	}
	if closed.CaptureID != "" {
		if err = rootClosedCaptureValid(ctx, resource, latest, closed); err != nil {
			return err
		}
	}
	if closed.Disposition == "operation-completed" {
		return rootClosedCheckpointUseValid(ctx, store, resource, latest)
	}
	if closed.Disposition == "aborted-not-launched" && latest.Subject.Kind != workspace.SubjectAdmission {
		return task.ErrEvidenceFault
	}
	return rootClosedReleaseQuiescent(ctx, resource, latest)
}

func rootClosedCaptureValid(ctx context.Context, resource *workspace.Resource, latest workspace.Reservation, closed workspace.ReservationClosed) error {
	receipt, err := resource.CaptureReceipt(ctx, closed.CaptureID)
	if err != nil {
		return requiredRootQuiescenceRecord(err)
	}
	if receipt.Status != closed.Disposition {
		return task.ErrIdentityMismatch
	}
	obligation, err := resource.CaptureObligation(ctx, latest.Sequence)
	if workspace.OperationAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if receipt.Subject != obligation.Subject {
		return task.ErrIdentityMismatch
	}
	return nil
}

func rootClosedCheckpointUseValid(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, latest workspace.Reservation) (resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, latest.Subject.ID)
	if err != nil {
		return requiredRootQuiescenceRecord(err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	return requireManagedCheckpointReservation(resource, latest, operation)
}

func rootClosedReleaseQuiescent(ctx context.Context, resource *workspace.Resource, latest workspace.Reservation) error {
	if latest.Subject.Kind != workspace.SubjectOperation {
		return nil
	}
	action, err := resource.ReleaseAction(ctx, latest.Subject)
	if workspace.OperationAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	authorized, err := ReleaseAcknowledged(ctx, resource, action.Discard)
	if err != nil || !authorized {
		return err
	}
	_, err = resource.Release(ctx)
	if workspace.OperationAbsent(err) {
		return workspace.ErrBusy
	}
	return err
}

func rootWorkspaceOperationsValid(ctx context.Context, store *taskdir.Store, resource *workspace.Resource) error {
	ids, err := resource.OperationIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = rootWorkspaceOperationValid(ctx, store, resource, id); err != nil {
			return err
		}
	}
	return nil
}

func rootWorkspaceOperationValid(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, id string) (resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, id)
	if err != nil {
		return requiredRootQuiescenceRecord(err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if operation.Request().WorkspaceID != resource.Request().WorkspaceID {
		return task.ErrIdentityMismatch
	}
	return requiredRootQuiescenceRecord(validateStoredWorkspaceActionRequest(ctx, store, operation))
}

func rootWorkspaceRemovalQuiescent(ctx context.Context, store *taskdir.Store, resource *workspace.Resource) error {
	release, releaseErr := resource.Release(ctx)
	_, removedErr := resource.Removed(ctx)
	if removedErr == nil {
		return releaseErr
	}
	if !workspace.OperationAbsent(removedErr) {
		return errors.Join(releaseErr, removedErr)
	}
	if workspace.OperationAbsent(releaseErr) {
		return nil
	}
	if releaseErr != nil {
		return releaseErr
	}
	if _, err := workspaceReleaseBudget(ctx, resource, release); err != nil {
		return requiredRootQuiescenceRecord(err)
	}
	subject, err := RemovalSubject(release)
	if err != nil {
		return err
	}
	return rootWorkspaceActionQuiescent(ctx, store, resource, workspace.OperationRemove, subject)
}

func rootWorkspaceForgetQuiescent(ctx context.Context, store *taskdir.Store, resource *workspace.Resource) error {
	intent, intentErr := resource.ForgetIntent(ctx)
	_, forgottenErr := resource.Forgotten(ctx)
	if forgottenErr == nil {
		return intentErr
	}
	if !workspace.OperationAbsent(forgottenErr) {
		return errors.Join(intentErr, forgottenErr)
	}
	if workspace.OperationAbsent(intentErr) {
		return nil
	}
	if intentErr != nil {
		return intentErr
	}
	subject, err := ForgetSubject(intent)
	if err != nil {
		return err
	}
	return rootWorkspaceActionQuiescent(ctx, store, resource, workspace.OperationForget, subject)
}

func rootWorkspaceActionQuiescent(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, kind string, subject workspace.Subject) (resultErr error) {
	operation, err := latestWorkspaceOperationWithClosure(ctx, store, resource, kind, subject, readOperationProof)
	if workspace.OperationAbsent(err) {
		return workspace.ErrBusy
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if err = validateStoredWorkspaceActionRequest(ctx, store, operation); err != nil {
		return requiredRootQuiescenceRecord(err)
	}
	proof, err := readOperationProof(ctx, operation)
	if err != nil {
		return err
	}
	_, _, acceptErr := acceptedClosedWorkspaceOperation(ctx, store, operation, proof)
	if acceptErr == nil {
		// A successful helper still needs its resource's terminal receipt.
		return workspace.ErrBusy
	}
	if resolvedWorkspaceCaptureResult(acceptErr) {
		// The non-retrying coordinator cannot launch another failed attempt.
		return nil
	}
	return acceptErr
}

func rootCheckpointsQuiescent(ctx context.Context, store *taskdir.Store) error {
	ids, err := store.WorkspaceOperationIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = rootCheckpointCandidateQuiescent(ctx, store, id); err != nil {
			return fmt.Errorf("workspace operation %s quiescence: %w", id, err)
		}
	}
	return nil
}

func rootCheckpointCandidateQuiescent(ctx context.Context, store *taskdir.Store, id string) (resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, id)
	if err != nil {
		return requiredRootQuiescenceRecord(err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if operation.Request().Kind != workspace.OperationCheckpoint {
		return nil
	}
	if err = validateCheckpointCreationIntent(operation.Request()); err != nil {
		return err
	}
	if _, err = operation.CheckpointReceipt(ctx); workspace.OperationAbsent(err) {
		return workspace.ErrBusy
	} else if err != nil {
		return err
	}
	if _, err = operation.CheckpointCandidate(ctx); err != nil && !workspace.OperationAbsent(err) {
		return err
	}
	return rootCheckpointRemovalQuiescent(ctx, store, operation)
}

func rootCheckpointRemovalQuiescent(ctx context.Context, store *taskdir.Store, original *workspace.Operation) error {
	attempts, err := original.CheckpointRemovalAttempts(ctx)
	if err != nil {
		return err
	}
	if err = requireCheckpointRemovalHistory(ctx, original, attempts); err != nil {
		return err
	}
	for _, attempt := range attempts {
		removal, loadErr := loadCheckpointRemovalAttempt(ctx, store, original, attempt)
		if loadErr != nil {
			return loadErr
		}
		if err = removal.Close(); err != nil {
			return err
		}
	}
	if len(attempts) == 0 {
		return nil
	}
	_, err = original.CheckpointRemovalReceipt(ctx, attempts[len(attempts)-1])
	if workspace.OperationAbsent(err) {
		return workspace.ErrBusy
	}
	return err
}

func requiredRootQuiescenceRecord(err error) error {
	if workspace.OperationAbsent(err) {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	return err
}
