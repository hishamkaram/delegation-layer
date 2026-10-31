// Package recurrence adapts gocron's calendar lookup to Delegate's minute,
// timezone and first-realization policy. It owns no timers or persisted state.
package recurrence

import "errors"

var (
	ErrInvalidExpression = errors.New("invalid schedule expression")
	ErrInvalidTimezone   = errors.New("invalid schedule timezone")
	ErrLocalTimezone     = errors.New("local timezone requires an explicit named timezone")
	ErrRangeExhausted    = errors.New("no occurrence in the supported instant range")
	ErrLookup            = errors.New("schedule lookup fault")
)

// Warning is a bounded policy advisory, not a prediction of future zone rules.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
