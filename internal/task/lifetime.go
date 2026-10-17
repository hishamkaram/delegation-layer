package task

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"
)

const (
	LifetimeScopeCgroupV2     = "cgroup-v2"
	LifetimeScopeProcessGroup = "process-group"

	LifetimeAssuranceExact       = "exact"
	LifetimeAssuranceCooperative = "cooperative"

	LifetimeOwnerTask       = "task"
	LifetimeOwnerInspection = "inspection"
	LifetimeOwnerWorkspace  = "workspace"

	LifetimeTerminationCompleted    = "completed"
	LifetimeTerminationBudget       = "budget"
	LifetimeTerminationRequested    = "requested"
	LifetimeTerminationRecovery     = "recovery"
	LifetimeTerminationPreviousBoot = "previous-boot"

	LifetimeWorkerWaitSuccess    LifetimeWorkerWaitResult = "success"
	LifetimeWorkerWaitExitError  LifetimeWorkerWaitResult = "exit-error"
	LifetimeWorkerWaitError      LifetimeWorkerWaitResult = "wait-error"
	LifetimeWorkerWaitNotStarted LifetimeWorkerWaitResult = "not-started"

	// MaxLifetimeLaunches bounds the create-once launch namespace shared by
	// guardians and both durable record stores.
	MaxLifetimeLaunches uint64 = 64
)

// RecordAbsent reports only a direct, non-contextual record absence.
// Store readers must add ErrIdentityMismatch or ErrEvidenceFault after a
// primary record has been read and a required dependency is missing.
func RecordAbsent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) &&
		!errors.Is(err, ErrIdentityMismatch) &&
		!errors.Is(err, ErrEvidenceFault) &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded)
}

// LifetimeWorkerWaitResult is the closed classification of one managed-worker
// completion disposition: a native child Wait, no child started, or a typed
// worker-absence witness.
type LifetimeWorkerWaitResult string

// LifetimeIntentRecord reserves the immutable scope plan before native scope
// creation. A registration may be created only after this record exists.
type LifetimeIntentRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RootID        string `json:"root_id"`
	TaskID        string `json:"task_id"`
	OwnerKind     string `json:"owner_kind"`
	OwnerSHA256   string `json:"owner_sha256"`
	ScopeKind     string `json:"scope_kind"`
	Assurance     string `json:"assurance"`
	ScopePlan     string `json:"scope_plan"`
	BootID        string `json:"boot_id"`
	Deadline      string `json:"deadline"`
	CreatedAt     string `json:"created_at"`
}

// LifetimeRegistrationRecord binds one task to the platform scope that owns
// its native command descendants. The scope is registered before any native
// command may start.
type LifetimeRegistrationRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RootID        string `json:"root_id"`
	TaskID        string `json:"task_id"`
	OwnerKind     string `json:"owner_kind"`
	OwnerSHA256   string `json:"owner_sha256"`
	ScopeKind     string `json:"scope_kind"`
	Assurance     string `json:"assurance"`
	ScopeID       string `json:"scope_id"`
	BootID        string `json:"boot_id"`
	Deadline      string `json:"deadline"`
	CreatedAt     string `json:"created_at"`
}

// LifetimeWorkerRecord binds the managed worker identity to the canonical
// registration that authorized it.
type LifetimeWorkerRecord struct {
	SchemaVersion      int    `json:"schema_version"`
	RootID             string `json:"root_id"`
	TaskID             string `json:"task_id"`
	RegistrationSHA256 string `json:"registration_sha256"`
	PID                int    `json:"pid"`
	ProcessStart       string `json:"process_start"`
	BootID             string `json:"boot_id"`
	StartedAt          string `json:"started_at"`
}

// LifetimeWorkerWaitRecord binds one create-once managed-worker completion
// disposition to its registration or prepared intent. WorkerSHA256 is
// optional only when no child started, an exact scope has an unqualified Wait
// observation, or a previous-boot absence witness has no worker record.
type LifetimeWorkerWaitRecord struct {
	SchemaVersion      int                      `json:"schema_version"`
	RootID             string                   `json:"root_id"`
	TaskID             string                   `json:"task_id"`
	RegistrationSHA256 string                   `json:"registration_sha256"`
	WorkerSHA256       string                   `json:"worker_sha256,omitempty"`
	ChildPID           int                      `json:"child_pid"`
	Result             LifetimeWorkerWaitResult `json:"result"`
	ExitCode           *int                     `json:"exit_code,omitempty"`
	ObservedAt         string                   `json:"observed_at"`
	IntentSHA256       string                   `json:"intent_sha256,omitempty"`
	AbsenceProof       LifetimeAbsenceProof     `json:"absence_proof,omitempty"`
	ObservedBootID     string                   `json:"observed_boot_id,omitempty"`
}

