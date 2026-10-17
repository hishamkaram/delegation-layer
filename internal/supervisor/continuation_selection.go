package supervisor

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ValidateContinuationSelection(root string, selection AdmissionSelection, meta *task.MetaRecord) error {
	if meta.RunnerExecutable == "" && selection.Runner == "" {
		return errors.New("continuation unavailable: predecessor runner executable is not recorded")
	}
	if meta.RunnerExecutable != "" && selection.Runner != "" && selection.Runner != meta.RunnerExecutable {
		return fmt.Errorf("%w: continuation runner %q does not match predecessor runner %q", task.ErrIdentityMismatch, selection.Runner, meta.RunnerExecutable)
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
