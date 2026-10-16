package workspace

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	resourceReleaseName = "release.json"
	resourceRemovedName = "removed.json"
)

type ReleaseRecord struct {
	SchemaVersion int    `json:"schema_version"`
	WorkspaceID   string `json:"workspace_id"`
	OperationID   string `json:"operation_id"`
	Discard       bool   `json:"discard"`
}

// RemovedRecord is an immutable success fact published only after the
// coordinator has completed and verified the exact Git removal.
type RemovedRecord struct {
	SchemaVersion int    `json:"schema_version"`
	WorkspaceID   string `json:"workspace_id"`
	OperationID   string `json:"operation_id"`
}

func (r *Resource) RecordRelease(ctx context.Context, record ReleaseRecord) error {
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

func (r *Resource) Release(ctx context.Context) (record ReleaseRecord, resultErr error) {
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
	record := RemovedRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: r.request.WorkspaceID, OperationID: operationID}
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

func (r *Resource) Removed(ctx context.Context) (record RemovedRecord, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		record, err = readRemovedTx(tx, r.request.WorkspaceID)
		return err
	})
	return record, resultErr
}

func validateReleaseRecord(record ReleaseRecord, workspaceID string) error {
	if record.SchemaVersion != task.SchemaVersion || record.WorkspaceID != workspaceID {
		return task.ErrIdentityMismatch
	}
	return errors.Join(task.ValidateTaskID(record.WorkspaceID), task.ValidateTaskID(record.OperationID))
}

func readReleaseTx(tx *taskdir.ControlTransaction, workspaceID string) (ReleaseRecord, error) {
	var record ReleaseRecord
	if err := tx.Read(resourceReleaseName, &record); err != nil {
		return ReleaseRecord{}, classifyResourceRecordReadError(err)
	}
	if err := validateReleaseRecord(record, workspaceID); err != nil {
		return ReleaseRecord{}, classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(resourceReleaseName, record); err != nil {
		return ReleaseRecord{}, err
	}
	return record, nil
}

func readOptionalReleaseTx(tx *taskdir.ControlTransaction, workspaceID string) (ReleaseRecord, bool, error) {
	record, err := readReleaseTx(tx, workspaceID)
	if task.RecordAbsent(err) {
		return ReleaseRecord{}, false, nil
	}
	if err != nil {
		return ReleaseRecord{}, false, err
	}
	return record, true, nil
}

func readRemovedTx(tx *taskdir.ControlTransaction, workspaceID string) (RemovedRecord, error) {
	var record RemovedRecord
	if err := tx.Read(resourceRemovedName, &record); err != nil {
		return RemovedRecord{}, classifyResourceRecordReadError(err)
	}
	if record.SchemaVersion != task.SchemaVersion || record.WorkspaceID != workspaceID {
		return RemovedRecord{}, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	if err := errors.Join(task.ValidateTaskID(record.WorkspaceID), task.ValidateTaskID(record.OperationID)); err != nil {
		return RemovedRecord{}, classifyStoredResourceRecordError(err)
	}
	if err := validateRemovedAgainstReleaseTx(tx, record); err != nil {
		return RemovedRecord{}, err
	}
	if err := tx.Verify(resourceRemovedName, record); err != nil {
		return RemovedRecord{}, err
	}
	return record, nil
}

func validateRemovedAgainstReleaseTx(tx *taskdir.ControlTransaction, record RemovedRecord) error {
	release, err := readReleaseTx(tx, record.WorkspaceID)
	if err != nil {
		return requireResourceRecord(err)
	}
	if release.OperationID != record.OperationID {
		return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	return nil
}
