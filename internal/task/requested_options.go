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
// accepted; Interactive and provider support are decided by the caller and the
// provider catalog. The native timeout is compared with the budget only when
// both are supplied, because an omitted budget takes a provider-specific
// default that this function cannot know. The request is never modified.
func ValidateRequestedOptions(request TaskConfig) error {
	if request.Permission != "" {
		if err := policy.ValidateMode(request.Permission); err != nil {
			return err
		}
	}
	var budget time.Duration
	if request.Budget != "" {
		var err error
		if budget, err = policy.ParseBudget(request.Budget); err != nil {
			return err
		}
	}
	if request.NativeTimeout != "" {
		timeout, err := parseNativeTimeout(request.NativeTimeout)
		if err != nil {
			return err
		}
		if request.Budget != "" {
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

// checkNativeTimeoutBudget requires a parsed native timeout not to exceed the
// task budget.
func checkNativeTimeoutBudget(timeout, budget time.Duration) error {
	if timeout > budget {
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
