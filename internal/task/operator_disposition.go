package task

import "fmt"

const OperatorPermissionSupervisorMaintenance = "allow_supervisor_maintenance"

// OperatorDispositionSubject binds the immutable task evidence for one human
// decision. Current queue transports and observation times are deliberately
// absent: completed same-boot handoffs do not revoke the task's permission.
type OperatorDispositionSubject struct {
	RootDevice         uint64            `json:"root_device"`
	RootInode          uint64            `json:"root_inode"`
	Receipt            SupervisorReceipt `json:"receipt"`
	IntentSHA256       string            `json:"intent_sha256"`
	RegistrationSHA256 string            `json:"registration_sha256"`
	WorkerSHA256       string            `json:"worker_sha256"`
	LaunchSetSHA256    string            `json:"launch_set_sha256"`
	BootID             string            `json:"boot_id"`
	Permission         string            `json:"permission"`
}

// OperatorDispositionRecord grants only supervisor maintenance. It is never
// a native lifetime disposition, publication, result or resource release.
type OperatorDispositionRecord struct {
	SchemaVersion int                        `json:"schema_version"`
	Subject       OperatorDispositionSubject `json:"subject"`
	Challenge     string                     `json:"challenge"`
	ConfirmedAt   string                     `json:"confirmed_at"`
}

// OperatorDispositionQueue is a caller-owned exact, ended queue observation.
// The supervisor owner validates its current generation mapping separately;
// taskdir matches the row against the task's immutable receipt under exclusions.
type OperatorDispositionQueue struct {
	NumericTaskID int64
	Label         string
	Ended         bool
}

func OperatorDispositionChallenge(subject OperatorDispositionSubject) (string, error) {
	if err := validateOperatorDispositionSubject(subject); err != nil {
		return "", err
	}
	data, err := MarshalCanonical(subject)
	if err != nil {
		return "", err
	}
	return ComputeSHA256(append([]byte("delegation-layer:operator-disposition:v1\n"), data...)), nil
}

func validateOperatorDispositionSubject(subject OperatorDispositionSubject) error {
	if subject.RootInode == 0 || subject.Permission != OperatorPermissionSupervisorMaintenance || !validLifetimeToken(subject.BootID) {
		return ErrIdentityMismatch
	}
	if err := ValidateSupervisorReceipt(&subject.Receipt); err != nil {
		return err
	}
	for _, digest := range []string{subject.IntentSHA256, subject.RegistrationSHA256, subject.WorkerSHA256, subject.LaunchSetSHA256} {
		if err := ValidateSHA256(digest); err != nil {
			return err
		}
	}
	return nil
}

func ValidateOperatorDispositionRecord(record *OperatorDispositionRecord) error {
	if record == nil || record.SchemaVersion != SchemaVersion {
		return ErrUnsupportedSchema
	}
	challenge, err := OperatorDispositionChallenge(record.Subject)
	if err != nil {
		return err
	}
	if challenge != record.Challenge {
		return fmt.Errorf("%w: operator disposition challenge", ErrIdentityMismatch)
	}
	return validateTimestamp(record.ConfirmedAt)
}
