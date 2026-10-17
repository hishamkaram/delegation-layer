package supervisor

import (
	"debug/buildinfo"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/config"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ValidateManagedOrdinaryRunnerExecutable(root, recorded, current, ownership string) error {
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
	return ValidateManagedTaskRunnerExecutable(root, recorded, current)
}

func ValidateManagedTaskRunnerExecutable(root, recorded, current string) error {
	canonical, err := config.CanonicalizePath(current)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if IsStateRunnerPath(root, canonical) {
		if CurrentManagedRunnerExecutable(root, canonical) {
			return nil
		}
		return task.ErrEvidenceFault
	}
	recordedPath, err := config.CanonicalizePath(recorded)
	if err != nil || canonical != recordedPath {
		return task.ErrEvidenceFault
	}
	info, err := buildinfo.ReadFile(canonical)
	if err != nil || !ManagedRunnerBuildInfo(info) {
		return task.ErrEvidenceFault
	}
	return nil
}

// ValidateProviderRunnerExecutable accepts the inspection worker pinned when
// the task was admitted or a verified content-addressed runner installed in
// the state root. Its content-addressed identity proves the successor is
// managed; explicit custom ownership in either task record keeps the original
// runner pinned. Inspection-worker upgrades retain their separate journal
// authorization requirement.
func ValidateProviderRunnerExecutable(root string, binding inspection.Binding, taskOwnership, current string) error {
	canonical, err := config.CanonicalizePath(current)
	if err != nil {
		return task.ErrEvidenceFault
	}
	digest, err := commonprovider.FingerprintExecutable(canonical)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if canonical == binding.WorkerExecutable && digest == binding.WorkerSHA256 {
		return nil
	}
	if binding.RunnerOwnership == task.RunnerOwnershipCustom || taskOwnership == task.RunnerOwnershipCustom {
		return task.ErrEvidenceFault
	}
	if IsStateRunnerPath(root, canonical) && CurrentManagedRunnerExecutable(root, canonical) {
		return nil
	}
	return task.ErrEvidenceFault
}
