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

// EnvironmentApplicator owns the process-level save/apply/restore operation.
// The workflow restores it after retry; the runtime implementation stays at app.
type EnvironmentApplicator func([]string, bool) (func() error, error)

type ExistingTaskOptions struct {
	Creation         CreationOptions
	Selection        supervisorcore.AdmissionSelection
	RequestedBudget  string
	BriefPath        string
	ApplyEnvironment EnvironmentApplicator
}

type ExistingTaskResult struct {
	Observation
	RootID, TaskID          string
	Creation                *CreationResult
	Submission              *SubmissionResult
	Retried                 bool
	Stage                   ExistingStage
	Failure                 error
	EnvironmentRestoreError error
	TaskCloseError          error
}