// LifetimeLaunchRecord binds one authorized native start to its registered
// scope. Sequence is monotonic within the task and is never reused.
type LifetimeLaunchRecord struct {
	SchemaVersion      int    `json:"schema_version"`
	RootID             string `json:"root_id"`
	TaskID             string `json:"task_id"`
	RegistrationSHA256 string `json:"registration_sha256"`
	Sequence           uint64 `json:"sequence"`
	PID                int    `json:"pid"`
	ProcessStart       string `json:"process_start"`
	PGID               int    `json:"pgid,omitempty"`
	StartedAt          string `json:"started_at"`
}

// LifetimeTerminationRecord is the only process-scope termination authority.
// Queue state and a worker exit never substitute for this record.
type LifetimeTerminationRecord struct {
	SchemaVersion      int    `json:"schema_version"`
	RootID             string `json:"root_id"`
	TaskID             string `json:"task_id"`
	RegistrationSHA256 string `json:"registration_sha256"`
	LaunchCount        uint64 `json:"launch_count"`
	LaunchSetSHA256    string `json:"launch_set_sha256"`
	Cause              string `json:"cause"`
	ScopeEmpty         bool   `json:"scope_empty"`
	ObservedAt         string `json:"observed_at"`
}

func ValidateLifetimeRegistrationRecord(r *LifetimeRegistrationRecord) error {
	if r == nil {
		return errors.New("nil lifetime registration")
	}
	if err := validateLifetimeIdentity(r.SchemaVersion, r.RootID, r.TaskID); err != nil {
		return err
	}
	if err := validateLifetimeOwner(r.OwnerKind, r.OwnerSHA256); err != nil {
		return err
	}
	if err := validateLifetimeScope(r.ScopeKind, r.Assurance); err != nil {
		return err
	}
	if !validLifetimeToken(r.ScopeID) || !validLifetimeToken(r.BootID) {
		return ErrIdentityMismatch
	}
	if err := validateTimestamp(r.Deadline); err != nil {
		return err
	}
	return validateTimestamp(r.CreatedAt)
}

func ValidateLifetimeIntentRecord(r *LifetimeIntentRecord) error {
	if r == nil {
		return errors.New("nil lifetime intent")
	}
	if err := validateLifetimeIdentity(r.SchemaVersion, r.RootID, r.TaskID); err != nil {
		return err
	}
	if err := validateLifetimeOwner(r.OwnerKind, r.OwnerSHA256); err != nil {
		return err
	}
	if err := validateLifetimeScope(r.ScopeKind, r.Assurance); err != nil {
		return err
	}
	if !validLifetimeToken(r.ScopePlan) || !validLifetimeToken(r.BootID) {
		return ErrIdentityMismatch
	}
	if err := validateTimestamp(r.Deadline); err != nil {
		return err
	}
	return validateTimestamp(r.CreatedAt)
}

func ValidateLifetimeLaunchRecord(r *LifetimeLaunchRecord) error {
	if r == nil {
		return errors.New("nil lifetime launch")
	}
	if err := validateLifetimeIdentity(r.SchemaVersion, r.RootID, r.TaskID); err != nil {
		return err
	}
	if err := ValidateSHA256(r.RegistrationSHA256); err != nil {
		return err
	}
	if r.Sequence == 0 || r.PID <= 0 || r.PGID < 0 || !validLifetimeToken(r.ProcessStart) {
		return ErrIdentityMismatch
	}
	return validateTimestamp(r.StartedAt)
}

