package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

type rootTaskClosureBusyError struct{}

func (rootTaskClosureBusyError) Error() string { return "root task closure is unproven" }

var errRootTaskClosureBusy = rootTaskClosureBusyError{}

// checkRootTaskClosure composes the journals' existing read-only closure policy.
// The caller must hold the root admission exclusive fence and retain its root
// pin throughout the census and subsequent supervisor stop. Those owners must
// exclude new tasks and auxiliary operations; this function acquires neither.
// Success is only a census verdict, never authority for later cleanup. It does
// not fence launch, cancel, recover, or change any durable lifecycle evidence.
func checkRootTaskClosure(ctx context.Context, store *taskdir.Store) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if store == nil {
		return task.ErrEvidenceFault
	}
	ids, err := store.TaskIDs(ctx)
	if err != nil {
		return rootClosureError("task census", "", err)
	}
	for _, id := range ids {
		if err = checkRootOrdinaryTaskClosure(ctx, store, id); err != nil {
			return rootClosureError("task", id, err)
		}
	}
	ids, err = store.InspectionOperationIDs(ctx)
	if err != nil {
		return rootClosureError("inspection census", "", err)
	}
	for _, id := range ids {
		if err = checkRootInspectionClosure(ctx, store, id); err != nil {
			return rootClosureError("inspection", id, err)
		}
	}
	ids, err = store.WorkspaceOperationIDs(ctx)
	if err != nil {
		return rootClosureError("workspace census", "", err)
	}
	for _, id := range ids {
		if err = checkRootWorkspaceClosure(ctx, store, id); err != nil {
			return rootClosureError("workspace operation", id, err)
		}
	}
	return ctx.Err()
}

func checkRootOrdinaryTaskClosure(ctx context.Context, store *taskdir.Store, id string) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	td, err := store.OpenTask(id)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	_, err = guardian.ReadClosureProof(ctx, td)
	if !task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, guardian.ErrProofPending) }) {
		return err
	}
	proven, _, err := td.ReadUnlaunchedTaskReferenceContext(ctx)
	if err != nil {
		return err
	}
	if !proven {
		return guardian.ErrProofPending
	}
	return nil
}

func checkRootInspectionClosure(ctx context.Context, store *taskdir.Store, id string) (resultErr error) {
	operation, err := inspection.LoadOperationContext(ctx, store, id)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	_, resultErr = guardian.ReadAuxiliaryClosureProof(ctx, operation)
	return resultErr
}

func checkRootWorkspaceClosure(ctx context.Context, store *taskdir.Store, id string) (resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, id)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	_, resultErr = guardian.ReadAuxiliaryClosureProof(ctx, operation)
	return resultErr
}

func rootClosureError(kind, id string, err error) error {
	if task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, guardian.ErrProofPending) || errors.Is(cause, task.ErrLockBusy) || task.RecordAbsent(cause)
	}) {
		return fmt.Errorf("%w: %s %s: %w", errRootTaskClosureBusy, kind, id, err)
	}
	return fmt.Errorf("root closure: %s %s: %w", kind, id, err)
}
