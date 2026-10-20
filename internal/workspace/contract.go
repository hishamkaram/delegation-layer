// Package workspace exposes managed checkout contracts and runtime operations.
// Its sealed implementations own Git commands and durable journals separately.
package workspace

import "github.com/hishamkaram/delegation-layer/internal/workspace/contract"

const (
	ArtifactLimit         = contract.ArtifactLimit
	DefaultBudget         = contract.DefaultBudget
	OperationCapture      = contract.OperationCapture
	OperationForget       = contract.OperationForget
	OperationProvision    = contract.OperationProvision
	OperationRemove       = contract.OperationRemove
	OperationVerify       = contract.OperationVerify
	SubjectAdmission      = contract.SubjectAdmission
	SubjectOperation      = contract.SubjectOperation
	SubjectTask           = contract.SubjectTask
	FailureDiagnosticName = contract.FailureDiagnosticName

	TurnObserved            = contract.TurnObserved
	TurnUnavailable         = contract.TurnUnavailable
	TurnChanged             = contract.TurnChanged
	TurnUnchanged           = contract.TurnUnchanged
	TurnBaselineUnavailable = contract.TurnBaselineUnavailable

	CapturePatchName      = contract.CapturePatchName
	CaptureNameStatusName = contract.CaptureNameStatusName
	TurnPatchName         = contract.TurnPatchName
	TurnNameStatusName    = contract.TurnNameStatusName
)

type (
	AdmissionAttempt   = contract.AdmissionAttempt
	AnchorRecord       = contract.AnchorRecord
	Artifact           = contract.Artifact
	CaptureArtifact    = contract.CaptureArtifact
	CaptureCandidate   = contract.CaptureCandidate
	CaptureDiff        = contract.CaptureDiff
	CaptureExport      = contract.CaptureExport
	CaptureObligation  = contract.CaptureObligation
	CaptureReceipt     = contract.CaptureReceipt
	Checkout           = contract.Checkout
	DirectoryIdentity  = contract.DirectoryIdentity
	ForgetArtifact     = contract.ForgetArtifact
	ForgetIntent       = contract.ForgetIntent
	ForgetRef          = contract.ForgetRef
	ForgottenRecord    = contract.ForgottenRecord
	OperationReference = contract.OperationReference
	OperationRequest   = contract.OperationRequest
	OperationResult    = contract.OperationResult
	Receipt            = contract.Receipt
	Registration       = contract.Registration
	ReleaseAction      = contract.ReleaseAction
	ReleaseRecord      = contract.ReleaseRecord
	RemovedRecord      = contract.RemovedRecord
	Reservation        = contract.Reservation
	ReservationClosed  = contract.ReservationClosed
	ResourceRequest    = contract.ResourceRequest
	Source             = contract.Source
	Subject            = contract.Subject
	TurnBaseline       = contract.TurnBaseline
	TurnObservation    = contract.TurnObservation
)

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

func ValidateArtifact(artifact Artifact) error { return contract.ValidateArtifact(artifact) }

func PathWithin(root, path string) bool { return contract.PathWithin(root, path) }

func ObserveTurn(baseline *TurnBaseline, final CaptureCandidate) *TurnObservation {
	return contract.ObserveTurn(baseline, final)
}
