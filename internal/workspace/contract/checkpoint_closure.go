package contract

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ClosureEvidence is the canonical lifetime-closure subset a qualified
// receipt records. Its four values use the CaptureReceipt field encoding.
type ClosureEvidence struct {
	Kind              task.LifetimeClosureKind
	TerminationSHA256 string
	WorkerWaitSHA256  string
	UnlaunchedSHA256  string
}

// ValidateClosureEvidence accepts exactly one closed-lifetime evidence shape
// and reports whether its worker may have run. An unlaunched or unregistered
// recovered lifetime never ran its worker; open or registration-only kinds are
// not recorded closure evidence.
func ValidateClosureEvidence(closure ClosureEvidence) (workerMayHaveRun bool, err error) {
	switch closure.Kind {
	case task.LifetimeClosureUnregisteredRecovered:
		if closure.TerminationSHA256 != "" || closure.UnlaunchedSHA256 != "" {
			return false, task.ErrIdentityMismatch
		}
		return false, task.ValidateSHA256(closure.WorkerWaitSHA256)
	case task.LifetimeClosureLegacyPreviousBoot:
		if closure.WorkerWaitSHA256 != "" || closure.UnlaunchedSHA256 != "" {
			return false, task.ErrIdentityMismatch
		}
		return true, task.ValidateSHA256(closure.TerminationSHA256)
	case "":
		return validateRecordedClosure(closure)
	case task.LifetimeClosureOpen, task.LifetimeClosureRegisteredNative, task.LifetimeClosureRegisteredRecovered:
		return false, task.ErrInvalidEnum
	default:
		return false, task.ErrInvalidEnum
	}
}

func validateRecordedClosure(closure ClosureEvidence) (bool, error) {
	if closure.UnlaunchedSHA256 == "" {
		return true, errors.Join(task.ValidateSHA256(closure.TerminationSHA256), task.ValidateSHA256(closure.WorkerWaitSHA256))
	}
	if closure.TerminationSHA256 != "" || closure.WorkerWaitSHA256 != "" {
		return false, task.ErrIdentityMismatch
	}
	return false, task.ValidateSHA256(closure.UnlaunchedSHA256)
}

// validateCheckpointSuccessClosure requires the default native encoding for a
// checkpoint success, after ValidateClosureEvidence accepted the closure: no
// recovery kind, termination and worker-wait digests, and no unlaunched record.
// A legacy previous-boot closure has no worker wait and cannot record success.
// This checks encoding only; the coordinator qualifies the actual closure kind.
func validateCheckpointSuccessClosure(closure ClosureEvidence) error {
	if closure.Kind != "" || closure.UnlaunchedSHA256 != "" || closure.TerminationSHA256 == "" || closure.WorkerWaitSHA256 == "" {
		return task.ErrIdentityMismatch
	}
	return nil
}

// validateUnavailableReason requires the not-launched reason exactly when the
// closure proves the worker never ran, and otherwise one of the run reasons.
func validateUnavailableReason(reason, notLaunched string, workerMayHaveRun bool, runReasons ...string) error {
	if !workerMayHaveRun {
		if reason != notLaunched {
			return task.ErrIdentityMismatch
		}
		return nil
	}
	for _, allowed := range runReasons {
		if reason == allowed {
			return nil
		}
	}
	return task.ErrIdentityMismatch
}
