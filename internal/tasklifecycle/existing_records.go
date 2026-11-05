package tasklifecycle

import (
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type ExistingStage uint8

const (
	ExistingPreparation ExistingStage = iota
	ExistingTerminalRelease
	ExistingExplicitRunner
	ExistingStarted
	ExistingEnvironment
	ExistingProfile
	ExistingRunnerRefresh
)

type ExistingTaskOptions struct {
	Creation        CreationOptions
	Selection       supervisorcore.AdmissionSelection
	RequestedBudget string
	BriefPath       string
}

type ExistingTaskResult struct {
	// RootAdmissionCleanup owns only a newly acquired lease retained behind dependent cleanup or failed Release.
	// The caller closes dependent cleanup, then retries Release before closing Store.
	RootAdmissionCleanup *taskdir.RootAdmissionLease

	Observation
	RootID, TaskID            string
	Creation                  *CreationResult
	Submission                *SubmissionResult
	Retried                   bool
	Stage                     ExistingStage
	Failure                   error
	RootAdmissionReleaseError error
	TaskCloseError            error
}
