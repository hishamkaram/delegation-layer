package supervisor

import (
	"fmt"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func newStateTerminalEventRunnerRecord(revision uint64, digest string) stateTerminalEventRunnerRecord {
	return stateTerminalEventRunnerRecord{
		SchemaVersion:          task.SchemaVersion,
		ProtocolPath:           stateTerminalEventRunnerName,
		ImplementationRevision: revision,
		BuildSHA256:            digest,
	}
}

func validateStateTerminalEventRunnerRecord(record stateTerminalEventRunnerRecord) error {
	if record.SchemaVersion != task.SchemaVersion || record.ProtocolPath != stateTerminalEventRunnerName || record.ImplementationRevision == 0 || task.ValidateSHA256(record.BuildSHA256) != nil {
		return fmt.Errorf("%w: invalid terminal event runner record identity", pueue.ErrConfiguration)
	}
	return nil
}

func IsStateRunnerPath(root, path string) bool {
	base := filepath.Join(root, ".supervisor")
	if filepath.Dir(path) != base {
		return false
	}
	name := filepath.Base(path)
	if name == stateRunnerName {
		return true
	}
	prefix := stateRunnerName + "-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	digest := strings.TrimPrefix(name, prefix)
	if len(digest) != 64 {
		return false
	}
	for _, value := range digest {
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
			return false
		}
	}
	return true
}

func ManagedRunnerBuildInfo(info *debug.BuildInfo) bool {
	return info != nil && info.Path == managedRunnerMain
}

func stateSupervisorPairDigest(clientDigest, daemonDigest string) string {
	return task.ComputeSHA256([]byte(clientDigest + "\n" + daemonDigest))
}

func validateStateSupervisorPairRecord(record stateSupervisorPairRecord) error {
	if record.SchemaVersion != 1 || task.ValidateSHA256(record.PairSHA256) != nil || task.ValidateSHA256(record.ClientSHA256) != nil || task.ValidateSHA256(record.DaemonSHA256) != nil || record.PairSHA256 != stateSupervisorPairDigest(record.ClientSHA256, record.DaemonSHA256) {
		return fmt.Errorf("%w: invalid state-root supervisor pair identity", pueue.ErrConfiguration)
	}
	return nil
}

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
		!IsStateRunnerPath(root, receipt.RunnerExecutable) || task.ValidateSHA256(receipt.RunnerSHA256) != nil {
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
