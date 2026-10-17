package tasklifecycle

import (
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// DispatchRequest contains requested values and the recorded continuation binding.
// Only journal operations grant authority; these values confer none.
type DispatchRequest struct {
	Root, TaskID, GeneratedTaskID, ResumeTask     string
	Provider                                      string
	Auto                                          bool
	Config                                        task.TaskConfig
	Cwd, BriefPath                                string
	BriefBytes                                    []byte
	Isolation, WorkspaceID, Base, WorkspaceBudget string
	WorkspaceBinding                              *task.WorkspaceBinding
	Selection                                     supervisorcore.AdmissionSelection
}

type TaskRecordPresence uint8

const (
	RecordUnknown TaskRecordPresence = iota
	RecordAbsent
	RecordCreated
)

type DispatchPhase uint8

const (
	DispatchValidate DispatchPhase = iota
	DispatchOpenState
	DispatchGenerateID
	DispatchWorkspace
	DispatchProviderLookup
	DispatchProviderSelection
	DispatchPredecessor
	DispatchOpenTask
	DispatchEnvironment
)

// DispatchResult preserves primary observations and later closure faults in
// execution order. It is not a second task state or an authorization cache.
type DispatchResult struct {
	Root, RootID, TaskID, Provider string
	Record                         TaskRecordPresence
	Creation                       *CreationResult
	Existing                       *ExistingTaskResult
	Phase                          DispatchPhase
	Failure                        error
	EnvironmentRestoreError        error
	Workspace                      *workspaces.AdmissionFinish
	StoreCloseError                error
}
