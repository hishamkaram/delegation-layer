package tasklifecycle

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
)

type SubmissionStage uint8

const (
	SubmissionClient SubmissionStage = iota
	SubmissionProfile
	SubmissionRunner
	SubmissionRunnerValidation
	SubmissionSession
	SubmissionPermit
	SubmissionObservation
	SubmissionRecords
)

// SubmissionResult retains the primary failure and exact closure phases.
// A late close error cannot retroactively change the primary observation.
type SubmissionResult struct {
	Observation
	Stage              SubmissionStage
	Failure            error
	PermitReleaseError error
	TaskCloseError     error
}

type SubmissionOptions struct {
	Supervisor          SupervisorOptions
	RequestedRunner     string
	DefaultRunner       string
	OpenValidationStore admissioncore.SubmissionStoreOpener
}
