package workspaces

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// An accepted operation keeps its selected worker. Missing installations and
// historical state copies refuse a new command; neither retargets that request.
func validateWorkspaceOperationExecutable(root, recorded string) error {
	if pueue.IsLegacyStateRunnerPath(root, recorded) {
		return task.ErrIdentityMismatch
	}
	selected, err := supervisorcore.ResolveRunner(recorded, "")
	if err != nil {
		return err
	}
	if selected != recorded || pueue.IsLegacyStateRunnerPath(root, selected) {
		return task.ErrIdentityMismatch
	}
	return nil
}
