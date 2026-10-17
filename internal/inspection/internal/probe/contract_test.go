package probe

import (
	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"
)

const (
	maxProjectedFacts = contract.MaxProjectedFacts
	maxRuntimeFacts   = contract.MaxRuntimeFacts
)

var (
	ErrProjectionDefinition       = contract.ErrProjectionDefinition
	ErrProjectionDeadline         = contract.ErrProjectionDeadline
	ErrProjectionNativeTooLarge   = contract.ErrProjectionNativeTooLarge
	ErrProjectionCallback         = contract.ErrProjectionCallback
	ErrProjectionFacts            = contract.ErrProjectionFacts
	ErrProjectionHTTP             = contract.ErrProjectionHTTP
	ErrProjectionResponseTooLarge = contract.ErrProjectionResponseTooLarge
	ErrProjectionCanceled         = contract.ErrProjectionCanceled
)
