package tasklifecycle

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type CreationStage uint8

const (
	CreationAdmission CreationStage = iota
	CreationProfile
	CreationBrief
	CreationCapture
	CreationRecord
	CreationResources
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
	// RootAdmissionCleanup owns only a newly acquired lease retained behind dependent cleanup or failed Release.
	// The caller closes dependent cleanup, then retries Release before closing Store.
	RootAdmissionCleanup      *taskdir.RootAdmissionLease
	RootAdmissionReleaseError error

	Capability *admissioncore.CapabilityEvidence
	TaskRecord CreationRecordObservation
	Supervisor *task.SupervisorRef // Exact binding persisted in newly created metadata.
	Submission *SubmissionResult
	Stage      CreationStage
	Failure    error
}

type CreationOptions struct {
	RootAdmission       *taskdir.RootAdmissionLease // Borrowed; the outer creator owns Release.
	Admission           admissioncore.PreparationOptions
	PublisherVersion    string
	OpenValidationStore admissioncore.SubmissionStoreOpener
}
