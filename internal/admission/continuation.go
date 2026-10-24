package admission

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ValidateContinuation constructs the exact successor identity and applies
// the shared static admission gate before any continuation recovery. The
// provider candidate is compiled in the predecessor's recorded launch scope,
// selected from the caller's scope by Profiles; supervisor control keeps the
// caller's own environment.
func ValidateContinuation(options StaticOptions, root string, predecessor task.TaskRecord, predecessorMeta task.MetaRecord, prior *task.PriorSession, successor task.TaskRecord) (task.TaskRecord, error) {
	if prior == nil {
		return task.TaskRecord{}, errors.New("predecessor session is unavailable")
	}
	priorCopy := *prior
	successor.PriorSession = &priorCopy
	if err := options.Catalog.ValidateContinuation(predecessor, predecessorMeta, successor); err != nil {
		return task.TaskRecord{}, err
	}
	profiles, err := options.Profiles.ForRecord(predecessorMeta.Environment, predecessorMeta.EnvironmentRecorded)
	if err != nil {
		return task.TaskRecord{}, err
	}
	options.Profiles = profiles
	if err = ValidateStatic(options, root, successor); err != nil {
		return task.TaskRecord{}, err
	}
	return successor, nil
}
