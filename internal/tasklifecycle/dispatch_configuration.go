package tasklifecycle

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func (options DispatchOptions) staticAdmission() admissioncore.StaticOptions {
	return admissioncore.StaticOptions{Catalog: options.Catalog, Profiles: options.Creation.Admission.Profiles}
}

// workspaceOptions derives the workspace operation scope from the launch
// environment Profiles owns, so Git validation sees the same scope.
func (options DispatchOptions) workspaceOptions() workspaces.OperationOptions {
	workspace := options.Workspace
	workspace.Environment = options.Creation.Admission.Profiles.Environment()
	return workspace
}

func (options DispatchOptions) openStore(root string, create bool) (*taskdir.Store, error) {
	if options.OpenStore == nil {
		return nil, task.ErrEvidenceFault
	}
	return options.OpenStore(root, create)
}