func ValidateLifetimeWorkerRecord(r *LifetimeWorkerRecord) error {
	if r == nil {
		return errors.New("nil lifetime worker")
	}
	if err := validateLifetimeIdentity(r.SchemaVersion, r.RootID, r.TaskID); err != nil {
		return err
	}
	if err := ValidateSHA256(r.RegistrationSHA256); err != nil {
		return err
	}
	if r.PID <= 0 || !validLifetimeToken(r.ProcessStart) || !validLifetimeToken(r.BootID) {
		return ErrIdentityMismatch
	}
	return validateLifetimeWorkerTimestamp(r.StartedAt)
}

func ValidateLifetimeWorkerWaitRecord(r *LifetimeWorkerWaitRecord) error {
	if r == nil {
		return errors.New("nil lifetime worker wait")
	}
	if err := validateLifetimeIdentity(r.SchemaVersion, r.RootID, r.TaskID); err != nil {
		return err
	}
	if r.Result != LifetimeWorkerWaitAbsent {
		if err := ValidateSHA256(r.RegistrationSHA256); err != nil {
			return err
		}
	}
	if r.WorkerSHA256 != "" {
		if err := ValidateSHA256(r.WorkerSHA256); err != nil {
			return err
		}
	}
	if err := validateLifetimeWorkerWaitAbsenceFields(r); err != nil {
		return err
	}
	if err := validateLifetimeWorkerWaitResult(r); err != nil {
		return err
	}
	return validateLifetimeWorkerTimestamp(r.ObservedAt)
}

func validateLifetimeWorkerWaitResult(r *LifetimeWorkerWaitRecord) error {
	switch r.Result {
	case LifetimeWorkerWaitAbsent:
		return validateLifetimeWorkerWaitAbsenceResult(r)
	case LifetimeWorkerWaitNotStarted:
		if !LifetimeWorkerWaitProvesNotStarted(r) {
			return ErrIdentityMismatch
		}
	case LifetimeWorkerWaitSuccess:
		if err := validateStartedLifetimeWorkerWait(r); err != nil {
			return err
		}
		if r.ExitCode == nil || *r.ExitCode != 0 {
			return ErrIdentityMismatch
		}
	case LifetimeWorkerWaitExitError:
		if err := validateStartedLifetimeWorkerWait(r); err != nil {
			return err
		}
		if r.ExitCode == nil || *r.ExitCode == 0 {
			return ErrIdentityMismatch
		}
	case LifetimeWorkerWaitError:
		if err := validateStartedLifetimeWorkerWait(r); err != nil {
			return err
		}
		// A wait error may happen before an exit status is available. When
		// one is available, preserve it without interpreting the wait error.
	default:
		return fmt.Errorf("invalid lifetime worker wait result: %w", ErrInvalidEnum)
	}
	return nil
}

func validateStartedLifetimeWorkerWait(r *LifetimeWorkerWaitRecord) error {
	if r.ChildPID <= 0 {
		return ErrIdentityMismatch
	}
	return nil
}

// LifetimeWorkerWaitMatchesRegistration reports whether a wait record binds
// to the supplied canonical registration winner.
func LifetimeWorkerWaitMatchesRegistration(wait *LifetimeWorkerWaitRecord, registration *LifetimeRegistrationRecord, registrationSHA256 string) bool {
	return wait != nil && registration != nil &&
		wait.RootID == registration.RootID &&
		wait.TaskID == registration.TaskID &&
		wait.RegistrationSHA256 == registrationSHA256
}

// LifetimeWorkerWaitMatchesWorker reports whether a qualified wait record
// binds to the supplied canonical worker winner and its process identity.
func LifetimeWorkerWaitMatchesWorker(wait *LifetimeWorkerWaitRecord, worker *LifetimeWorkerRecord, workerSHA256 string) bool {
	return wait != nil && worker != nil && wait.WorkerSHA256 != "" &&
		wait.WorkerSHA256 == workerSHA256 &&
		wait.RootID == worker.RootID &&
		wait.TaskID == worker.TaskID &&
		wait.ChildPID == worker.PID
}

// LifetimeWorkerWaitIsQualified reports whether a wait record carries the
// canonical worker identity needed for qualified proof.
func LifetimeWorkerWaitIsQualified(wait *LifetimeWorkerWaitRecord) bool {
	return wait != nil && wait.Result != LifetimeWorkerWaitAbsent && wait.WorkerSHA256 != ""
}

