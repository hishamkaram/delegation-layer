package journal

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

const (
	resourceForgetIntentName = "forget-intent.json"
	resourceForgottenName    = "forgotten.json"
)

// RecordForgetIntent creates the exact deletion inventory once. No ref or
// artifact is removed by this method.
func (r *Resource) RecordForgetIntent(ctx context.Context, intent contract.ForgetIntent) error {
	if err := validateForgetIntent(intent, r.request.WorkspaceID); err != nil {
		return err
	}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if _, err := tx.Put(resourceForgetIntentName, intent); err != nil {
			return err
		}
		return tx.Verify(resourceForgetIntentName, intent)
	})
}

func (r *Resource) ForgetIntent(ctx context.Context) (intent contract.ForgetIntent, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		intent, err = readForgetIntentTx(tx, r.request.WorkspaceID)
		return err
	})
	return intent, resultErr
}

// RecordForgotten publishes the terminal tombstone after the coordinator has
// completed every listed deletion. Storage itself never deletes inventory.
func (r *Resource) RecordForgotten(ctx context.Context, operationID string) error {
	if err := task.ValidateTaskID(operationID); err != nil {
		return err
	}
	record := contract.ForgottenRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: r.request.WorkspaceID, OperationID: operationID}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := validateForgottenAgainstIntentTx(tx, record); err != nil {
			return err
		}
		if _, err := tx.Put(resourceForgottenName, record); err != nil {
			return err
		}
		return tx.Verify(resourceForgottenName, record)
	})
}

func (r *Resource) Forgotten(ctx context.Context) (record contract.ForgottenRecord, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		record, err = readForgottenTx(tx, r.request.WorkspaceID)
		return err
	})
	return record, resultErr
}

func validateForgetIntent(intent contract.ForgetIntent, workspaceID string) error {
	if intent.SchemaVersion != task.SchemaVersion || intent.WorkspaceID != workspaceID {
		return task.ErrIdentityMismatch
	}
	deadline, err := time.Parse(time.RFC3339Nano, intent.Deadline)
	if intent.BudgetNanos <= 0 || err != nil || deadline.IsZero() || deadline.UTC().Format(time.RFC3339Nano) != intent.Deadline {
		return task.ErrIdentityMismatch
	}
	if err := errors.Join(task.ValidateTaskID(intent.WorkspaceID), task.ValidateTaskID(intent.OperationID)); err != nil {
		return err
	}
	seenRefs := make(map[string]struct{}, len(intent.Refs))
	for _, reference := range intent.Refs {
		if err := contract.ValidateOwnedRef(reference.Ref); err != nil {
			return err
		}
		if _, exists := seenRefs[reference.Ref]; exists {
			return task.ErrIdentityMismatch
		}
		seenRefs[reference.Ref] = struct{}{}
		if err := validateForgetOID(reference.ExpectedOID); err != nil {
			return err
		}
	}
	seenArtifacts := make(map[string]struct{}, len(intent.Artifacts))
	for _, artifact := range intent.Artifacts {
		if err := validateForgetArtifact(artifact); err != nil {
			return err
		}
		key := artifact.OperationID + "\x00" + artifact.Name
		if _, exists := seenArtifacts[key]; exists {
			return task.ErrIdentityMismatch
		}
		seenArtifacts[key] = struct{}{}
	}
	return nil
}

func validateForgetArtifact(artifact contract.ForgetArtifact) error {
	if err := task.ValidateTaskID(artifact.OperationID); err != nil {
		return err
	}
	if err := contract.ValidateArtifactName(artifact.Name); err != nil {
		return err
	}
	if artifact.Length < 0 || artifact.Length > contract.ArtifactLimit {
		return task.ErrIdentityMismatch
	}
	return task.ValidateSHA256(artifact.SHA256)
}

func validateForgetOID(oid string) error {
	switch len(oid) {
	case 40:
		return contract.ValidateOID(oid, "sha1")
	case 64:
		return contract.ValidateOID(oid, "sha256")
	default:
		return task.ErrIdentityMismatch
	}
}

func readForgetIntentTx(tx *taskdir.ControlTransaction, workspaceID string) (contract.ForgetIntent, error) {
	var intent contract.ForgetIntent
	if err := tx.Read(resourceForgetIntentName, &intent); err != nil {
		return contract.ForgetIntent{}, classifyResourceRecordReadError(err)
	}
	if err := validateForgetIntent(intent, workspaceID); err != nil {
		return contract.ForgetIntent{}, classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(resourceForgetIntentName, intent); err != nil {
		return contract.ForgetIntent{}, err
	}
	return intent, nil
}

func readForgottenTx(tx *taskdir.ControlTransaction, workspaceID string) (contract.ForgottenRecord, error) {
	var record contract.ForgottenRecord
	if err := tx.Read(resourceForgottenName, &record); err != nil {
		return contract.ForgottenRecord{}, classifyResourceRecordReadError(err)
	}
	if record.SchemaVersion != task.SchemaVersion || record.WorkspaceID != workspaceID {
		return contract.ForgottenRecord{}, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	if err := errors.Join(task.ValidateTaskID(record.WorkspaceID), task.ValidateTaskID(record.OperationID)); err != nil {
		return contract.ForgottenRecord{}, classifyStoredResourceRecordError(err)
	}
	if err := validateForgottenAgainstIntentTx(tx, record); err != nil {
		return contract.ForgottenRecord{}, err
	}
	if err := tx.Verify(resourceForgottenName, record); err != nil {
		return contract.ForgottenRecord{}, err
	}
	return record, nil
}

func validateForgottenAgainstIntentTx(tx *taskdir.ControlTransaction, record contract.ForgottenRecord) error {
	intent, err := readForgetIntentTx(tx, record.WorkspaceID)
	if err != nil {
		return requireResourceRecord(err)
	}
	if intent.OperationID != record.OperationID {
		return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	return nil
}
