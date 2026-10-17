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
// provider turn. Managed state-root runners must still match their content
// address; inspection workers retain their separately pinned identity.
func (profiles Profiles) ForRunner(root string, req task.TaskRecord, meta task.MetaRecord, store *taskdir.Store, currentExecutable, runnerOwnership string) (profile commonprovider.PreparedProfile, resultErr error) {
	candidate, facts, err := profiles.Existing(root, req, meta)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	if err = validateRunnerExecutableBeforeProvider(root, candidate, req, meta, store, currentExecutable, runnerOwnership); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return finalizeMatchedProfile(candidate, facts, root, req, meta)
}

func validateRunnerExecutableBeforeProvider(root string, candidate commonprovider.ProfileCandidate, req task.TaskRecord, meta task.MetaRecord, store *taskdir.Store, currentExecutable, runnerOwnership string) error {
	if candidate.Inspection == nil || meta.RuntimePreflightSHA256 != "" {
		return supervisorcore.ValidateManagedOrdinaryRunnerExecutable(root, meta.RunnerExecutable, currentExecutable, runnerOwnership)
	}
	return validateInspectionRunnerExecutable(root, req, meta, store, currentExecutable)
}

func validateInspectionRunnerExecutable(root string, req task.TaskRecord, meta task.MetaRecord, store *taskdir.Store, currentExecutable string) (resultErr error) {
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
	return supervisorcore.ValidateProviderRunnerExecutable(root, operation.Request().Binding, meta.RunnerOwnership, currentExecutable)
}
