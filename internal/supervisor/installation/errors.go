package installation

import (
	"errors"
	"os"
	"os/exec"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ErrSupervisorSelectionChanged refuses replacement of an accepted resolved native path.
var ErrSupervisorSelectionChanged = errors.New("private supervisor executable selection differs from accepted bootstrap")

// Only a raw installation lookup can classify this observation. Inventory
// validation returns before this boundary, so missing inventory members remain
// hard integrity errors. The original lookup error remains available to callers.
type unavailableBundledPairError struct{ cause error }

func (e *unavailableBundledPairError) Error() string { return e.cause.Error() }
func (e *unavailableBundledPairError) Unwrap() error { return e.cause }

func classifyUnavailableBundledPair(err error) error {
	if !missingBundledPairCauses(err) {
		return err
	}
	return &unavailableBundledPairError{cause: err}
}

// UnavailableBundledPair reports only classified installation absence, preserving independent faults.
func UnavailableBundledPair(err error) bool {
	var unavailable *unavailableBundledPairError
	return errors.As(err, &unavailable) && unavailable != nil && missingBundledPairCauses(err)
}

func missingBundledPairCauses(err error) bool {
	return errors.Is(err, os.ErrNotExist) && task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, os.ErrNotExist) || errors.Is(cause, exec.ErrNotFound) || errors.Is(cause, pueue.ErrConfiguration)
	})
}
