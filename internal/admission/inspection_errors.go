package admission

import (
	"errors"
	"fmt"
	"os"
)

// annotateMissingInspectionStage gives a missing-file failure a fixed stage
// without exposing paths, records, or native diagnostics. The original error
// remains wrapped so callers can still classify errors.Is(os.ErrNotExist).
func AnnotateMissingInspectionStage(stage string, err error) error {
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fmt.Errorf("inspection %s: %w", stage, err)
}

// retryableInspectionStatusError preserves a completed but unusable control
// observation while the immutable admission clock still permits another exact
// read. Identity, evidence, and deadline failures never use this wrapper.
type retryableInspectionStatusError struct {
	cause error
}

func (err *retryableInspectionStatusError) Error() string {
	return err.cause.Error()
}

func (err *retryableInspectionStatusError) Unwrap() error {
	return err.cause
}

func inspectionStatusRetryError(err error) error {
	var retry *retryableInspectionStatusError
	if !errors.As(err, &retry) || retry == nil {
		return nil
	}
	return retry.cause
}

func classifiedInspectionStatusError(retry bool, err error) error {
	if !retry || err == nil {
		return err
	}
	return &retryableInspectionStatusError{cause: err}
}

// errInspectionWorkerUnavailable is the only failure returned by the worker.
// Inspection diagnostics can include native or provider-sensitive details, so
// neither those details nor supervisor command errors cross this boundary.
var ErrInspectionWorkerUnavailable = errors.New("native inspection unavailable")
