package supervisor

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/inspection"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ValidateManagedOrdinaryRunnerExecutable(recorded, current, ownership string, target RunnerRepairTarget) error {
	if ownership != task.RunnerOwnershipManaged {
		return nil
	}
	if current == "" {
		var err error
		current, err = os.Executable()
		if err != nil {
			return task.ErrEvidenceFault
		}
	}
	return validateManagedRunningRunner(recorded, current, target)
}

// Native lifetime ownership verifies the executing process. The resolver owns
// successor role selection; recorded paths do not claim byte immutability.
func validateManagedRunningRunner(recorded, current string, target RunnerRepairTarget) error {
	if !filepath.IsAbs(current) || filepath.Clean(current) != current {
		return task.ErrEvidenceFault
	}
	if current == recorded {
		return nil
	}
	if err := target.ValidateExecutable(current); err != nil {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	return nil
}

// Inspection-worker upgrades also require their independent request journal.
func ValidateProviderRunnerExecutable(root string, binding inspection.Binding, taskOwnership, current string, target RunnerRepairTarget) error {
	if !filepath.IsAbs(current) || filepath.Clean(current) != current {
		return task.ErrEvidenceFault
	}
	if current == binding.WorkerExecutable {
		return nil
	}
	if !ManagedInspectionBinding(root, binding) || taskOwnership != task.RunnerOwnershipManaged {
		return task.ErrEvidenceFault
	}
	return validateManagedRunningRunner(binding.WorkerExecutable, current, target)
}
