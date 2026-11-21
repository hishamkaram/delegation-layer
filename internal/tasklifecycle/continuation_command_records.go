package tasklifecycle

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

type ContinuationOptions struct {
	Dispatch DispatchOptions
	// SuccessorTaskID is an optional caller-allocated identity for tracking an
	// in-flight admission. It grants no admission or compatibility authority.
	SuccessorTaskID string
	// PredecessorGuard selects canonical saved evidence before any continuation
	// mutation. Guarded callers supply a stable successor ID and fresh origin.
	PredecessorGuard *taskdir.ConnectorTaskSelection
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
