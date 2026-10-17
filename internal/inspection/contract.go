package inspection

import (
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"
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
