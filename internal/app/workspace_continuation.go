package app

import "github.com/hishamkaram/delegation-layer/internal/admission"

func managedContinuationFailure(root string, response Response, cause error) commandResult {
	result := failed(response, admission.WithStage("prepare-workspace", cause), 1)
	if response.Workspace != nil {
		setManagedAdmissionPendingFailure(root, "", *response.Workspace, &result)
	}
	return result
}
