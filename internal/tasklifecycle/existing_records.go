package tasklifecycle

import (
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
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
	// ExistingOnly forbids fresh preparation when canonical prepared records are unavailable.
	ExistingOnly    bool
	Creation        CreationOptions
	Selection       supervisorcore.AdmissionSelection
	RequestedBudget string
	BriefPath       string
}

type ExistingTaskResult struct {
	Observation
	RootID, TaskID string
	Creation       *CreationResult
	Submission     *SubmissionResult
	Retried        bool
	Stage          ExistingStage
	Failure        error
	TaskCloseError error
}
