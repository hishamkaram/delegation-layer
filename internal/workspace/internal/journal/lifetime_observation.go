package journal

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ObserveLifetimeContext reads a canonical existing workspace operation using
// nonblocking leases only. The supplied reader cannot outlive its callback.
func ObserveLifetimeContext(ctx context.Context, store *taskdir.Store, id string, observe func(taskdir.ControlLifetimeObservation) error) (resultErr error) {
	if store == nil || observe == nil {
		return task.ErrInvalidPermit
	}
	control, err := store.TryOpenWorkspaceOperation(ctx, id)
	if err != nil {
		return err
	}
	defer func() { resultErr = control.CloseWithError(resultErr) }()
	owner, present, err := control.TryAcquireExistingLifetimeOwnerContext(ctx)
	if owner != nil {
		defer func() { resultErr = owner.ReleaseWithError(resultErr) }()
	}
	if err != nil {
		return err
	}
	return control.TryWithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		request, deadline, readErr := readOperationRequestTx(tx, store.RootID, id)
		if readErr != nil {
			return readErr
		}
		operation, readErr := newOperation(control, request, deadline)
		if readErr != nil {
			return readErr
		}
		records, readErr := operation.TransactionReader(tx)
		if readErr != nil {
			return readErr
		}
		preparationOnly, readErr := workspacePreparationOnlyTx(tx, operation.digest)
		if readErr != nil {
			return readErr
		}
		return observe(taskdir.ControlLifetimeObservation{Records: records, OwnerPresent: present, PreparationOnly: preparationOnly})
	})
}

func workspacePreparationOnlyTx(tx *taskdir.ControlTransaction, digest string) (bool, error) {
	names, err := tx.RecordNames()
	if err != nil {
		return false, err
	}
	for _, name := range names {
		switch name {
		case ".operation.lock", ".lifetime-owner.lock", operationRequestName:
		case "submission.json":
			if err = tx.Verify(name, operationGuard{SchemaVersion: task.SchemaVersion, RequestSHA256: digest}); err != nil {
				return false, err
			}
		default:
			return false, nil
		}
	}
	return true, nil
}
