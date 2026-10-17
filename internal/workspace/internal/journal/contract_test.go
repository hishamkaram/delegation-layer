// Pure records retain their original facade names and type identity.
package journal

import (
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

type Artifact = contract.Artifact

const ArtifactLimit = contract.ArtifactLimit

type (
	CaptureArtifact   = contract.CaptureArtifact
	CaptureCandidate  = contract.CaptureCandidate
	CaptureExport     = contract.CaptureExport
	CaptureObligation = contract.CaptureObligation
	CaptureReceipt    = contract.CaptureReceipt
	Checkout          = contract.Checkout
)

const DefaultBudget = contract.DefaultBudget

type DirectoryIdentity = contract.DirectoryIdentity

var (
	ErrBusy                    = contract.ErrBusy
	ErrIdentity                = contract.ErrIdentity
	ErrIdentityMismatch        = contract.ErrIdentityMismatch
	ErrInvalidGitContext       = contract.ErrInvalidGitContext
	ErrPending                 = contract.ErrPending
	ErrRefConflict             = contract.ErrRefConflict
	ErrRefUnreadable           = contract.ErrRefUnreadable
	ErrReleased                = contract.ErrReleased
	ErrUnownedRef              = contract.ErrUnownedRef
	ErrUnsupportedObjectFormat = contract.ErrUnsupportedObjectFormat
)

type (
	ForgetArtifact  = contract.ForgetArtifact
	ForgetIntent    = contract.ForgetIntent
	ForgetRef       = contract.ForgetRef
	ForgottenRecord = contract.ForgottenRecord
)

const (
	OperationCapture   = contract.OperationCapture
	OperationForget    = contract.OperationForget
	OperationProvision = contract.OperationProvision
)

type OperationReference = contract.OperationReference

const OperationRemove = contract.OperationRemove

type (
	OperationRequest = contract.OperationRequest
	OperationResult  = contract.OperationResult
)

const OperationVerify = contract.OperationVerify

type (
	Registration      = contract.Registration
	ReleaseAction     = contract.ReleaseAction
	ReleaseRecord     = contract.ReleaseRecord
	RemovedRecord     = contract.RemovedRecord
	Reservation       = contract.Reservation
	ReservationClosed = contract.ReservationClosed
	ResourceRequest   = contract.ResourceRequest
	Source            = contract.Source
	Subject           = contract.Subject
)

const (
	SubjectAdmission = contract.SubjectAdmission
	SubjectOperation = contract.SubjectOperation
	SubjectTask      = contract.SubjectTask
)

type (
	AnchorRecord     = contract.AnchorRecord
	AdmissionAttempt = contract.AdmissionAttempt
	Receipt          = contract.Receipt
)

const FailureDiagnosticName = contract.FailureDiagnosticName

func ValidateArtifact(artifact Artifact) error { return contract.ValidateArtifact(artifact) }
