package app

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func workspaceRuntimeOptions(deps Dependencies) workspacescore.OperationOptions {
	return workspacescore.OperationOptions{ObservationTimeout: deps.SupervisorOptions.ObservationTimeout, RecoverInspection: admissioncore.RecoverInspectionLifetime, Runtime: admissioncore.NewWorkspaceRuntime(supervisorAdmissionOptions(deps, deps.SupervisorOptions))}
}
