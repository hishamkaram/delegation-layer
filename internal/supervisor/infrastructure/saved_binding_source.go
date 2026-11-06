package infrastructure

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type SavedBindingKind string

const (
	SavedBindingTaskMetadata     SavedBindingKind = "taskmeta"
	SavedBindingTaskSubmission   SavedBindingKind = "tasksubmit"
	SavedBindingTaskStop         SavedBindingKind = "taskstop"
	SavedBindingWorkspaceRequest SavedBindingKind = "workspacerequest"
)

// SavedBindingSource selects one canonical record slot in the receiving root.
// That task, stop request or workspace request owns the binding. This immutable
// locator grants no recovery authority: the owner must re-read the slot through
// its validating reader, compare the whole binding and verify its private root.
// Paths and executable selections never come from this wire value.
type SavedBindingSource struct {
	RootID        string           `json:"root_id"`
	Kind          SavedBindingKind `json:"kind"`
	OwnerID       string           `json:"owner_id"`
	RecordID      string           `json:"record_id"`
	BindingSHA256 string           `json:"binding_sha256"`
}

func (s SavedBindingSource) Validate() error {
	if err := errors.Join(task.ValidateRootID(s.RootID), task.ValidateTaskID(s.OwnerID), task.ValidateSHA256(s.BindingSHA256)); err != nil {
		return errors.Join(ErrIdentity, err)
	}
	switch s.Kind {
	case SavedBindingTaskMetadata, SavedBindingTaskSubmission, SavedBindingWorkspaceRequest:
		if s.RecordID != "" {
			return ErrIdentity
		}
	case SavedBindingTaskStop:
		if err := task.ValidateStopRequestID(s.RecordID); err != nil {
			return errors.Join(ErrIdentity, err)
		}
	default:
		return ErrIdentity
	}
	return nil
}

// SavedBindingDigest uses the complete persisted value, including resolution
// and endpoint fields. Legacy records retain their existing validation rules;
// actual recovery eligibility and executable custody belong to their consumers.
func SavedBindingDigest(binding task.SupervisorRef) (string, error) {
	if err := task.ValidateSupervisorRef(binding); err != nil {
		return "", errors.Join(ErrIdentity, err)
	}
	data, err := task.MarshalCanonical(binding)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

func (s SavedBindingSource) ValidateBinding(binding task.SupervisorRef) error {
	if err := s.Validate(); err != nil {
		return err
	}
	digest, err := SavedBindingDigest(binding)
	if err != nil {
		return err
	}
	if digest != s.BindingSHA256 {
		return ErrIdentity
	}
	return nil
}
