package tasklifecycle

import (
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func DispatchRecordLookup(requestedTaskID string, exists bool, lookupErr error) TaskRecordPresence {
	if exists {
		return RecordCreated
	}
	if lookupErr != nil || requestedTaskID != "" {
		return RecordUnknown
	}
	return RecordAbsent
}

func WorkspaceDispatchRequest(request DispatchRequest) workspaces.DispatchRequest {
	return workspaces.DispatchRequest{
		Isolation: request.Isolation, WorkspaceID: request.WorkspaceID, Base: request.Base,
		Provider: request.Provider, Auto: request.Auto, Config: request.Config,
		WorkspaceBudget: request.WorkspaceBudget, Cwd: request.Cwd,
		HasWorkspaceBinding: request.WorkspaceBinding != nil, RunnerOwnership: supervisorcore.RequestedRunnerOwnership(request.Selection),
	}
}

func (result DispatchResult) RecordPresence() TaskRecordPresence {
	created := result.Creation
	if result.Existing != nil && result.Existing.Creation != nil {
		created = result.Existing.Creation
	}
	if created != nil {
		switch created.TaskRecord {
		case CreationRecordUnobserved:
		case CreationRecordUnknown:
			return RecordUnknown
		case CreationRecordCreated:
			return RecordCreated
		}
	}
	return result.Record
}
