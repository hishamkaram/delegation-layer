package tasklifecycle

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
)

type CreationStage uint8

const (
	CreationAdmission CreationStage = iota
	CreationProfile
	CreationBrief
	CreationCapture
	CreationRecord
)

type CreationRecordObservation uint8

const (
	CreationRecordUnobserved CreationRecordObservation = iota
	CreationRecordUnknown
	CreationRecordCreated
)

// CreationResult contains observations, not journal authority. Submission keeps
// the primary failure separate from later closure faults for faithful projection.
type CreationResult struct {
	Capability *admissioncore.CapabilityEvidence
	TaskRecord CreationRecordObservation
	Submission *SubmissionResult
	Stage      CreationStage
	Failure    error
}

type CreationOptions struct {
	Admission           admissioncore.PreparationOptions
	PublisherVersion    string
	OpenValidationStore admissioncore.SubmissionStoreOpener
}
