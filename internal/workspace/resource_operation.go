package workspace

import (
	"context"
	"errors"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type OperationReference struct {
	SchemaVersion int    `json:"schema_version"`
	WorkspaceID   string `json:"workspace_id"`
	OperationID   string `json:"operation_id"`
	Kind          string `json:"kind"`
}

const operationRecordPrefix = "operation-"

// RecordOperation binds the workspace to an immutable auxiliary request.
// The pointer stores only the operation identity and kind; the full request
// remains authoritative in its operation journal.
func (r *Resource) RecordOperation(ctx context.Context, operation OperationRequest) error {
	if _, err := validateOperation(operation, r.request.RootID); err != nil {
		return err
	}
	if operation.WorkspaceID != r.request.WorkspaceID {
		return task.ErrIdentityMismatch
	}
	reference := OperationReference{
		SchemaVersion: task.SchemaVersion,
		WorkspaceID:   operation.WorkspaceID,
		OperationID:   operation.OperationID,
		Kind:          operation.Kind,
	}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if _, err := tx.Put(operationRecordName(operation.OperationID), reference); err != nil {
			return err
		}
		return tx.Verify(operationRecordName(operation.OperationID), reference)
	})
}

// OperationIDs enumerates only operation records whose decoded identities and
// canonical bytes validate; filenames are indexes, never authority.
func (r *Resource) OperationIDs(ctx context.Context) (ids []string, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		names, err := r.control.RecordNamesContext(ctx)
		if err != nil {
			return err
		}
		for _, name := range names {
			id, candidate, parseErr := validateIDName(name, operationRecordPrefix, ".json")
			if parseErr != nil {
				return parseErr
			}
			if !candidate {
				continue
			}
			reference, readErr := readOperationReferenceTx(tx, r.request.WorkspaceID, id)
			if readErr != nil {
				return requireResourceRecord(readErr)
			}
			if reference.OperationID != id {
				return task.ErrIdentityMismatch
			}
			ids = append(ids, id)
		}
		return nil
	})
	if resultErr != nil {
		return nil, resultErr
	}
	slices.Sort(ids)
	return ids, nil
}

func operationRecordName(operationID string) string {
	return operationRecordPrefix + operationID + ".json"
}

func readOperationReferenceTx(tx *taskdir.ControlTransaction, workspaceID, operationID string) (OperationReference, error) {
	if task.ValidateTaskID(operationID) != nil {
		return OperationReference{}, task.ErrIdentityMismatch
	}
	var reference OperationReference
	if err := tx.Read(operationRecordName(operationID), &reference); err != nil {
		return OperationReference{}, classifyResourceRecordReadError(err)
	}
	if err := validateOperationReference(reference, workspaceID, operationID); err != nil {
		return OperationReference{}, classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(operationRecordName(operationID), reference); err != nil {
		return OperationReference{}, err
	}
	return reference, nil
}

func validateOperationReference(reference OperationReference, workspaceID, operationID string) error {
	if reference.SchemaVersion != task.SchemaVersion || reference.WorkspaceID != workspaceID || reference.OperationID != operationID {
		return task.ErrIdentityMismatch
	}
	if err := errors.Join(task.ValidateTaskID(reference.WorkspaceID), task.ValidateTaskID(reference.OperationID)); err != nil {
		return err
	}
	switch reference.Kind {
	case OperationProvision, OperationCapture, OperationVerify, OperationRemove, OperationForget:
		return nil
	default:
		return task.ErrIdentityMismatch
	}
}
