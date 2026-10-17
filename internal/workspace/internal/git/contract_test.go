package git

import "github.com/hishamkaram/delegation-layer/internal/workspace/contract"

type (
	Source           = contract.Source
	Checkout         = contract.Checkout
	CaptureCandidate = contract.CaptureCandidate
	CaptureExport    = contract.CaptureExport
)

var (
	ErrInvalidGitContext       = contract.ErrInvalidGitContext
	ErrIdentityMismatch        = contract.ErrIdentityMismatch
	ErrUnsupportedObjectFormat = contract.ErrUnsupportedObjectFormat
	ErrUnownedRef              = contract.ErrUnownedRef
	ErrRefConflict             = contract.ErrRefConflict
	ErrRefUnreadable           = contract.ErrRefUnreadable
)

type ForgetRef = contract.ForgetRef

const ArtifactLimit = contract.ArtifactLimit
