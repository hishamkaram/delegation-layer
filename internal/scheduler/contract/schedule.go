package contract

import (
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// State is a schedule's mutable lifecycle state. Deleted is terminal.
type State string

const (
	StateEnabled State = "enabled"
	StatePaused  State = "paused"
	StateDeleted State = "deleted"
)

// ValidateState accepts only the closed state values.
func ValidateState(state State) error {
	switch state {
	case StateEnabled, StatePaused, StateDeleted:
		return nil
	default:
		return fmt.Errorf("%w: schedule state %q", task.ErrInvalidEnum, state)
	}
}

// Schedule is a full read: the immutable definition plus its mutable state and
// cursor. The cursor is the exclusive lower bound for the next claimed due time
// and only moves forward.
type Schedule struct {
	Definition Definition
	State      State
	Cursor     time.Time
}

// ScheduleSummary is a metadata-only read. It never carries the brief.
type ScheduleSummary struct {
	Header      DefinitionHeader
	Digests     Digests
	BriefLength int64
	State       State
	Cursor      time.Time
}

// InitialSchedule is the only valid newly created schedule: enabled, with its
// cursor at the definition's creation time. It owns a copy of the definition.
func InitialSchedule(d Definition) (Schedule, error) {
	if err := ValidateDefinition(d); err != nil {
		return Schedule{}, err
	}
	return Schedule{Definition: d.Clone(), State: StateEnabled, Cursor: d.CreatedAt}, nil
}

// ValidateSchedule checks a full read.
func ValidateSchedule(s Schedule) error {
	if err := ValidateDefinition(s.Definition); err != nil {
		return err
	}
	return validateMutable(s.Definition.CreatedAt, s.State, s.Cursor)
}

// ValidateSummary checks a metadata-only read, including that its definition
// digest binds the header and brief digest. A mismatch is an invariant fault.
func ValidateSummary(s ScheduleSummary) error {
	if err := validateBriefLength(s.BriefLength); err != nil {
		return err
	}
	definitionSHA256, err := HeaderSHA256(s.Header, s.Digests.BriefSHA256)
	if err != nil {
		return err
	}
	if definitionSHA256 != s.Digests.DefinitionSHA256 {
		return fmt.Errorf("%w: schedule %s definition digest does not match its metadata", task.ErrInvariantFault, s.Header.ScheduleID)
	}
	return validateMutable(s.Header.CreatedAt, s.State, s.Cursor)
}

func validateMutable(createdAt time.Time, state State, cursor time.Time) error {
	if err := ValidateState(state); err != nil {
		return err
	}
	if err := ValidateInstant("schedule cursor", cursor); err != nil {
		return err
	}
	if cursor.Before(createdAt) {
		return errors.New("schedule cursor precedes the schedule creation time")
	}
	return nil
}
