package task

import (
	"errors"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config/policy"
)

var errNativeTimeoutBounds = errors.New("native timeout must be positive and no greater than the task budget")

// ValidateRequestedOptions checks the syntax of the options a caller supplied
// without choosing a provider or applying any default. Omitted fields are
// accepted because caller intent keeps omission distinct; Interactive and
// provider support are decided by the caller and the provider catalog. A
// supplied budget is a finite positive duration or "unlimited". The native
// timeout is compared with the budget only when both are supplied: a finite
// budget bounds it and an unlimited budget has no enclosing bound. Concrete
// normalization requires the budget. The request is never modified.
func ValidateRequestedOptions(request TaskConfig) error {
	if _, err := ParseOutputSilenceTimeout(request.OutputSilenceTimeout); err != nil {
		return err
	}
	if request.Permission != "" {
		if err := policy.ValidateMode(request.Permission); err != nil {
			return err
		}
	}
	var budget policy.ExecutionBudget
	if request.Budget != "" {
		var err error
		if budget, err = policy.ParseExecutionBudget(request.Budget); err != nil {
			return err
		}
	}
	if request.NativeTimeout != "" {
		timeout, err := parseNativeTimeout(request.NativeTimeout)
		if err != nil {
			return err
		}
		if !budget.IsZero() {
			if err := checkNativeTimeoutBudget(timeout, budget); err != nil {
				return err
			}
		}
	}
	if err := validateModelEffort(request.Model, request.Effort); err != nil {
		return err
	}
	return ValidateReviewTarget(request.ReviewTarget)
}

// parseNativeTimeout parses a supplied native timeout and requires it to be
// positive. The budget bound is applied separately because the budget may not
// be known yet.
func parseNativeTimeout(value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, errNativeTimeoutBounds
	}
	return duration, nil
}

// checkNativeTimeoutBudget requires a parsed native timeout to fit inside the
// task budget. An unlimited budget encloses every positive timeout.
func checkNativeTimeoutBudget(timeout time.Duration, budget policy.ExecutionBudget) error {
	if !budget.Encloses(timeout) {
		return errNativeTimeoutBounds
	}
	return nil
}

// validateModelEffort requires each supplied literal to be unpadded and free of
// NUL bytes. Empty values are omitted values.
func validateModelEffort(model, effort string) error {
	for _, value := range []string{model, effort} {
		if strings.TrimSpace(value) != value || strings.ContainsRune(value, '\x00') {
			return errors.New("model and effort must be nonblank literal values when supplied")
		}
	}
	return nil
}
