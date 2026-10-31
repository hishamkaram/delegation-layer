package contract

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// codePattern is the stored shape of fault and not-created codes. The closed
// code set stays with the existing public failure classifier; raw provider or
// error text is never stored.
var codePattern = regexp.MustCompile(`^[a-z_]{1,64}$`)

// Observation is a cached reading of the occurrence's task. It never authorizes
// a launch or overrides the task's files. An equal or older ObservedAt never
// replaces a stored observation.
type Observation struct {
	ObservedAt  time.Time
	Admission   task.Admission
	Liveness    task.Liveness
	Publication task.Publication
	// OutcomeSHA256 is the digest of outcome.json, present exactly when
	// Publication is committed or rejected.
	OutcomeSHA256 string
	// FaultCode optionally names a classified observation fault.
	FaultCode string
}

// OccurrenceObservation records an observation for an occurrence.
type OccurrenceObservation struct {
	TaskID      string
	Observation Observation
}

// ReleaseKind is the closed reason an occurrence released its reservation.
type ReleaseKind string

const (
	// ReleaseNotCreated: admission did not create the task; Ref is its code.
	ReleaseNotCreated ReleaseKind = "not_created"
	// ReleaseClosed: canonical execution closure or a lifecycle-owned permanent
	// fenced-never-started disposition; Ref is the validated existing record's
	// SHA-256. A reference alone does not prove either disposition.
	ReleaseClosed ReleaseKind = "closed"
)

// Release is the immutable fact that ends an occurrence's reservation.
type Release struct {
	ReleasedAt time.Time
	Kind       ReleaseKind
	Ref        string
}

// OccurrenceRelease conditionally releases an active occurrence.
type OccurrenceRelease struct {
	TaskID  string
	Release Release
}

// ValidateObservation checks an observation's closed values and shapes.
func ValidateObservation(observation Observation) error {
	if err := ValidateInstant("observation observed_at", observation.ObservedAt); err != nil {
		return err
	}
	if err := validateAdmission(observation.Admission); err != nil {
		return err
	}
	if err := validateLiveness(observation.Liveness); err != nil {
		return err
	}
	outcome, err := publicationHasOutcome(observation.Publication)
	if err != nil {
		return err
	}
	if !outcome && observation.OutcomeSHA256 != "" {
		return errors.New("observation outcome digest requires a committed or rejected publication")
	}
	if outcome {
		if err := task.ValidateSHA256(observation.OutcomeSHA256); err != nil {
			return fmt.Errorf("observation outcome digest: %w", err)
		}
	}
	if observation.FaultCode != "" {
		return validateCode("observation fault code", observation.FaultCode)
	}
	return nil
}

// ValidateOccurrenceObservation checks an observation before it is recorded.
func ValidateOccurrenceObservation(record OccurrenceObservation) error {
	if err := task.ValidateTaskID(record.TaskID); err != nil {
		return fmt.Errorf("occurrence task id: %w", err)
	}
	return ValidateObservation(record.Observation)
}

// ValidateRelease checks a release's kind and reference shape.
func ValidateRelease(release Release) error {
	if err := ValidateInstant("occurrence released_at", release.ReleasedAt); err != nil {
		return err
	}
	switch release.Kind {
	case ReleaseNotCreated:
		return validateCode("not-created release code", release.Ref)
	case ReleaseClosed:
		if err := task.ValidateSHA256(release.Ref); err != nil {
			return fmt.Errorf("closed release digest: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("%w: release kind %q", task.ErrInvalidEnum, release.Kind)
	}
}

// ValidateOccurrenceRelease checks a release before it is recorded.
func ValidateOccurrenceRelease(record OccurrenceRelease) error {
	if err := task.ValidateTaskID(record.TaskID); err != nil {
		return fmt.Errorf("occurrence task id: %w", err)
	}
	return ValidateRelease(record.Release)
}

// CheckRepeatedRelease decides a release that found the occurrence already
// released. The same kind and reference is an idempotent repeat; anything else
// conflicts with the immutable stored release.
func CheckRepeatedRelease(stored, requested Release) error {
	if stored.Kind == requested.Kind && stored.Ref == requested.Ref {
		return nil
	}
	return fmt.Errorf("%w: occurrence was already released as %s", ErrConflict, stored.Kind)
}

func validateCode(name, code string) error {
	if !codePattern.MatchString(code) {
		return fmt.Errorf("%s must match [a-z_]{1,64}", name)
	}
	return nil
}

func validateAdmission(admission task.Admission) error {
	switch admission {
	case task.AdmissionUnknown, task.AdmissionNotAdmitted, task.AdmissionAdmitted:
		return nil
	default:
		return fmt.Errorf("%w: observation admission %d", task.ErrInvalidEnum, admission)
	}
}

func validateLiveness(liveness task.Liveness) error {
	switch liveness {
	case task.LivenessUndetermined, task.LivenessRunning, task.LivenessEnded:
		return nil
	default:
		return fmt.Errorf("%w: observation liveness %d", task.ErrInvalidEnum, liveness)
	}
}

func publicationHasOutcome(publication task.Publication) (bool, error) {
	switch publication {
	case task.PublicationCommitted, task.PublicationRejected:
		return true, nil
	case task.PublicationUnknown, task.PublicationPending:
		return false, nil
	default:
		return false, fmt.Errorf("%w: observation publication %d", task.ErrInvalidEnum, publication)
	}
}
