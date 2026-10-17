package journal

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

const (
	resourceReleaseName = "release.json"
	resourceRemovedName = "removed.json"
)

func (r *Resource) RecordRelease(ctx context.Context, record contract.ReleaseRecord) error {
	if err := validateReleaseRecord(record, r.request.WorkspaceID); err != nil {
		return err
	}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if _, err := tx.Put(resourceReleaseName, record); err != nil {
			return err
		}
		return tx.Verify(resourceReleaseName, record)
	})
}

func (r *Resource) Release(ctx context.Context) (record contract.ReleaseRecord, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		record, err = readReleaseTx(tx, r.request.WorkspaceID)
		return err
	})
	return record, resultErr
}

// RecordRemoved publishes only the terminal removal receipt. It performs no
// filesystem or Git operation and never deletes capture evidence.
func (r *Resource) RecordRemoved(ctx context.Context, operationID string) error {
	if err := task.ValidateTaskID(operationID); err != nil {
		return err
	}
	record := contract.RemovedRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: r.request.WorkspaceID, OperationID: operationID}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := validateRemovedAgainstReleaseTx(tx, record); err != nil {
			return err
		}
		if _, err := tx.Put(resourceRemovedName, record); err != nil {
			return err
		}
		return tx.Verify(resourceRemovedName, record)
	})
}

func (r *Resource) Removed(ctx context.Context) (record contract.RemovedRecord, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		record, err = readRemovedTx(tx, r.request.WorkspaceID)
		return err
	})
	return record, resultErr
}

func validateReleaseRecord(record contract.ReleaseRecord, workspaceID string) error {
	if record.SchemaVersion != task.SchemaVersion || record.WorkspaceID != workspaceID {
		return task.ErrIdentityMismatch
	}
	return errors.Join(task.ValidateTaskID(record.WorkspaceID), task.ValidateTaskID(record.OperationID))
}

func readReleaseTx(tx *taskdir.ControlTransaction, workspaceID string) (contract.ReleaseRecord, error) {
	var record contract.ReleaseRecord
	if err := tx.Read(resourceReleaseName, &record); err != nil {
		return contract.ReleaseRecord{}, classifyResourceRecordReadError(err)
	}
	if err := validateReleaseRecord(record, workspaceID); err != nil {
		return contract.ReleaseRecord{}, classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(resourceReleaseName, record); err != nil {
		return contract.ReleaseRecord{}, err
	}
	return record, nil
}

func readOptionalReleaseTx(tx *taskdir.ControlTransaction, workspaceID string) (contract.ReleaseRecord, bool, error) {
	record, err := readReleaseTx(tx, workspaceID)
	if task.RecordAbsent(err) {
		return contract.ReleaseRecord{}, false, nil
	}
	if err != nil {
		return contract.ReleaseRecord{}, false, err
	}
	return record, true, nil
}

func readRemovedTx(tx *taskdir.ControlTransaction, workspaceID string) (contract.RemovedRecord, error) {
	var record contract.RemovedRecord
	if err := tx.Read(resourceRemovedName, &record); err != nil {
		return contract.RemovedRecord{}, classifyResourceRecordReadError(err)
	}
	if record.SchemaVersion != task.SchemaVersion || record.WorkspaceID != workspaceID {
		return contract.RemovedRecord{}, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	if err := errors.Join(task.ValidateTaskID(record.WorkspaceID), task.ValidateTaskID(record.OperationID)); err != nil {
		return contract.RemovedRecord{}, classifyStoredResourceRecordError(err)
	}
	if err := validateRemovedAgainstReleaseTx(tx, record); err != nil {
		return contract.RemovedRecord{}, err
	}
	if err := tx.Verify(resourceRemovedName, record); err != nil {
		return contract.RemovedRecord{}, err
	}
	return record, nil
}

func validateRemovedAgainstReleaseTx(tx *taskdir.ControlTransaction, record contract.RemovedRecord) error {
	release, err := readReleaseTx(tx, record.WorkspaceID)
	if err != nil {
		return requireResourceRecord(err)
	}
	if release.OperationID != record.OperationID {
		return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	return nil
}
