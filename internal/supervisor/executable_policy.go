package supervisor

import (
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func queuedRunnerCommandIdentity(rootID, label, taskPrefix string) (taskID string, inspectionWorker, matched bool) {
	if strings.HasPrefix(label, taskPrefix) {
		return strings.TrimPrefix(label, taskPrefix), false, true
	}
	inspectionPrefix := (pueue.InspectionIdentity{RootID: rootID}).Group() + "-"
	if strings.HasPrefix(label, inspectionPrefix) {
		return strings.TrimPrefix(label, inspectionPrefix), true, true
	}
	return "", false, false
}

func validateRunnerRepairReceiptShape(root string, receipt runnerRepairReceipt) error {
	if receipt.SchemaVersion != task.SchemaVersion || receipt.Revision != runnerRepairReceiptRevision ||
		task.ValidateFreshSupervisorRef(receipt.Supervisor) != nil ||
		!pueue.IsPrivateConfig(root, receipt.Supervisor.ConfigPath) ||
		!filepath.IsAbs(receipt.RunnerExecutable) || filepath.Clean(receipt.RunnerExecutable) != receipt.RunnerExecutable ||
		receipt.ExecutableIdentity.Validate() != nil {
		return task.ErrEvidenceFault
	}
	if receipt.ExecutableIdentity == task.ExecutableIdentityInstalledPathV1 {
		if receipt.RunnerSHA256 != "" || pueue.IsLegacyStateRunnerPath(root, receipt.RunnerExecutable) {
			return task.ErrEvidenceFault
		}
	} else if !pueue.IsLegacyStateRunnerPath(root, receipt.RunnerExecutable) || task.ValidateSHA256(receipt.RunnerSHA256) != nil {
		return task.ErrEvidenceFault
	}
	return nil
}

func validateInspectionReplacementIdentity(store *taskdir.Store, replacement pueue.RunnerCommandReplacement, request inspection.RequestRecord, identity pueue.InspectionIdentity) error {
	if (pueue.InspectionIdentity{RootID: replacement.RootID, QueueGroup: replacement.InspectionGroup}).Group() != request.Group {
		return task.ErrIdentityMismatch
	}
	if request.RootID != store.RootID || request.IdentityID() != replacement.TaskID || request.Task.RootID != store.RootID || request.Task.TaskID != request.TaskID || request.Group != identity.Group() || request.Label != identity.Label() || replacement.Label != identity.Label() || request.Binding.WorkerExecutable != replacement.OldRunner {
		return task.ErrIdentityMismatch
	}
	return nil
}
