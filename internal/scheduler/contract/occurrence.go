package contract

import (
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Claim asks to reserve the occurrence of ScheduleID at Due for the caller's
// new TaskID. One transaction inserts the armed occurrence and advances the
// cursor from ExpectedCursor to Due, so Due must be after ExpectedCursor.
type Claim struct {
	ScheduleID     string
	ExpectedCursor time.Time
	Due            time.Time
	TaskID         string
	ArmedAt        time.Time
}

// ValidateClaim checks a claim before the store attempts it.
func ValidateClaim(claim Claim) error {
	if err := validateScheduleID(claim.ScheduleID); err != nil {
		return err
	}
	if err := task.ValidateTaskID(claim.TaskID); err != nil {
		return fmt.Errorf("occurrence task id: %w", err)
	}
	if err := ValidateInstant("claim expected cursor", claim.ExpectedCursor); err != nil {
		return err
	}
	if err := ValidateInstant("occurrence due", claim.Due); err != nil {
		return err
	}
	if !claim.Due.After(claim.ExpectedCursor) {
		return errors.New("occurrence due time must be after the expected cursor")
	}
	return ValidateInstant("occurrence armed_at", claim.ArmedAt)
}

// ClaimOutcome classifies a claim attempt. The zero value is ClaimUnknown, so
// an unassigned outcome never authorizes anything.
type ClaimOutcome uint8

const (
	// ClaimUnknown: the commit outcome is uncertain or the attempt failed.
	ClaimUnknown ClaimOutcome = iota
	// ClaimExisting: the due time is already claimed; TaskID names that
	// occurrence for observation only.
	ClaimExisting
	// ClaimNewlyCommitted: this call committed the claim for the requested TaskID.
	ClaimNewlyCommitted
)

// ClaimResult is the store's answer to a claim.
type ClaimResult struct {
	Outcome ClaimOutcome
	TaskID  string
}

// AuthorizesInvocation reports whether the caller may make its one downstream
// invocation. Only a claim this call newly committed does; an existing or
// uncertain claim never does, even when a later read shows the caller's row.
func (r ClaimResult) AuthorizesInvocation() bool {
	switch r.Outcome {
	case ClaimNewlyCommitted:
		return true
	case ClaimUnknown, ClaimExisting:
		return false
	default:
		return false
	}
}

// Occurrence is one armed reservation, keyed by its task ID. The task's own
// files stay authoritative for its outcome; Observation is only a cache.
type Occurrence struct {
	TaskID      string
	ScheduleID  string
	Due         time.Time
	ArmedAt     time.Time
	Observation *Observation
	Attempt     *AttemptFailure
	// Release is nil while the occurrence is the schedule's active reservation.
	Release *Release
}

// ValidateOccurrence checks a stored occurrence.
func ValidateOccurrence(o Occurrence) error {
	if err := task.ValidateTaskID(o.TaskID); err != nil {
		return fmt.Errorf("occurrence task id: %w", err)
	}
	if err := validateScheduleID(o.ScheduleID); err != nil {
		return err
	}
	if err := ValidateInstant("occurrence due", o.Due); err != nil {
		return err
	}
	if err := ValidateInstant("occurrence armed_at", o.ArmedAt); err != nil {
		return err
	}
	if o.Observation != nil {
		if err := ValidateObservation(*o.Observation); err != nil {
			return err
		}
	}
	if o.Attempt != nil {
		if err := ValidateAttempt(*o.Attempt); err != nil {
			return err
		}
	}
	if o.Release != nil {
		return ValidateRelease(*o.Release)
	}
	return nil
}

// clone returns an occurrence whose optional facts no longer alias the input.
func (o Occurrence) clone() Occurrence {
	if o.Observation != nil {
		observation := *o.Observation
		o.Observation = &observation
	}
	if o.Attempt != nil {
		attempt := *o.Attempt
		o.Attempt = &attempt
	}
	if o.Release != nil {
		release := *o.Release
		o.Release = &release
	}
	return o
}
