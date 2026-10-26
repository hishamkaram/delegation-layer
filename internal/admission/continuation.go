package admission

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ValidateContinuation constructs the exact successor identity and applies
// the shared static admission gate before any continuation recovery. The
// predecessor's immutable records decide only provider compatibility; the
// provider candidate is compiled in the successor's own launch scope, the one
// options.Profiles already carries. A predecessor environment is never a
// fallback.
func ValidateContinuation(options StaticOptions, root string, predecessor task.TaskRecord, predecessorMeta task.MetaRecord, prior *task.PriorSession, successor task.TaskRecord) (task.TaskRecord, error) {
	if prior == nil {
		return task.TaskRecord{}, errors.New("predecessor session is unavailable")
	}
	priorCopy := *prior
	successor.PriorSession = &priorCopy
	if err := options.Catalog.ValidateContinuation(predecessor, predecessorMeta, successor); err != nil {
		return task.TaskRecord{}, err
	}
	if err := ValidateStatic(options, root, successor); err != nil {
		return task.TaskRecord{}, err
	}
	return successor, nil
}
