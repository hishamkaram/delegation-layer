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
	// RootAdmissionCleanup owns only a newly acquired lease retained behind dependent cleanup or failed Release.
	// The caller closes dependent cleanup, then retries Release before closing Store.
	RootAdmissionCleanup *taskdir.RootAdmissionLease
	// StoreCleanup owns the Store retained behind a failed lease release or Close.
	StoreCleanup *taskdir.Store

	RootAdmissionReleaseError   error
	StoreCloseError             error
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
