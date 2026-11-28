package policy

import (
	"errors"
	"fmt"
	"time"
)

// UnlimitedBudget is the only spelling of an execution budget without a
// wall-clock bound. Matching is exact; no case or whitespace variant is accepted.
const UnlimitedBudget = "unlimited"

// Budgets that releases before explicit execution budgets applied when a
// request omitted one. Only LegacyOmittedBudget reads them.
const (
	legacyOmittedBudget            = 30 * time.Minute
	legacyOmittedCodexReviewBudget = time.Hour
)

var (
	ErrInvalidBudget = errors.New(`budget must be a finite positive duration or "unlimited"`)
	// ErrBudgetRequired reports an omitted budget. It also matches
	// ErrInvalidBudget so existing classification keeps one budget category.
	ErrBudgetRequired = fmt.Errorf("%w: an explicit budget is required", ErrInvalidBudget)
)

// ExecutionBudget is an immutable, validated wall-clock budget for one
// execution: either a finite positive duration or unlimited. The zero value is
// not a budget; it means none was supplied and reports IsZero. Each budget has
// one representation, so values compare with ==.
type ExecutionBudget struct {
	duration  time.Duration
	unlimited bool
}

// FiniteExecutionBudget returns a finite budget. The duration must be positive.
func FiniteExecutionBudget(duration time.Duration) (ExecutionBudget, error) {
	if duration <= 0 {
		return ExecutionBudget{}, ErrInvalidBudget
	}
	return ExecutionBudget{duration: duration}, nil
}

// UnlimitedExecutionBudget returns the budget with no wall-clock bound.
func UnlimitedExecutionBudget() ExecutionBudget {
	return ExecutionBudget{unlimited: true}
}

// ParseExecutionBudget parses supplied budget text: a Go duration greater than
// zero, or exactly UnlimitedBudget. Empty text is an omission, not unlimited,
// and returns ErrBudgetRequired. No default is applied.
func ParseExecutionBudget(text string) (ExecutionBudget, error) {
	switch text {
	case "":
		return ExecutionBudget{}, ErrBudgetRequired
	case UnlimitedBudget:
		return UnlimitedExecutionBudget(), nil
	}
	duration, err := time.ParseDuration(text)
	if err != nil {
		return ExecutionBudget{}, fmt.Errorf("%w: %w", ErrInvalidBudget, err)
	}
	return FiniteExecutionBudget(duration)
}

// LegacyOmittedBudget returns the budget that releases before explicit budgets
// applied when a request omitted one: one hour for Codex review and thirty
// minutes otherwise. It exists only to resolve inputs saved under those
// semantics, such as schedules saved without a budget. New execution
// requests must supply a budget.
func LegacyOmittedBudget(provider string) ExecutionBudget {
	if provider == ProviderCodexReview {
		return ExecutionBudget{duration: legacyOmittedCodexReviewBudget}
	}
	return ExecutionBudget{duration: legacyOmittedBudget}
}

// IsZero reports that no budget was supplied.
func (b ExecutionBudget) IsZero() bool { return b == ExecutionBudget{} }

// Unlimited reports whether the budget has no wall-clock bound.
func (b ExecutionBudget) Unlimited() bool { return b.unlimited }

// Finite returns the bound of a finite budget. It reports false for an
// unlimited or zero budget.
func (b ExecutionBudget) Finite() (time.Duration, bool) {
	return b.duration, b.duration > 0
}

// String returns the canonical persisted text: the Go duration spelling for a
// finite budget, UnlimitedBudget, or empty for the zero value.
func (b ExecutionBudget) String() string {
	if b.unlimited {
		return UnlimitedBudget
	}
	if b.duration > 0 {
		return b.duration.String()
	}
	return ""
}

// Nanos returns the derived durable nanosecond projection: the bound of a
// finite budget, or zero for an unlimited or zero budget. Zero is never a
// finite budget, so readers that require a positive value fail closed.
func (b ExecutionBudget) Nanos() int64 { return int64(b.duration) }

// Encloses reports whether a duration fits inside the budget. An unlimited
// budget encloses every duration; the zero value encloses none.
func (b ExecutionBudget) Encloses(duration time.Duration) bool {
	if b.unlimited {
		return true
	}
	return b.duration > 0 && duration <= b.duration
}

// BudgetIntent distinguishes fresh explicit input from a schedule saved under
// the historical omission contract. Only the scheduler adapter selects legacy.
type BudgetIntent uint8

const (
	ExplicitBudget BudgetIntent = iota
	LegacyScheduleBudget
)

func ResolveExecutionBudget(provider, text string, intent BudgetIntent) (ExecutionBudget, error) {
	switch intent {
	case ExplicitBudget:
		return ParseExecutionBudget(text)
	case LegacyScheduleBudget:
		if text == "" {
			return LegacyOmittedBudget(provider), nil
		}
		return ParseExecutionBudget(text)
	}
	return ExecutionBudget{}, ErrInvalidBudget
}
