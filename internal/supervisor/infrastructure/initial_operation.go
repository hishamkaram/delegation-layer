package infrastructure

import (
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type InitialOperationKind string

const (
	InitialEnsureOwn           InitialOperationKind = "ensure-own"
	InitialRecoverSaved        InitialOperationKind = "recover-saved"
	InitialPrepareContinuation InitialOperationKind = "prepare-continuation"
	InitialMigrateOwn          InitialOperationKind = "migrate-own"
)

// InitialOperation is immutable purpose and subject data owned by the accepted
// startup intent. Its zero value preserves the legacy EnsureOwn operation.
// Subject records remain authoritative; these selectors confer no authority.
type InitialOperation struct {
	Kind   InitialOperationKind `json:"kind"`
	Source SavedBindingSource   `json:"source,omitzero"`
}

func (o InitialOperation) Validate(rootID string) error {
	if err := task.ValidateRootID(rootID); err != nil {
		return fmt.Errorf("%w: invalid initial operation root: %w", ErrIdentity, err)
	}
	if (o.Kind == InitialRecoverSaved || o.Kind == InitialPrepareContinuation) && o.Source.RootID != rootID {
		return ErrIdentity
	}
	return o.validateSubject()
}

func (o InitialOperation) validateSubject() error {
	switch o.Kind {
	case "", InitialEnsureOwn, InitialMigrateOwn:
		if o.Source != (SavedBindingSource{}) {
			return fmt.Errorf("%w: initial operation has an unexpected subject", ErrIdentity)
		}
	case InitialPrepareContinuation:
		if o.Source.Kind != SavedBindingTaskMetadata {
			return ErrIdentity
		}
		fallthrough
	case InitialRecoverSaved:
		return o.Source.Validate()
	default:
		return fmt.Errorf("%w: unknown initial operation", ErrIdentity)
	}
	return nil
}

func (o InitialOperation) normalized() InitialOperation {
	if o.Kind == InitialEnsureOwn {
		return InitialOperation{}
	}
	return o
}

// A present empty kind must not alias the omitted legacy value. Decode through
// a distinct representation to retain strict nested field checks without
// recursively invoking this method.
func (o *InitialOperation) UnmarshalJSON(data []byte) error {
	type representation InitialOperation
	var decoded representation
	if err := task.DecodeStrict(data, &decoded); err != nil {
		return err
	}
	if decoded.Kind == "" {
		return fmt.Errorf("%w: present initial operation requires a kind", ErrIdentity)
	}
	operation := InitialOperation(decoded)
	if err := operation.validateSubject(); err != nil {
		return err
	}
	*o = operation
	return nil
}
