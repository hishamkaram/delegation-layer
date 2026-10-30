package supervisor

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// continuationRunnerRequestError is the one refusal for a continuation whose
// requested runner differs from the predecessor's recorded runner. It matches
// task.ErrIdentityMismatch for existing consumers, carries no path, and has no
// cause so a wrapped copy stays recognizable as this exact refusal only.
type continuationRunnerRequestError struct{}

var errContinuationRunnerRequest error = continuationRunnerRequestError{}

func (continuationRunnerRequestError) Error() string {
	return "continuation runner does not match the predecessor runner"
}

func (continuationRunnerRequestError) Is(target error) bool {
	return target == task.ErrIdentityMismatch
}

// IsContinuationRunnerRequest reports whether leaf is the exact refusal that
// ValidateContinuationSelection returns for an unequal runner. It is a leaf
// predicate: task.AllErrorCauses unwraps ordinary wrapping before calling it.
func IsContinuationRunnerRequest(leaf error) bool {
	return leaf == errContinuationRunnerRequest //nolint:errorlint // exact leaf identity; the caller's traversal already unwrapped
}

func ValidateContinuationSelection(root string, selection AdmissionSelection, meta *task.MetaRecord) error {
	if meta.RunnerExecutable == "" && selection.Runner == "" {
		return errors.New("continuation unavailable: predecessor runner executable is not recorded")
	}
	if meta.RunnerExecutable != "" && selection.Runner != "" && selection.Runner != meta.RunnerExecutable {
		return errContinuationRunnerRequest
	}
	if meta.SupervisorConfig.ConfigPath == "" {
		return errors.New("continuation unavailable: predecessor supervisor binding is not recorded")
	}
	private := pueue.IsPrivateConfig(root, meta.SupervisorConfig.ConfigPath)
	if selection.ConfigPath != "" && (private || selection.ConfigPath != meta.SupervisorConfig.ConfigPath) {
		return fmt.Errorf("%w: continuation must use the predecessor supervisor binding", task.ErrRequestConflict)
	}
	return nil
}
