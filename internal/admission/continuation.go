package admission

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// validateContinuationAdmission constructs the exact successor identity and
// applies the shared static admission gate before any continuation recovery.
// The saved launch environment is used only while compiling the provider
// candidate; supervisor control keeps the caller's own environment.
func ValidateContinuation(options StaticOptions, applyEnvironment func([]string, bool) (func() error, error), root string, predecessor task.TaskRecord, predecessorMeta task.MetaRecord, prior *task.PriorSession, successor task.TaskRecord) (task.TaskRecord, error) {
	if prior == nil {
		return task.TaskRecord{}, errors.New("predecessor session is unavailable")
	}
	priorCopy := *prior
	successor.PriorSession = &priorCopy
	if err := options.Catalog.ValidateContinuation(predecessor, predecessorMeta, successor); err != nil {
		return task.TaskRecord{}, err
	}
	if err := validateStaticContinuationAdmission(options, applyEnvironment, root, predecessorMeta, successor); err != nil {
		return task.TaskRecord{}, err
	}
	return successor, nil
}

func validateStaticContinuationAdmission(options StaticOptions, applyEnvironment func([]string, bool) (func() error, error), root string, predecessorMeta task.MetaRecord, successor task.TaskRecord) (resultErr error) {
	if applyEnvironment == nil {
		return task.ErrEvidenceFault
	}
	restore, err := applyEnvironment(predecessorMeta.Environment, predecessorMeta.EnvironmentRecorded)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, restore()) }()
	return ValidateStatic(options, root, successor)
}
