package contract

import (
	"errors"
	"fmt"
	"time"
)

// StateChange is a compare-and-set of a schedule's state from From to To. It
// never cancels or releases an active occurrence.
type StateChange struct {
	ScheduleID string
	From       State
	To         State
	// Resume optionally records the paused span when a paused schedule is
	// enabled again, advancing the cursor in the same transaction. It is
	// permitted only for paused to enabled.
	Resume *Resume
}

// Resume is the paused span (From, To] recorded on resume; To becomes the cursor.
type Resume struct {
	ExpectedCursor time.Time
	From           time.Time
	To             time.Time
}

// Span returns the skip span a resume records.
func (r Resume) Span() SkipSpan {
	return SkipSpan{From: r.From, To: r.To, Reason: SkipPaused}
}

// ValidateStateChange accepts only permitted transitions. Deleted is terminal.
func ValidateStateChange(change StateChange) error {
	if err := validateScheduleID(change.ScheduleID); err != nil {
		return err
	}
	if err := ValidateState(change.From); err != nil {
		return err
	}
	if err := ValidateState(change.To); err != nil {
		return err
	}
	if !permittedTransition(change.From, change.To) {
		return fmt.Errorf("schedule state cannot change from %s to %s", change.From, change.To)
	}
	if change.Resume == nil {
		return nil
	}
	if change.From != StatePaused || change.To != StateEnabled {
		return errors.New("only resuming a paused schedule records a paused span")
	}
	if err := ValidateSkipSpan(change.Resume.Span()); err != nil {
		return err
	}
	return validateCursorAdvance(change.Resume.ExpectedCursor, change.Resume.From)
}

func permittedTransition(from, to State) bool {
	switch from {
	case StateEnabled:
		return to == StatePaused || to == StateDeleted
	case StatePaused:
		return to == StateEnabled || to == StateDeleted
	case StateDeleted:
		return false
	default:
		return false
	}
}