// LifetimeWorkerWaitProvesNotStarted reports whether the durable disposition
// proves that the registered scope never launched a managed-worker child.
func LifetimeWorkerWaitProvesNotStarted(wait *LifetimeWorkerWaitRecord) bool {
	return wait != nil && wait.Result == LifetimeWorkerWaitNotStarted &&
		wait.WorkerSHA256 == "" && wait.ChildPID == 0 && wait.ExitCode == nil
}

// LifetimeIntentMatchesRegistration reports whether a registration carries
// the immutable facts reserved by its intent. The concrete scope identifier is
// intentionally absent: it is assigned only when the native scope is created.
func LifetimeIntentMatchesRegistration(intent *LifetimeIntentRecord, registration *LifetimeRegistrationRecord) bool {
	return intent != nil && registration != nil &&
		intent.RootID == registration.RootID &&
		intent.TaskID == registration.TaskID &&
		intent.OwnerKind == registration.OwnerKind &&
		intent.OwnerSHA256 == registration.OwnerSHA256 &&
		intent.ScopeKind == registration.ScopeKind &&
		intent.Assurance == registration.Assurance &&
		intent.BootID == registration.BootID &&
		intent.Deadline == registration.Deadline
}

func ValidateLifetimeTerminationRecord(r *LifetimeTerminationRecord) error {
	if r == nil {
		return errors.New("nil lifetime termination")
	}
	if err := validateLifetimeIdentity(r.SchemaVersion, r.RootID, r.TaskID); err != nil {
		return err
	}
	if err := ValidateSHA256(r.RegistrationSHA256); err != nil {
		return err
	}
	if err := ValidateSHA256(r.LaunchSetSHA256); err != nil {
		return err
	}
	switch r.Cause {
	case LifetimeTerminationCompleted, LifetimeTerminationBudget, LifetimeTerminationRequested, LifetimeTerminationRecovery, LifetimeTerminationPreviousBoot:
	default:
		return fmt.Errorf("invalid lifetime termination cause: %w", ErrInvalidEnum)
	}
	if !r.ScopeEmpty {
		return ErrIdentityMismatch
	}
	return validateTimestamp(r.ObservedAt)
}

func validLifetimeToken(value string) bool {
	return nonblank(value) && len(value) <= MaxControlRecordSize
}

func validateLifetimeOwner(ownerKind, ownerSHA256 string) error {
	switch ownerKind {
	case LifetimeOwnerTask, LifetimeOwnerInspection, LifetimeOwnerWorkspace:
	default:
		return fmt.Errorf("invalid lifetime owner kind: %w", ErrInvalidEnum)
	}
	return ValidateSHA256(ownerSHA256)
}

func validateLifetimeScope(scopeKind, assurance string) error {
	switch scopeKind {
	case LifetimeScopeCgroupV2:
		if assurance != LifetimeAssuranceExact {
			return ErrIdentityMismatch
		}
	case LifetimeScopeProcessGroup:
		if assurance != LifetimeAssuranceCooperative {
			return ErrIdentityMismatch
		}
	default:
		return fmt.Errorf("invalid lifetime scope kind: %w", ErrInvalidEnum)
	}
	return nil
}

func validateLifetimeWorkerTimestamp(value string) error {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return fmt.Errorf("invalid required RFC3339 timestamp: %w", err)
	}
	if parsed.UTC().Format(time.RFC3339Nano) != value {
		return fmt.Errorf("lifetime worker timestamp is not canonical UTC RFC3339Nano")
	}
	return nil
}

func validateLifetimeIdentity(schema int, rootID, taskID string) error {
	if schema != SchemaVersion {
		return fmt.Errorf("%w: lifetime schema %d", ErrUnsupportedSchema, schema)
	}
	if err := ValidateRootID(rootID); err != nil {
		return err
	}
	return ValidateTaskID(taskID)
}

// LifetimeTaskOwnerSHA256 binds lifetime evidence to one immutable prepared
// task without copying both record digests into every lifetime record.
func LifetimeTaskOwnerSHA256(specSHA256, metaSHA256 string) (string, error) {
	if err := ValidateSHA256(specSHA256); err != nil {
		return "", err
	}
	if err := ValidateSHA256(metaSHA256); err != nil {
		return "", err
	}
	return ComputeSHA256([]byte("delegation-layer:lifetime-owner:task:v1\n" + specSHA256 + "\n" + metaSHA256)), nil
}
