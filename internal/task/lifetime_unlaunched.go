package task

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const LifetimeUnlaunchedCauseDeadlineExpired = "deadline-expired"

var (
	ErrLifetimeUnlaunched         = errors.New("lifetime was sealed as unlaunched")
	ErrLifetimeDeadlineNotExpired = errors.New("lifetime deadline has not expired")
)

// LifetimeUnlaunchedRecord is the immutable proof that an auxiliary lifetime
// never reached native intent publication before its deadline expired. It is
// deliberately separate from native termination evidence: no scope or worker
// existed for this proof to terminate or wait for.
type LifetimeUnlaunchedRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RootID        string `json:"root_id"`
	TaskID        string `json:"task_id"`
	OwnerKind     string `json:"owner_kind"`
	OwnerSHA256   string `json:"owner_sha256"`
	Deadline      string `json:"deadline"`
	Cause         string `json:"cause"`
	ObservedAt    string `json:"observed_at"`
}

// ValidateLifetimeUnlaunchedRecord validates the complete immutable fence
// shape, including the ordering that makes deadline expiry observable proof.
func ValidateLifetimeUnlaunchedRecord(record *LifetimeUnlaunchedRecord) error {
	if record == nil {
		return errors.New("nil lifetime unlaunched record")
	}
	if err := validateLifetimeIdentity(record.SchemaVersion, record.RootID, record.TaskID); err != nil {
		return err
	}
	if err := validateLifetimeOwner(record.OwnerKind, record.OwnerSHA256); err != nil {
		return err
	}
	if record.OwnerKind != LifetimeOwnerWorkspace && record.OwnerKind != LifetimeOwnerInspection {
		return ErrInvalidEnum
	}
	if record.Cause != LifetimeUnlaunchedCauseDeadlineExpired {
		return fmt.Errorf("invalid lifetime unlaunched cause: %w", ErrInvalidEnum)
	}
	deadline, err := validateLifetimeUnlaunchedTimestamp("lifetime unlaunched deadline", record.Deadline)
	if err != nil {
		return err
	}
	observedAt, err := validateLifetimeUnlaunchedTimestamp("lifetime unlaunched observed_at", record.ObservedAt)
	if err != nil {
		return err
	}
	if observedAt.Before(deadline) {
		return fmt.Errorf("lifetime unlaunched observation precedes deadline: %w", ErrIdentityMismatch)
	}
	return nil
}

func validateLifetimeUnlaunchedTimestamp(name, value string) (time.Time, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsRune(value, '\x00') {
		return time.Time{}, fmt.Errorf("missing %s", name)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s: %w", name, err)
	}
	if parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, fmt.Errorf("%s is not canonical UTC RFC3339Nano", name)
	}
	if parsed.IsZero() {
		return time.Time{}, fmt.Errorf("%s is zero: %w", name, ErrIdentityMismatch)
	}
	return parsed, nil
}
