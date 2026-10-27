package tasklifecycle

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

type ContinuationOptions struct {
	Dispatch   DispatchOptions
	Supervisor supervisorcore.ContinuationOptions
}

type ContinuationPhase uint8

const (
	ContinueResolveRoot ContinuationPhase = iota
	ContinueLoadPredecessor
	ContinueWorkspace
	ContinueProvider
	ContinueValidation
	ContinueRunner
	ContinueSession
	ContinueGenerateID
	ContinueSupervisor
	ContinueResources
)

type ContinuationResult struct {
	Root, RootID, PredecessorID string
	Workspace                   *workspaces.Description
	Mode                        commonprovider.ContinuationMode
	SessionSource               ContinuationSessionSource
	Runner                      string
	Supervisor                  *task.SupervisorRef
	Dispatch                    *DispatchResult
	// Resources is the continuation's initial invocation-local assessment;
	// a dispatched successor reports its own latest assessment instead.
	Resources *resourceadmission.Assessment
	Phase     ContinuationPhase
	Failure   error
}
