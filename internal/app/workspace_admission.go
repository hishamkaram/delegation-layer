package app

import workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"

type (
	WorkspaceResponse         = workspacescore.Description
	WorkspaceArtifactResponse = workspacescore.ArtifactDescription
)

func applyManagedAdmissionFinish(root string, finished workspacescore.AdmissionFinish, result *commandResult) {
	if finished.AbortError != nil {
		mergeCommandClose(result, func() error { return finished.AbortError })
	}
	result.response.Workspace = &finished.Description
	if finished.DescriptionError != nil {
		mergeCommandClose(result, func() error { return finished.DescriptionError })
	}
	setManagedAdmissionPendingFailure(root, finished.OperationID, finished.Description, result)
	mergeCommandClose(result, func() error { return finished.CloseError })
}
