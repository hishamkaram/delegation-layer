package tasklifecycle

import (
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
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
	TeamTarget                                    *task.TeamTarget
	TeamContext                                   *task.TeamContext
	ProjectRoot                                   string
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
	DispatchResources
)

// DispatchResult preserves primary observations and later closure faults in
// execution order. It is not a second task state or an authorization cache.
type DispatchResult struct {
	Root, RootID, TaskID, Provider string
	Team                           *task.TeamContext
	Record                         TaskRecordPresence
	Creation                       *CreationResult
	Existing                       *ExistingTaskResult
	Phase                          DispatchPhase
	Failure                        error
	Workspace                      *workspaces.AdmissionFinish
	StoreCloseError                error
	// Resources is the latest invocation-local resource assessment of a fresh
	// admission. It is nil when the task ID was not definitely absent.
	Resources *resourceadmission.Assessment
}
