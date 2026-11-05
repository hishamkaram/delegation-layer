package supervisor

import (
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// NewSavedBindingSource describes the caller's canonical record slot. It grants
// no authority: resident preparation re-reads and validates that exact record.
func NewSavedBindingSource(rootID string, kind infrastructure.SavedBindingKind, ownerID, recordID string, binding task.SupervisorRef) (infrastructure.SavedBindingSource, error) {
	digest, err := infrastructure.SavedBindingDigest(binding)
	if err != nil {
		return infrastructure.SavedBindingSource{}, err
	}
	source := infrastructure.SavedBindingSource{RootID: rootID, Kind: kind, OwnerID: ownerID, RecordID: recordID, BindingSHA256: digest}
	if err = source.Validate(); err != nil {
		return infrastructure.SavedBindingSource{}, err
	}
	return source, nil
}
