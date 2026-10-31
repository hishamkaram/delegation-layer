package contract

import (
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// SkipReason is the closed reason a span of due times was not claimed.
type SkipReason string

const (
	// SkipOverlapActive: a known admitted occurrence of the schedule was still active.
	SkipOverlapActive SkipReason = "overlap_active"
	// SkipBlockedUncertain: an earlier occurrence's launch or lifetime is unresolved.
	SkipBlockedUncertain SkipReason = "blocked_uncertain"
	// SkipMissedUnavailable: the scheduler was stopped, asleep, or stalled.
	SkipMissedUnavailable SkipReason = "missed_unavailable"
	// SkipMissedCapacity: no admission worker became available in time.
	SkipMissedCapacity SkipReason = "missed_capacity"
	// SkipPaused: the schedule was paused; recorded only when it is resumed.
	SkipPaused SkipReason = "paused"
)

// ValidateSkipReason accepts only the closed reason values.
func ValidateSkipReason(reason SkipReason) error {
	switch reason {
	case SkipOverlapActive, SkipBlockedUncertain, SkipMissedUnavailable, SkipMissedCapacity, SkipPaused:
		return nil
	default:
		return fmt.Errorf("%w: skip reason %q", task.ErrInvalidEnum, reason)
	}
}

// SkipSpan is a compact (From, To] range of unclaimed due times. From is the
// cursor an earlier claim or span already accounted for, so it is excluded; To
// becomes the cursor and is included, because a claim must be after the cursor.
type SkipSpan struct {
	From   time.Time
	To     time.Time
	Reason SkipReason
}

// Covers reports whether a due time lies in the span (From, To].
func (s SkipSpan) Covers(due time.Time) bool {
	return due.After(s.From) && !due.After(s.To)
}

// Skip records a span and advances the cursor from ExpectedCursor to Span.To in
// one atomic compare-and-set. Paused spans are recorded only by resume.
type Skip struct {
	ScheduleID     string
	ExpectedCursor time.Time
	Span           SkipSpan
}

// ValidateSkip checks a skip before it is recorded.
func ValidateSkip(skip Skip) error {
	if err := validateScheduleID(skip.ScheduleID); err != nil {
		return err
	}
	if skip.Span.Reason == SkipPaused {
		return errors.New("paused skip spans are recorded only by resuming the schedule")
	}
	if err := ValidateSkipSpan(skip.Span); err != nil {
		return err
	}
	return validateCursorAdvance(skip.ExpectedCursor, skip.Span.From)
}

// ValidateSkipSpan checks a stored span.
func ValidateSkipSpan(span SkipSpan) error {
	if err := ValidateSkipReason(span.Reason); err != nil {
		return err
	}
	if err := ValidateInstant("skip from", span.From); err != nil {
		return err
	}
	if err := ValidateInstant("skip to", span.To); err != nil {
		return err
	}
	if !span.To.After(span.From) {
		return errors.New("skip span must end after it starts")
	}
	return nil
}

// validateCursorAdvance requires a span to start at or after the expected cursor,
// so recorded spans never overlap.
func validateCursorAdvance(expectedCursor, from time.Time) error {
	if err := ValidateInstant("expected cursor", expectedCursor); err != nil {
		return err
	}
	if from.Before(expectedCursor) {
		return errors.New("skip span starts before the expected cursor")
	}
	return nil
}
