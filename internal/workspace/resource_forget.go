package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	resourceForgetIntentName = "forget-intent.json"
	resourceForgottenName    = "forgotten.json"
)

// ForgetRef is one exact Delegate-owned ref in the forget inventory. The
// expected OID is compared by the coordinator's Git CAS operation.
type ForgetRef struct {
	Ref         string `json:"ref"`
	ExpectedOID string `json:"expected_oid"`
}

// ForgetArtifact identifies an optional workspace artifact that the
// coordinator may remove after validating this exact inventory.
type ForgetArtifact struct {
	OperationID string `json:"operation_id"`
	Name        string `json:"name"`
	Length      int64  `json:"length"`
	SHA256      string `json:"sha256"`
}

// ForgetIntent is the sole durable authority for intentionally forgetting the
// listed refs and optional workspace artifacts. It does not perform deletion.
type ForgetIntent struct {
	SchemaVersion int              `json:"schema_version"`
	WorkspaceID   string           `json:"workspace_id"`
	OperationID   string           `json:"operation_id"`
	BudgetNanos   int64            `json:"budget_nanos"`
	Deadline      string           `json:"deadline"`
	Refs          []ForgetRef      `json:"refs,omitempty"`
	Artifacts     []ForgetArtifact `json:"artifacts,omitempty"`
}

// ForgottenRecord is the immutable terminal tombstone for one forget intent.
type ForgottenRecord struct {
	SchemaVersion int    `json:"schema_version"`
	WorkspaceID   string `json:"workspace_id"`
	OperationID   string `json:"operation_id"`
}

// RecordForgetIntent creates the exact deletion inventory once. No ref or
// artifact is removed by this method.
func (r *Resource) RecordForgetIntent(ctx context.Context, intent ForgetIntent) error {
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

func (r *Resource) ForgetIntent(ctx context.Context) (intent ForgetIntent, resultErr error) {
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
	record := ForgottenRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: r.request.WorkspaceID, OperationID: operationID}
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

func (r *Resource) Forgotten(ctx context.Context) (record ForgottenRecord, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		record, err = readForgottenTx(tx, r.request.WorkspaceID)
		return err
	})
	return record, resultErr
}

func validateForgetIntent(intent ForgetIntent, workspaceID string) error {
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
		if err := validateOwnedRef(reference.Ref); err != nil {
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

func validateForgetArtifact(artifact ForgetArtifact) error {
	if err := task.ValidateTaskID(artifact.OperationID); err != nil {
		return err
	}
	if err := validateArtifactName(artifact.Name); err != nil {
		return err
	}
	if artifact.Length < 0 || artifact.Length > ArtifactLimit {
		return task.ErrIdentityMismatch
	}
	return task.ValidateSHA256(artifact.SHA256)
}

func validateForgetOID(oid string) error {
	switch len(oid) {
	case 40:
		return validateOID(oid, "sha1")
	case 64:
		return validateOID(oid, "sha256")
	default:
		return task.ErrIdentityMismatch
	}
}

func readForgetIntentTx(tx *taskdir.ControlTransaction, workspaceID string) (ForgetIntent, error) {
	var intent ForgetIntent
	if err := tx.Read(resourceForgetIntentName, &intent); err != nil {
		return ForgetIntent{}, classifyResourceRecordReadError(err)
	}
	if err := validateForgetIntent(intent, workspaceID); err != nil {
		return ForgetIntent{}, classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(resourceForgetIntentName, intent); err != nil {
		return ForgetIntent{}, err
	}
	return intent, nil
}

func readForgottenTx(tx *taskdir.ControlTransaction, workspaceID string) (ForgottenRecord, error) {
	var record ForgottenRecord
	if err := tx.Read(resourceForgottenName, &record); err != nil {
		return ForgottenRecord{}, classifyResourceRecordReadError(err)
	}
	if record.SchemaVersion != task.SchemaVersion || record.WorkspaceID != workspaceID {
		return ForgottenRecord{}, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	if err := errors.Join(task.ValidateTaskID(record.WorkspaceID), task.ValidateTaskID(record.OperationID)); err != nil {
		return ForgottenRecord{}, classifyStoredResourceRecordError(err)
	}
	if err := validateForgottenAgainstIntentTx(tx, record); err != nil {
		return ForgottenRecord{}, err
	}
	if err := tx.Verify(resourceForgottenName, record); err != nil {
		return ForgottenRecord{}, err
	}
	return record, nil
}

func validateForgottenAgainstIntentTx(tx *taskdir.ControlTransaction, record ForgottenRecord) error {
	intent, err := readForgetIntentTx(tx, record.WorkspaceID)
	if err != nil {
		return requireResourceRecord(err)
	}
	if intent.OperationID != record.OperationID {
		return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	return nil
}
