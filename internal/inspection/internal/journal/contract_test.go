package journal

import (
	"encoding/json"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	AdmissionTimeout              = contract.AdmissionTimeout
	WorkerExitProofGuardianWaitV1 = contract.WorkerExitProofGuardianWaitV1
	ModelDiscoveryTimeout         = contract.ModelDiscoveryTimeout
	ResultEligible                = contract.ResultEligible
	ResultUnavailable             = contract.ResultUnavailable
	ResultExpired                 = contract.ResultExpired
)

type (
	Binding                    = contract.Binding
	RequestRecord              = contract.RequestRecord
	SubmissionRecord           = contract.SubmissionRecord
	StartRecord                = contract.StartRecord
	ManagedWorkerUpgradeRecord = contract.ManagedWorkerUpgradeRecord
	GroupRequestRecord         = contract.GroupRequestRecord
	GroupMigrationRecord       = contract.GroupMigrationRecord
	GroupCreationRecord        = contract.GroupCreationRecord
	GroupObservationRecord     = contract.GroupObservationRecord
	ResultReason               = contract.ResultReason
	ReceiptRecord              = contract.ReceiptRecord
	ResultRecord               = contract.ResultRecord
	CompletionRecord           = contract.CompletionRecord
	WorkerObservationRecord    = contract.WorkerObservationRecord
	StopRequestRecord          = contract.StopRequestRecord
	StopReplyRecord            = contract.StopReplyRecord
	StopObservationRecord      = contract.StopObservationRecord
)

var (
	ErrAdmissionExpired           = contract.ErrAdmissionExpired
	ErrInvalidAdmissionTime       = contract.ErrInvalidAdmissionTime
	ErrGroupMigrationRequired     = contract.ErrGroupMigrationRequired
	ErrGroupMigrationBusy         = contract.ErrGroupMigrationBusy
	ErrGroupMigrationUnsupported  = contract.ErrGroupMigrationUnsupported
	ErrProjectionDefinition       = contract.ErrProjectionDefinition
	ErrProjectionDeadline         = contract.ErrProjectionDeadline
	ErrProjectionNativeTooLarge   = contract.ErrProjectionNativeTooLarge
	ErrProjectionCallback         = contract.ErrProjectionCallback
	ErrProjectionFacts            = contract.ErrProjectionFacts
	ErrProjectionHTTP             = contract.ErrProjectionHTTP
	ErrProjectionResponseTooLarge = contract.ErrProjectionResponseTooLarge
	ErrProjectionCanceled         = contract.ErrProjectionCanceled
	ErrStopAlreadyClaimed         = contract.ErrStopAlreadyClaimed
)

func ValidateBinding(binding Binding) error { return contract.ValidateBinding(binding) }
func ValidateRequestRecord(record RequestRecord, expectedRoot string) error {
	return contract.ValidateRequestRecord(record, expectedRoot)
}

func TimeoutForRevision(revision string) time.Duration { return contract.TimeoutForRevision(revision) }

const (
	maxProjectedFacts = contract.MaxProjectedFacts
	maxRuntimeFacts   = contract.MaxRuntimeFacts
)

func groupForRoot(rootID string) string                 { return contract.GroupForRoot(rootID) }
func groupGeneration(binding task.SupervisorRef) string { return contract.GroupGeneration(binding) }
func labelForIdentity(rootID, taskID string) string     { return contract.LabelForIdentity(rootID, taskID) }
func requestDigest(record RequestRecord) string         { return contract.RequestDigest(record) }
func inspectionTimeout(binding Binding) time.Duration {
	return contract.InspectionTimeout(binding)
}

func canonicalFactsWithin(raw json.RawMessage, limit int) (json.RawMessage, error) {
	return contract.CanonicalFactsWithin(raw, limit)
}

func canonicalProjectionFacts(raw json.RawMessage) (json.RawMessage, error) {
	return contract.CanonicalProjectionFacts(raw)
}
