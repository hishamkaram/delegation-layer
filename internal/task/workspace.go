package task

import "errors"

// MaxWorkspaceArtifactSize bounds each Git capture or verification artifact.
// It is independent of provider brief and result limits.
const MaxWorkspaceArtifactSize int64 = 8 * 1024 * 1024

// Clone prevents request snapshots from sharing the optional binding pointer.
func (binding *WorkspaceBinding) Clone() *WorkspaceBinding {
	if binding == nil {
		return nil
	}
	copy := *binding
	return &copy
}

func sameWorkspaceBinding(first, second *WorkspaceBinding) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return *first == *second
}

func validateWorkspaceBinding(binding *WorkspaceBinding) error {
	if binding == nil {
		return nil
	}
	if binding.Reservation == 0 || binding.BudgetNanos <= 0 {
		return ErrIdentityMismatch
	}
	return errors.Join(ValidateTaskID(binding.ID), ValidateSHA256(binding.RequestSHA256))
}
