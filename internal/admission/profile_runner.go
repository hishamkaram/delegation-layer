package admission

import (
	"context"
	"errors"
	"os"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ForRunner adds the final executable identity check that
// is possible only inside the queued runner process, immediately before the
// provider turn. Native ownership and immutable records retain existing
// images; a managed successor requires the selected installed role.
func (profiles Profiles) ForRunner(root string, req task.TaskRecord, meta task.MetaRecord, store *taskdir.Store, currentExecutable, runnerOwnership, configuredRole string) (profile commonprovider.PreparedProfile, resultErr error) {
	candidate, facts, err := profiles.Existing(root, req, meta)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	if err = validateRunnerExecutableBeforeProvider(root, candidate, req, meta, store, currentExecutable, runnerOwnership, configuredRole); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return finalizeMatchedProfile(candidate, facts, root, req, meta)
}

func validateRunnerExecutableBeforeProvider(root string, candidate commonprovider.ProfileCandidate, req task.TaskRecord, meta task.MetaRecord, store *taskdir.Store, currentExecutable, runnerOwnership, configuredRole string) error {
	if candidate.Inspection == nil || meta.RuntimePreflightSHA256 != "" {
		if currentExecutable == "" {
			var err error
			currentExecutable, err = os.Executable()
			if err != nil {
				return err
			}
		}
		var target supervisorcore.RunnerRepairTarget
		if runnerOwnership == task.RunnerOwnershipManaged {
			var err error
			target, err = managedSuccessorTarget(configuredRole, meta.RunnerExecutable, currentExecutable)
			if err != nil {
				return err
			}
		}
		return supervisorcore.ValidateManagedOrdinaryRunnerExecutable(root, meta.RunnerExecutable, currentExecutable, runnerOwnership, target)
	}
	return validateInspectionRunnerExecutable(root, req, store, currentExecutable, runnerOwnership, configuredRole)
}

func validateInspectionRunnerExecutable(root string, req task.TaskRecord, store *taskdir.Store, currentExecutable, runnerOwnership, configuredRole string) (resultErr error) {
	if store == nil {
		return task.ErrEvidenceFault
	}
	operation, err := LoadInspectionOperationContext(context.Background(), store, req)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if currentExecutable == "" {
		currentExecutable, err = os.Executable()
		if err != nil {
			return task.ErrEvidenceFault
		}
	}
	binding := operation.Request().Binding
	var target supervisorcore.RunnerRepairTarget
	if supervisorcore.ManagedInspectionBinding(root, binding) && runnerOwnership == task.RunnerOwnershipManaged {
		target, err = managedSuccessorTarget(configuredRole, binding.WorkerExecutable, currentExecutable)
		if err != nil {
			return err
		}
		if binding.RunnerOwnership == "" && currentExecutable != binding.WorkerExecutable {
			if _, err = operation.ReadProofContext(context.Background()); err != nil {
				return err
			}
		}
	}
	return supervisorcore.ValidateProviderRunnerExecutable(root, binding, runnerOwnership, currentExecutable, target)
}
