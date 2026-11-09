package taskdir

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ReadNeverStartedTaskContext borrows only existing maintenance, task and owner
// locks to reuse the canonical permanent-refusal proof. Task storage owns all
// authority; a missing or busy owner is unproven, not permission to create it.
func (r *ReadOnlyStore) ReadNeverStartedTaskContext(ctx context.Context, id string) (proven bool, reference string, resultErr error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return false, "", err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return false, "", err
	}
	maintenance, err := openExistingStoreMaintenanceLock(ctx, r.store)
	if err != nil {
		return false, "", err
	}
	// This borrow does not copy mutexes or root ownership. All methods below
	// close only their existing lock handles; r retains the rooted reader.
	borrowed := &Store{Root: r.store.Root, RootID: r.store.RootID, rootHandle: r.store.rootHandle, predicates: r.store.predicates, maintLock: maintenance, faultInjector: r.store.faultInjector}
	maintenance.faultSource = borrowed.FaultInjector
	td, err := borrowed.newTaskHandle(id, false)
	defer func() {
		if td != nil {
			resultErr = errors.Join(resultErr, td.Close())
		}
		if CleanupRetainsStore(resultErr, borrowed) {
			r.proofCleanup = &readOnlyProofCleanup{store: borrowed, task: td, cause: resultErr}
			return
		}
		resultErr = errors.Join(resultErr, maintenance.closeIdle())
	}()
	if err != nil {
		return false, "", err
	}
	proven, reference, resultErr = td.ReadUnlaunchedTaskReferenceContext(ctx)
	return proven, reference, resultErr
}

// This reader retains the exact borrowed store while a canonical cleanup error
// still owns one of its locks. Close never destroys the ancestor root early.
type readOnlyProofCleanup struct {
	store *Store
	task  *TaskDir
	cause error
}

func (r *ReadOnlyStore) closeProofCleanup() error {
	pending := r.proofCleanup
	if pending == nil {
		return nil
	}
	if CleanupRetainsStore(pending.cause, pending.store) {
		return pending.cause
	}
	var taskErr error
	if pending.task != nil {
		taskErr = pending.task.Close()
	}
	if err := errors.Join(taskErr, pending.store.maintLock.closeIdle()); err != nil {
		return errors.Join(pending.cause, err)
	}
	r.proofCleanup = nil
	return nil
}
