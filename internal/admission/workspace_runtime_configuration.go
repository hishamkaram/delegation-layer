package admission

import supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

type workspaceRuntime struct {
	configuration supervisorcore.AdmissionOptions
}

// NewWorkspaceRuntime composes supervisor operations with admission's stage errors.
// Configuration is private and copied; the returned value has no mutable owner state.
func NewWorkspaceRuntime(configuration supervisorcore.AdmissionOptions) workspaceRuntime {
	return workspaceRuntime{configuration: configuration}
}
