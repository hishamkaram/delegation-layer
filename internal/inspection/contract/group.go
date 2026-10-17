package contract

import (
	"errors"
	"fmt"
	"time"

	pueue "github.com/hishamkaram/delegation-layer/internal/pueue/protocol"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var (
	// ErrGroupMigrationRequired means an existing inspection journal belongs
	// to another supervisor and cannot be safely rebound automatically.
	ErrGroupMigrationRequired = errors.New("inspection group migration required")
	// ErrGroupMigrationBusy means the old supervisor still owns queued or
	// running inspection work, so a new generation must not be opened.
	ErrGroupMigrationBusy = errors.New("inspection group migration has active work")
	// ErrGroupMigrationUnsupported means the journal is bound to an external
	// supervisor that Delegate must never stop or migrate automatically.
	ErrGroupMigrationUnsupported = errors.New("inspection group migration is unsupported")
)

// GroupRequestRecord is the immutable root-scoped group binding. The group
// name and parallelism are derived by this package; callers cannot select a
// broader group or change its scheduling width through this record.
type GroupRequestRecord struct {
	SchemaVersion int                `json:"schema_version"`
	RootID        string             `json:"root_id"`
	Supervisor    task.SupervisorRef `json:"supervisor"`
	Name          string             `json:"name"`
	ParallelTasks uint64             `json:"parallel_tasks"`
}

// GroupMigrationRecord is an immutable prepare/complete marker for a
// supervisor generation change. The old group request remains the authority;
// these records only make a crash between supervisor transitions observable.
type GroupMigrationRecord struct {
	SchemaVersion   int                `json:"schema_version"`
	RootID          string             `json:"root_id"`
	LegacySHA256    string             `json:"legacy_sha256"`
	SuccessorSHA256 string             `json:"successor_sha256"`
	Successor       GroupRequestRecord `json:"successor"`
}

// GroupCreationRecord is the durable create-once intent for one supervisor
// group creation. Its existence never grants a second create permit.
type GroupCreationRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RequestSHA256 string `json:"request_sha256"`
	CreatedAt     string `json:"created_at"`
}

// GroupObservationRecord contains only fresh, bounded supervisor facts that
// prove the expected group is currently usable. There is intentionally no
// timestamp: a stored observation is never a substitute for a fresh snapshot.
type GroupObservationRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RequestSHA256 string `json:"request_sha256"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	ParallelTasks uint64 `json:"parallel_tasks"`
}

func GroupGeneration(binding task.SupervisorRef) string {
	identity := struct {
		ConfigPath           string `json:"config_path"`
		ConfigDigest         string `json:"config_digest"`
		ResolvedConfigSHA256 string `json:"resolved_config_sha256"`
		Endpoint             string `json:"endpoint"`
	}{
		ConfigPath:           binding.ConfigPath,
		ConfigDigest:         binding.ConfigDigest,
		ResolvedConfigSHA256: binding.ResolvedConfigSHA256,
		Endpoint:             binding.Endpoint,
	}
	bytes, err := task.MarshalCanonical(identity)
	if err != nil {
		return "0000000000000000"
	}
	return task.ComputeSHA256(bytes)[:16]
}

func ValidateGroupRequest(record GroupRequestRecord, rootID string, binding task.SupervisorRef) error {
	if err := ValidateStoredGroupRequest(record, rootID); err != nil {
		return err
	}
	if !task.SameSupervisorIdentity(record.Supervisor, binding) {
		return task.ErrEvidenceFault
	}
	return nil
}

func ValidateStoredGroupRequest(record GroupRequestRecord, rootID string) error {
	if record.SchemaVersion != task.SchemaVersion {
		return fmt.Errorf("%w: group request schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	if err := task.ValidateRootID(record.RootID); err != nil {
		return err
	}
	if record.RootID != rootID {
		return task.ErrIdentityMismatch
	}
	if err := task.ValidateFreshSupervisorRef(record.Supervisor); err != nil {
		return err
	}
	if record.Name != GroupForRoot(rootID) {
		return task.ErrIdentityMismatch
	}
	if record.ParallelTasks != 1 {
		return task.ErrInvalidEnum
	}
	return nil
}

func SameGroupRequest(left, right GroupRequestRecord) bool {
	return left.SchemaVersion == right.SchemaVersion &&
		left.RootID == right.RootID &&
		task.SameSupervisorIdentity(left.Supervisor, right.Supervisor) &&
		left.Name == right.Name &&
		left.ParallelTasks == right.ParallelTasks
}

func ValidGroupTime(now time.Time) (string, error) {
	text := CanonicalNow(now)
	if text == "" {
		return "", ErrInvalidAdmissionTime
	}
	parsed, err := CanonicalTimestamp("group creation", text)
	if err != nil || !parsed.Equal(now.UTC()) {
		return "", ErrInvalidAdmissionTime
	}
	return text, nil
}

func ValidateGroupCreation(record GroupCreationRecord, requestSHA string) error {
	if record.SchemaVersion != task.SchemaVersion {
		return fmt.Errorf("%w: group creation schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	if record.RequestSHA256 != requestSHA {
		return task.ErrEvidenceFault
	}
	if err := task.ValidateSHA256(record.RequestSHA256); err != nil {
		return err
	}
	_, err := CanonicalTimestamp("group creation", record.CreatedAt)
	return err
}

func GroupObservationForSnapshot(request GroupRequestRecord, requestSHA string, snapshot pueue.QueueSnapshot) (GroupObservationRecord, error) {
	group, ok := snapshot.Groups[request.Name]
	if !ok || group.Status != "Running" || group.ParallelTasks != 1 {
		return GroupObservationRecord{}, pueue.ErrUnknown
	}
	record := GroupObservationRecord{
		SchemaVersion: task.SchemaVersion,
		RequestSHA256: requestSHA,
		Name:          request.Name,
		Status:        group.Status,
		ParallelTasks: group.ParallelTasks,
	}
	if err := validateGroupObservation(record, requestSHA, request.Name); err != nil {
		return GroupObservationRecord{}, err
	}
	return record, nil
}

func validateGroupObservation(record GroupObservationRecord, requestSHA, name string) error {
	if record.SchemaVersion != task.SchemaVersion {
		return fmt.Errorf("%w: group observation schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	if err := task.ValidateSHA256(record.RequestSHA256); err != nil {
		return err
	}
	if record.RequestSHA256 != requestSHA || record.Name != name || record.Status != "Running" || record.ParallelTasks != 1 {
		return task.ErrEvidenceFault
	}
	return nil
}
