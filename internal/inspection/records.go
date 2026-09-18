package inspection

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	requestRecordName    = "request.json"
	submissionRecordName = "submission.json"
	startRecordName      = "start.json"
	groupPrefix          = "delegation-inspection-"

	// AdmissionTimeout includes queue time before the worker starts.
	AdmissionTimeout = 20 * time.Second
)

var (
	ErrAdmissionExpired     = errors.New("inspection admission deadline expired")
	ErrInvalidAdmissionTime = errors.New("invalid inspection admission time")
)

// Binding records the immutable inspection definition and all executable and
// supervisor coordinates needed to reproduce it.
type Binding struct {
	DefinitionRevision string             `json:"definition_revision"`
	DefinitionSHA256   string             `json:"definition_sha256"`
	HelperExecutable   string             `json:"helper_executable"`
	HelperSHA256       string             `json:"helper_sha256"`
	WorkerExecutable   string             `json:"worker_executable"`
	WorkerSHA256       string             `json:"worker_sha256"`
	Supervisor         task.SupervisorRef `json:"supervisor"`
}

// RequestRecord is the immutable admission request persisted outside ordinary
// task directories. Task is the complete normalized task request.
type RequestRecord struct {
	SchemaVersion int             `json:"schema_version"`
	RootID        string          `json:"root_id"`
	TaskID        string          `json:"task_id"`
	Task          task.TaskRecord `json:"task"`
	TaskSHA256    string          `json:"task_sha256"`
	Binding       Binding         `json:"binding"`
	Group         string          `json:"group"`
	Label         string          `json:"label"`
	CreatedAt     string          `json:"created_at"`
	Deadline      string          `json:"deadline"`
}

// SubmissionRecord is the create-once intent for one supervisor admission.
type SubmissionRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RequestSHA256 string `json:"request_sha256"`
	CreatedAt     string `json:"created_at"`
}

// StartRecord is the create-once intent for one inspection worker start.
type StartRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RequestSHA256 string `json:"request_sha256"`
	CreatedAt     string `json:"created_at"`
}

func validText(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsRune(value, '\x00')
}

func validateAbsoluteCleanPath(name, value string) error {
	if !validText(value) || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("%s must be a clean absolute path", name)
	}
	return nil
}

// ValidateBinding checks all static paths and hashes required for inspection
// admission. The shared supervisor validator permits historical optional
// fields, so this boundary explicitly requires every fresh binding field.
func ValidateBinding(binding Binding) error {
	if !validText(binding.DefinitionRevision) {
		return errors.New("missing inspection definition revision")
	}
	if err := task.ValidateSHA256(binding.DefinitionSHA256); err != nil {
		return fmt.Errorf("inspection definition digest: %w", err)
	}
	for name, value := range map[string]string{
		"inspection helper executable": binding.HelperExecutable,
		"inspection worker executable": binding.WorkerExecutable,
	} {
		if err := validateAbsoluteCleanPath(name, value); err != nil {
			return err
		}
	}
	for name, value := range map[string]string{
		"inspection helper digest": binding.HelperSHA256,
		"inspection worker digest": binding.WorkerSHA256,
	} {
		if err := task.ValidateSHA256(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := task.ValidateFreshSupervisorRef(binding.Supervisor); err != nil {
		return fmt.Errorf("inspection supervisor binding: %w", err)
	}
	return nil
}

func canonicalTimestamp(name, value string) (time.Time, error) {
	if !validText(value) {
		return time.Time{}, fmt.Errorf("missing %s", name)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid %s: %w", name, err)
	}
	if parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, fmt.Errorf("%s is not canonical UTC RFC3339Nano", name)
	}
	return parsed, nil
}

func validateRequestIdentity(record RequestRecord, expectedRoot string) error {
	if record.SchemaVersion != task.SchemaVersion {
		return fmt.Errorf("%w: inspection request schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	if err := task.ValidateRootID(record.RootID); err != nil {
		return err
	}
	if err := task.ValidateTaskID(record.TaskID); err != nil {
		return err
	}
	if expectedRoot != "" && record.RootID != expectedRoot {
		return task.ErrIdentityMismatch
	}
	if record.Task.RootID != record.RootID || record.Task.TaskID != record.TaskID {
		return task.ErrIdentityMismatch
	}
	if err := task.ValidateTaskRecord(&record.Task); err != nil {
		return err
	}
	taskBytes, err := task.MarshalCanonical(record.Task)
	if err != nil {
		return err
	}
	if taskDigestErr := task.ValidateSHA256(record.TaskSHA256); taskDigestErr != nil {
		return taskDigestErr
	}
	if task.ComputeSHA256(taskBytes) != record.TaskSHA256 {
		return task.ErrEvidenceFault
	}
	if bindingErr := ValidateBinding(record.Binding); bindingErr != nil {
		return bindingErr
	}
	if record.Group != groupForRoot(record.RootID) || record.Label != labelForIdentity(record.RootID, record.TaskID) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateRequestTiming(record RequestRecord) (time.Time, error) {
	created, err := canonicalTimestamp("inspection request created_at", record.CreatedAt)
	if err != nil {
		return time.Time{}, err
	}
	deadline, err := canonicalTimestamp("inspection request deadline", record.Deadline)
	if err != nil {
		return time.Time{}, err
	}
	if deadline.Sub(created) != AdmissionTimeout {
		return time.Time{}, errors.New("inspection request deadline must equal created_at plus admission timeout")
	}
	return deadline, nil
}

func validateRequestRecord(record RequestRecord, expectedRoot string) (time.Time, error) {
	if err := validateRequestIdentity(record, expectedRoot); err != nil {
		return time.Time{}, err
	}
	return validateRequestTiming(record)
}

// ValidateRequestRecord validates an immutable request and its persisted
// deadline against the expected store root identity.
func ValidateRequestRecord(record RequestRecord, expectedRoot string) error {
	_, err := validateRequestRecord(record, expectedRoot)
	return err
}

func validateClaimRecord(schema int, digest, createdAt string) error {
	if schema != task.SchemaVersion {
		return fmt.Errorf("%w: inspection claim schema %d", task.ErrUnsupportedSchema, schema)
	}
	if err := task.ValidateSHA256(digest); err != nil {
		return err
	}
	_, err := canonicalTimestamp("inspection claim created_at", createdAt)
	return err
}

func groupForRoot(rootID string) string { return groupPrefix + rootID }

func labelForIdentity(rootID, taskID string) string { return groupForRoot(rootID) + "-" + taskID }

func cloneTaskRecord(record task.TaskRecord) task.TaskRecord {
	copy := record
	if record.PriorSession != nil {
		prior := *record.PriorSession
		copy.PriorSession = &prior
	}
	return copy
}

func cloneRequestRecord(record RequestRecord) RequestRecord {
	record.Task = cloneTaskRecord(record.Task)
	return record
}
