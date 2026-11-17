package contract

import (
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// MaintenanceQueue is an exact-label observation supplied under the caller's
// supervisor generation exclusion. Zero means no queue permission was observed.
type MaintenanceQueueState string

const (
	MaintenanceQueueAbsent MaintenanceQueueState = "absent"
	MaintenanceQueueEnded  MaintenanceQueueState = "ended"
)

type MaintenanceQueue struct {
	Label         string
	State         MaintenanceQueueState
	NumericTaskID *int64
}

// MaintenanceDispositionSubject binds immutable inspection evidence. Current
// transport and numeric queue addresses deliberately do not own permission.
type MaintenanceDispositionSubject struct {
	RootID         string `json:"root_id"`
	RootDevice     uint64 `json:"root_device"`
	RootInode      uint64 `json:"root_inode"`
	OperationID    string `json:"operation_id"`
	RequestSHA256  string `json:"request_sha256"`
	EvidenceSHA256 string `json:"evidence_sha256"`
	BootID         string `json:"boot_id"`
	Permission     string `json:"permission"`
}
type MaintenanceDispositionRecord struct {
	SchemaVersion int                           `json:"schema_version"`
	Subject       MaintenanceDispositionSubject `json:"subject"`
	Challenge     string                        `json:"challenge"`
	ConfirmedAt   string                        `json:"confirmed_at"`
}

func MaintenanceDispositionChallenge(subject MaintenanceDispositionSubject) (string, error) {
	if subject.RootInode == 0 || !validText(subject.BootID) || subject.Permission != task.OperatorPermissionSupervisorMaintenance {
		return "", task.ErrIdentityMismatch
	}
	if err := task.ValidateRootID(subject.RootID); err != nil {
		return "", err
	}
	if err := task.ValidateTaskID(subject.OperationID); err != nil {
		return "", err
	}
	for _, digest := range []string{subject.RequestSHA256, subject.EvidenceSHA256} {
		if err := task.ValidateSHA256(digest); err != nil {
			return "", err
		}
	}
	data, err := task.MarshalCanonical(subject)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(append([]byte("delegation-layer:inspection-maintenance:v1\n"), data...)), nil
}

func ValidateMaintenanceDisposition(record *MaintenanceDispositionRecord) error {
	if record == nil || record.SchemaVersion != task.SchemaVersion {
		return task.ErrUnsupportedSchema
	}
	challenge, err := MaintenanceDispositionChallenge(record.Subject)
	if err != nil {
		return err
	}
	if record.Challenge != challenge {
		return fmt.Errorf("%w: inspection settlement challenge", task.ErrIdentityMismatch)
	}
	_, err = CanonicalTimestamp("inspection settlement", record.ConfirmedAt)
	return err
}
