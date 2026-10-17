package workspaces

import (
	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// Admission owns one workspace resource and its use lease through dispatch.
// Only this package mutates the reservation and helper binding.
type Admission struct {
	resource    *workspace.Resource
	owner       *taskdir.LifetimeOwnerLease
	reservation workspace.Reservation
	binding     *task.WorkspaceBinding
	operationID string
	existing    bool
}

type dispatchIdentity struct {
	TaskID               string          `json:"task_id"`
	Provider             string          `json:"provider"`
	Auto                 bool            `json:"auto"`
	SourceCwd            string          `json:"source_cwd"`
	WorkspaceID          string          `json:"workspace_id,omitempty"`
	Base                 string          `json:"base"`
	Config               task.TaskConfig `json:"config"`
	BriefSHA256          string          `json:"brief_sha256"`
	WorkspaceBudgetNanos int64           `json:"workspace_budget_nanos"`
}

// DispatchRequest contains only immutable workspace admission inputs.
type DispatchRequest struct {
	Isolation           string
	WorkspaceID         string
	Base                string
	Provider            string
	Auto                bool
	Config              task.TaskConfig
	WorkspaceBudget     string
	Cwd                 string
	HasWorkspaceBinding bool
	RunnerOwnership     string
}

// AdmissionFinish separates the original finalization stages for presentation.
// It carries observations and errors, never a lease or mutable resource owner.
type AdmissionFinish struct {
	Description      Description
	OperationID      string
	AbortError       error
	DescriptionError error
	CloseError       error
}
