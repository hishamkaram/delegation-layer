package task

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// SchemaVersion defines the supported protocol JSON schema version.
const SchemaVersion = 1

// Maximum size for any control record JSON file (1 MiB).
const MaxControlRecordSize = 1024 * 1024

// Maximum size for input brief (8 MiB).
const MaxBriefSize = 8 * 1024 * 1024

// Sentinel errors.
var (
	ErrStopAlreadyRequested  = errors.New("stop request already exists: no new authority")
	ErrTerminalTask          = errors.New("task already has a valid terminal outcome")
	ErrInvalidTaskID         = errors.New("invalid task ID: must be exactly 32 lowercase hex characters")
	ErrInvalidRootID         = errors.New("invalid root ID: must be exactly 32 lowercase hex characters")
	ErrUnsupportedSchema     = errors.New("unsupported schema version")
	ErrDuplicateKey          = errors.New("duplicate key in JSON object")
	ErrTrailingJSON          = errors.New("unexpected trailing bytes after JSON payload")
	ErrControlRecordTooBig   = errors.New("control record exceeds maximum permitted size of 1 MiB")
	ErrIdentityMismatch      = errors.New("record identity does not match expected task or root")
	ErrInvalidEnum           = errors.New("invalid enum value")
	ErrRequestConflict       = errors.New("immutable task request conflict: request parameters do not match existing task")
	ErrOutcomeConflict       = errors.New("publication outcome conflict: candidate outcome contradicts existing valid outcome")
	ErrInvariantFault        = errors.New("invariant fault: store state violates delegation invariants")
	ErrUncertainDurability   = errors.New("uncertain durability: post-link directory barrier failed; destination preserved without authority")
	ErrLockBusy              = errors.New("lock acquisition busy")
	ErrSessionBusy           = errors.New("session continuation busy: active claim belongs to another task")
	ErrAlreadySubmitted      = errors.New("task already submitted")
	ErrAlreadyStarted        = errors.New("task already started")
	ErrPermitAlreadyUsed     = errors.New("authority permit has already been consumed")
	ErrInvalidPermit         = errors.New("invalid or nil authority permit")
	ErrNoSeal                = errors.New("evidence is not sealed: provider.exit absent")
	ErrIncompatiblePredicate = errors.New("incompatible predicate: exact predicate reference not available")
	ErrEvidenceFault         = errors.New("evidence fault: raw files do not match sealed manifest")
)

var hexPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidateTaskID verifies that id is exactly 32 lowercase hexadecimal characters.
func ValidateTaskID(id string) error {
	if !hexPattern.MatchString(id) {
		return ErrInvalidTaskID
	}
	return nil
}

// ValidateRootID verifies that id is exactly 32 lowercase hexadecimal characters.
func ValidateRootID(id string) error {
	if !hexPattern.MatchString(id) {
		return ErrInvalidRootID
	}
	return nil
}

// NewRandomID generates a 32-character lowercase hex string from 16 cryptographically random bytes.
func NewRandomID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating random ID: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// NewTaskID generates a valid random 32-character lowercase hex task ID.
func NewTaskID() (string, error) {
	return NewRandomID()
}

// NewRootID generates a valid random 32-character lowercase hex root ID.
func NewRootID() (string, error) {
	return NewRandomID()
}

// Liveness represents task execution liveness: Undetermined=0, Running=1, Ended=2.
type Liveness uint8

const (
	LivenessUndetermined Liveness = 0
	LivenessRunning      Liveness = 1
	LivenessEnded        Liveness = 2
)

func (l Liveness) String() string {
	switch l {
	case LivenessUndetermined:
		return "undetermined"
	case LivenessRunning:
		return "running"
	case LivenessEnded:
		return "ended"
	default:
		return "unknown"
	}
}

// Admission represents supervisor admission status: Unknown=0, NotAdmitted=1, Admitted=2.
type Admission uint8

const (
	AdmissionUnknown     Admission = 0
	AdmissionNotAdmitted Admission = 1
	AdmissionAdmitted    Admission = 2
)

func (a Admission) String() string {
	switch a {
	case AdmissionUnknown:
		return "unknown"
	case AdmissionNotAdmitted:
		return "not-admitted"
	case AdmissionAdmitted:
		return "admitted"
	default:
		return "unknown"
	}
}

// Publication represents outcome publication status: Unknown=0, Pending=1, Committed=2, Rejected=3.
type Publication uint8

const (
	PublicationUnknown   Publication = 0
	PublicationPending   Publication = 1
	PublicationCommitted Publication = 2
	PublicationRejected  Publication = 3
)

func (p Publication) String() string {
	switch p {
	case PublicationUnknown:
		return "unknown"
	case PublicationPending:
		return "pending"
	case PublicationCommitted:
		return "committed"
	case PublicationRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// Verdict constants.
const (
	VerdictCommitted = "committed"
	VerdictRejected  = "rejected"
)

// InvocationState constants.
const (
	InvocationStarted     = "started"
	InvocationStartFailed = "start_failed"
)

// PredicateRef identifies a pure publication predicate contract.
type PredicateRef struct {
	Adapter string `json:"adapter"`
	Mode    string `json:"mode"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// Equal returns true if two PredicateRefs are identical in all fields.
func (p PredicateRef) Equal(other PredicateRef) bool {
	return p.Adapter == other.Adapter &&
		p.Mode == other.Mode &&
		p.Version == other.Version &&
		p.SHA256 == other.SHA256
}

// TaskConfig captures requested configuration parameters.
type TaskConfig struct {
	Model         string `json:"model,omitempty"`
	Effort        string `json:"effort,omitempty"`
	Permission    string `json:"permission,omitempty"`
	Budget        string `json:"budget,omitempty"`
	NativeTimeout string `json:"native_timeout,omitempty"`
}

// EffectiveConfig captures resolved non-secret effective configuration.
type EffectiveConfig struct {
	Containment string         `json:"containment"`
	Approval    string         `json:"approval"`
	Digest      string         `json:"digest"`
	Policy      *PolicyDetails `json:"policy,omitempty"`
}

// PriorSession captures conversation continuity reference.
type PriorSession struct {
	Provider          string `json:"provider"`
	ConversationID    string `json:"conversation_id"`
	PredecessorTaskID string `json:"predecessor_task_id"`
}

// SupervisorRef binds task execution to an explicit supervisor instance.
type SupervisorRef struct {
	ClientExecutable     string `json:"client_executable,omitempty"`
	ClientSHA256         string `json:"client_sha256,omitempty"`
	DaemonExecutable     string `json:"daemon_executable,omitempty"`
	DaemonSHA256         string `json:"daemon_sha256,omitempty"`
	ResolutionOS         string `json:"resolution_os,omitempty"`
	ResolutionHome       string `json:"resolution_home,omitempty"`
	ResolutionDataLocal  string `json:"resolution_data_local,omitempty"`
	ResolutionConfig     string `json:"resolution_config,omitempty"`
	ResolutionRuntime    string `json:"resolution_runtime,omitempty"`
	ResolutionUsername   string `json:"resolution_username,omitempty"`
	ResolutionCwd        string `json:"resolution_cwd,omitempty"`
	ResolvedConfigSHA256 string `json:"resolved_config_sha256,omitempty"`
	Endpoint             string `json:"endpoint"`
	ConfigPath           string `json:"config_path"`
	ConfigDigest         string `json:"config_digest"`
	ObservedVersion      string `json:"observed_version"`
}

// RootRecord is stored in root.json.
type RootRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RootID        string `json:"root_id"`
	CreatedAt     string `json:"created_at"`
}

// TaskRecord is stored in task.json (the immutable task request).
type TaskRecord struct {
	SchemaVersion   int           `json:"schema_version"`
	RootID          string        `json:"root_id"`
	TaskID          string        `json:"task_id"`
	Provider        string        `json:"provider"`
	Mode            string        `json:"mode"`
	CanonicalCwd    string        `json:"canonical_cwd"`
	RequestedConfig TaskConfig    `json:"requested_config"`
	BudgetNanos     int64         `json:"budget_nanos"`
	PriorSession    *PriorSession `json:"prior_session,omitempty"`
	BriefSHA256     string        `json:"brief_sha256"`
	BriefLength     int64         `json:"brief_length"`
}

// MetaRecord is stored in meta.json (the immutable prepared execution plan).
type MetaRecord struct {
	SchemaVersion      int             `json:"schema_version"`
	RootID             string          `json:"root_id"`
	TaskID             string          `json:"task_id"`
	SpecSHA256         string          `json:"spec_sha256"`
	RequestedConfig    TaskConfig      `json:"requested_config"`
	EffectiveConfig    EffectiveConfig `json:"effective_config"`
	Containment        string          `json:"containment"`
	Approval           string          `json:"approval"`
	ProviderExecutable string          `json:"provider_executable"`
	ProviderVersion    string          `json:"provider_version"`
	PublisherBuild     string          `json:"publisher_build"`
	PublisherVersion   string          `json:"publisher_version"`
	// Environment is the adapter's bounded, nonsecret launch environment.
	Environment          []string         `json:"environment,omitempty"`
	Predicate            PredicateRef     `json:"predicate"`
	SupervisorConfig     SupervisorRef    `json:"supervisor_config"`
	CreatedAt            string           `json:"created_at"`
	OutputArtifacts      []OutputArtifact `json:"output_artifacts,omitempty"`
	OutputWriterContract string           `json:"output_writer_contract,omitempty"`
	InputFiles           []InputFile      `json:"input_files,omitempty"`
}

// SubmitRecord is stored in submit.json.
type SubmitRecord struct {
	SchemaVersion int           `json:"schema_version"`
	RootID        string        `json:"root_id"`
	TaskID        string        `json:"task_id"`
	SpecSHA256    string        `json:"spec_sha256"`
	MetaSHA256    string        `json:"meta_sha256"`
	Label         string        `json:"label"`
	Supervisor    SupervisorRef `json:"supervisor"`
	CreatedAt     string        `json:"created_at"`
}

// SupervisorReceipt is stored in supervisor.ref.json.
type SupervisorReceipt struct {
	MetaSHA256           string `json:"meta_sha256,omitempty"`
	ConfigPath           string `json:"config_path,omitempty"`
	ClientExecutable     string `json:"client_executable,omitempty"`
	ClientSHA256         string `json:"client_sha256,omitempty"`
	DaemonExecutable     string `json:"daemon_executable,omitempty"`
	DaemonSHA256         string `json:"daemon_sha256,omitempty"`
	ResolutionOS         string `json:"resolution_os,omitempty"`
	ResolutionHome       string `json:"resolution_home,omitempty"`
	ResolutionDataLocal  string `json:"resolution_data_local,omitempty"`
	ResolutionConfig     string `json:"resolution_config,omitempty"`
	ResolutionRuntime    string `json:"resolution_runtime,omitempty"`
	ResolutionUsername   string `json:"resolution_username,omitempty"`
	ResolutionCwd        string `json:"resolution_cwd,omitempty"`
	ResolvedConfigSHA256 string `json:"resolved_config_sha256,omitempty"`
	SchemaVersion        int    `json:"schema_version"`
	RootID               string `json:"root_id"`
	TaskID               string `json:"task_id"`
	SpecSHA256           string `json:"spec_sha256"`
	NumericTaskID        int64  `json:"numeric_task_id"`
	Label                string `json:"label"`
	ConfigDigest         string `json:"config_digest"`
	Endpoint             string `json:"endpoint"`
	ObservedVersion      string `json:"observed_version"`
}

// ProviderStartRecord is stored in provider.start.
type ProviderStartRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RootID        string `json:"root_id"`
	TaskID        string `json:"task_id"`
	SpecSHA256    string `json:"spec_sha256"`
	MetaSHA256    string `json:"meta_sha256"`
	BudgetNanos   int64  `json:"budget_nanos"`
	CreatedAt     string `json:"created_at"`
}

// ProviderStartedRecord is stored in provider.started.json.
type ProviderStartedRecord struct {
	SchemaVersion   int    `json:"schema_version"`
	RootID          string `json:"root_id"`
	TaskID          string `json:"task_id"`
	SpecSHA256      string `json:"spec_sha256"`
	MetaSHA256      string `json:"meta_sha256"`
	StartedAt       string `json:"started_at"`
	DiagnosticNanos int64  `json:"diagnostic_nanos"`
}

// ProviderRefRecord is stored in provider.ref.json.
type ProviderRefRecord struct {
	MetaSHA256     string `json:"meta_sha256,omitempty"`
	Provider       string `json:"provider,omitempty"`
	ObservedAt     string `json:"observed_at,omitempty"`
	SchemaVersion  int    `json:"schema_version"`
	RootID         string `json:"root_id"`
	TaskID         string `json:"task_id"`
	SpecSHA256     string `json:"spec_sha256"`
	ConversationID string `json:"conversation_id"`
}

// RawManifestEntry describes an immutable sealed raw output file.
type RawManifestEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ProviderExitRecord is stored in provider.exit (the seal).
type ProviderExitRecord struct {
	SchemaVersion   int                `json:"schema_version"`
	RootID          string             `json:"root_id"`
	TaskID          string             `json:"task_id"`
	SpecSHA256      string             `json:"spec_sha256"`
	MetaSHA256      string             `json:"meta_sha256"`
	InvocationState string             `json:"invocation_state"`
	ExitCode        int                `json:"exit_code"`
	Error           string             `json:"error"`
	Predicate       PredicateRef       `json:"predicate"`
	RawManifest     []RawManifestEntry `json:"raw_manifest"`
	ManifestSHA256  string             `json:"manifest_sha256"`
	ClosedAt        string             `json:"closed_at"`
}

// PayloadDescriptor describes an immutable payload file (result.txt or publish.reject).
type PayloadDescriptor struct {
	Basename string `json:"basename"`
	Length   int64  `json:"length"`
	SHA256   string `json:"sha256"`
}

// OutcomeRecord is stored in outcome.json (the sole publication authority).
type OutcomeRecord struct {
	SchemaVersion  int               `json:"schema_version"`
	RootID         string            `json:"root_id"`
	TaskID         string            `json:"task_id"`
	SpecSHA256     string            `json:"spec_sha256"`
	MetaSHA256     string            `json:"meta_sha256"`
	Verdict        string            `json:"verdict"`
	EvidenceSHA256 string            `json:"evidence_sha256"`
	Predicate      PredicateRef      `json:"predicate"`
	Payload        PayloadDescriptor `json:"payload"`
	Usage          []UsageMetadata   `json:"usage,omitempty"`
}

// PublishExitRecord is stored in publish.exit.
type PublishExitRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RootID        string `json:"root_id"`
	TaskID        string `json:"task_id"`
	SpecSHA256    string `json:"spec_sha256"`
	MetaSHA256    string `json:"meta_sha256"`
	PublishedAt   string `json:"published_at"`
	PublisherID   string `json:"publisher_id"`
}

// StopRequestRecord is stored in stop/<request_id>.request.json.
type StopRequestRecord struct {
	NumericTaskID *int64        `json:"numeric_task_id,omitempty"`
	MetaSHA256    string        `json:"meta_sha256,omitempty"`
	Label         string        `json:"label,omitempty"`
	BudgetNanos   int64         `json:"budget_nanos,omitempty"`
	Deadline      string        `json:"deadline,omitempty"`
	SchemaVersion int           `json:"schema_version"`
	RootID        string        `json:"root_id"`
	TaskID        string        `json:"task_id"`
	SpecSHA256    string        `json:"spec_sha256"`
	RequestID     string        `json:"request_id"`
	Cause         string        `json:"cause"`
	Supervisor    SupervisorRef `json:"supervisor"`
	RequestedAt   string        `json:"requested_at"`
}

// StopReplyRecord is stored in stop/<request_id>.reply.json.
type StopReplyRecord struct {
	Action        string         `json:"action,omitempty"`
	MetaSHA256    string         `json:"meta_sha256,omitempty"`
	Label         string         `json:"label,omitempty"`
	Supervisor    *SupervisorRef `json:"supervisor,omitempty"`
	RequestSHA256 string         `json:"request_sha256,omitempty"`
	NumericTaskID *int64         `json:"numeric_task_id,omitempty"`
	SchemaVersion int            `json:"schema_version"`
	RootID        string         `json:"root_id"`
	TaskID        string         `json:"task_id"`
	SpecSHA256    string         `json:"spec_sha256"`
	RequestID     string         `json:"request_id"`
	Acknowledged  bool           `json:"acknowledged"`
	Message       string         `json:"message"`
	RepliedAt     string         `json:"replied_at"`
}

// StopObservedRecord is stored in stop/<request_id>.observed.json.
type StopObservedRecord struct {
	State         string         `json:"state,omitempty"`
	MetaSHA256    string         `json:"meta_sha256,omitempty"`
	Label         string         `json:"label,omitempty"`
	Supervisor    *SupervisorRef `json:"supervisor,omitempty"`
	RequestSHA256 string         `json:"request_sha256,omitempty"`
	NumericTaskID *int64         `json:"numeric_task_id,omitempty"`
	SchemaVersion int            `json:"schema_version"`
	RootID        string         `json:"root_id"`
	TaskID        string         `json:"task_id"`
	SpecSHA256    string         `json:"spec_sha256"`
	RequestID     string         `json:"request_id"`
	Terminated    bool           `json:"terminated"`
	ObservedAt    string         `json:"observed_at"`
}

// FaultRecord is stored in faults/<random_id>.json.
type FaultRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RootID        string `json:"root_id"`
	TaskID        string `json:"task_id"`
	FaultID       string `json:"fault_id"`
	Message       string `json:"message"`
	RecordedAt    string `json:"recorded_at"`
}

// SessionClaimRecord is stored in sessions/<hash>/claim.json.
type SessionClaimRecord struct {
	SchemaVersion  int    `json:"schema_version"`
	Provider       string `json:"provider"`
	ConversationID string `json:"conversation_id"`
	RootID         string `json:"root_id"`
	TaskID         string `json:"task_id"`
	ClaimedAt      string `json:"claimed_at"`
}

// SessionReleaseRecord is stored in sessions/<hash>/release.json.
type SessionReleaseRecord struct {
	SchemaVersion             int    `json:"schema_version"`
	Provider                  string `json:"provider"`
	ConversationID            string `json:"conversation_id"`
	RootID                    string `json:"root_id"`
	TaskID                    string `json:"task_id"`
	PredecessorEvidenceSHA256 string `json:"predecessor_evidence_sha256"`
	ReleasedAt                string `json:"released_at"`
}

var hex64Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidateSHA256 verifies that s is exactly 64 lowercase hexadecimal characters.
func ValidateSHA256(s string) error {
	if !hex64Pattern.MatchString(s) {
		return errors.New("invalid SHA-256 digest: must be exactly 64 lowercase hex characters")
	}
	return nil
}

// ValidateRootRecord validates a root.json record.
func ValidateRootRecord(r *RootRecord) error {
	if r == nil {
		return errors.New("nil root record")
	}
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: root.json schema %d", ErrUnsupportedSchema, r.SchemaVersion)
	}
	if err := ValidateRootID(r.RootID); err != nil {
		return err
	}
	return validateTimestamp(r.CreatedAt)
}

// ValidateTaskRecord validates a task.json record.
func ValidateTaskRecord(r *TaskRecord) error {
	if r == nil {
		return errors.New("nil task record")
	}
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: task.json schema %d", ErrUnsupportedSchema, r.SchemaVersion)
	}
	if err := ValidateRootID(r.RootID); err != nil {
		return err
	}
	if err := ValidateTaskID(r.TaskID); err != nil {
		return err
	}
	if err := validateTaskConfiguration(r); err != nil {
		return err
	}
	if err := ValidateSHA256(r.BriefSHA256); err != nil {
		return fmt.Errorf("task record brief_sha256: %w", err)
	}
	if r.BriefLength <= 0 || r.BriefLength > MaxBriefSize {
		return errors.New("brief_length must be positive and at most 8 MiB")
	}
	return nil
}

// ValidateMetaRecord validates a meta.json record.
func ValidateMetaRecord(r *MetaRecord) error {
	if r == nil {
		return errors.New("nil meta record")
	}
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: meta.json schema %d", ErrUnsupportedSchema, r.SchemaVersion)
	}
	if err := ValidateRootID(r.RootID); err != nil {
		return err
	}
	if err := ValidateTaskID(r.TaskID); err != nil {
		return err
	}
	if err := ValidateSHA256(r.SpecSHA256); err != nil {
		return fmt.Errorf("meta record spec_sha256: %w", err)
	}
	if err := validateCanonicalInputFiles(r.InputFiles); err != nil {
		return fmt.Errorf("meta record input files: %w", err)
	}
	if err := validateCanonicalOutputArtifacts(r.OutputArtifacts); err != nil {
		return fmt.Errorf("meta record output artifacts: %w", err)
	}
	if err := ValidateOutputArtifacts(r.OutputArtifacts, r.OutputWriterContract); err != nil {
		return fmt.Errorf("meta record output contract: %w", err)
	}
	if err := ValidateInputOutputBindings(r.InputFiles, r.OutputArtifacts); err != nil {
		return fmt.Errorf("meta record artifact bindings: %w", err)
	}
	return validateMetaConfiguration(r)
}

// ValidateSubmitRecord validates a submit.json record.
func ValidateSubmitRecord(r *SubmitRecord) error {
	if r == nil {
		return errors.New("nil submit record")
	}
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: submit.json schema %d", ErrUnsupportedSchema, r.SchemaVersion)
	}
	if err := ValidateRootID(r.RootID); err != nil {
		return err
	}
	if err := ValidateTaskID(r.TaskID); err != nil {
		return err
	}
	if err := ValidateSHA256(r.SpecSHA256); err != nil {
		return fmt.Errorf("submit record spec_sha256: %w", err)
	}
	if err := ValidateSHA256(r.MetaSHA256); err != nil {
		return fmt.Errorf("submit record meta_sha256: %w", err)
	}
	if r.Label != "delegate:"+r.RootID+":"+r.TaskID {
		return errors.New("submit label must bind root and task identity")
	}
	if err := ValidateSupervisorRef(r.Supervisor); err != nil {
		return err
	}
	return validateTimestamp(r.CreatedAt)
}

// ValidateProviderStartRecord validates a provider.start record.
func ValidateProviderStartRecord(r *ProviderStartRecord) error {
	if r == nil {
		return errors.New("nil provider.start record")
	}
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: provider.start schema %d", ErrUnsupportedSchema, r.SchemaVersion)
	}
	if err := ValidateRootID(r.RootID); err != nil {
		return err
	}
	if err := ValidateTaskID(r.TaskID); err != nil {
		return err
	}
	if err := ValidateSHA256(r.SpecSHA256); err != nil {
		return fmt.Errorf("provider.start spec_sha256: %w", err)
	}
	if err := ValidateSHA256(r.MetaSHA256); err != nil {
		return fmt.Errorf("provider.start meta_sha256: %w", err)
	}
	if r.BudgetNanos <= 0 {
		return errors.New("budget_nanos must be positive in provider.start")
	}
	return validateTimestamp(r.CreatedAt)
}

// ValidateProviderStartedRecord validates a provider.started.json record.
func ValidateProviderStartedRecord(r *ProviderStartedRecord) error {
	if r == nil {
		return errors.New("nil provider.started.json record")
	}
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: provider.started.json schema %d", ErrUnsupportedSchema, r.SchemaVersion)
	}
	if err := ValidateRootID(r.RootID); err != nil {
		return err
	}
	if err := ValidateTaskID(r.TaskID); err != nil {
		return err
	}
	if err := ValidateSHA256(r.SpecSHA256); err != nil {
		return fmt.Errorf("provider.started.json spec_sha256: %w", err)
	}
	if err := ValidateSHA256(r.MetaSHA256); err != nil {
		return fmt.Errorf("provider.started.json meta_sha256: %w", err)
	}
	if r.DiagnosticNanos < 0 {
		return errors.New("negative start diagnostic duration")
	}
	return validateTimestamp(r.StartedAt)
}

// ValidateRawManifest validates raw manifest completeness, uniqueness, sorting, and digest.
func ValidateRawManifest(manifest []RawManifestEntry, manifestSHA256 string) error {
	if len(manifest) < 2 || len(manifest) > MaxOutputArtifacts+2 {
		return fmt.Errorf("%w: invalid raw manifest entry count", ErrEvidenceFault)
	}
	seenPaths := make(map[string]bool, len(manifest))
	for i, entry := range manifest {
		if err := validateRawEntry(entry); err != nil {
			return err
		}
		if i > 0 && manifest[i-1].Path >= entry.Path {
			return fmt.Errorf("%w: raw manifest paths are not strictly sorted and unique", ErrEvidenceFault)
		}
		seenPaths[entry.Path] = true
	}
	if !seenPaths["raw/stdout"] || !seenPaths["raw/stderr"] {
		return fmt.Errorf("%w: raw/stderr and raw/stdout are required", ErrEvidenceFault)
	}
	canonicalBytes, err := MarshalCanonical(manifest)
	if err != nil {
		return fmt.Errorf("%w: marshaling manifest: %w", ErrEvidenceFault, err)
	}
	if ComputeSHA256(canonicalBytes) != manifestSHA256 {
		return fmt.Errorf("%w: manifest digest mismatch", ErrEvidenceFault)
	}
	return nil
}

func validateRawEntry(entry RawManifestEntry) error {
	name, prefixed := strings.CutPrefix(entry.Path, "raw/")
	if !prefixed || entry.Size < 0 {
		return fmt.Errorf("%w: invalid raw entry path or size", ErrEvidenceFault)
	}
	if name != "stdout" && name != "stderr" {
		if err := ValidateArtifactName(name); err != nil {
			return fmt.Errorf("%w: unsafe raw artifact name: %w", ErrEvidenceFault, err)
		}
	}
	if err := ValidateSHA256(entry.SHA256); err != nil {
		return fmt.Errorf("%w: %w", ErrEvidenceFault, err)
	}
	return nil
}

// ValidateProviderExitRecord validates a provider.exit record.
func ValidateProviderExitRecord(r *ProviderExitRecord) error {
	if r == nil {
		return errors.New("nil provider.exit record")
	}
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: provider.exit schema version %d", ErrUnsupportedSchema, r.SchemaVersion)
	}
	if err := ValidateRootID(r.RootID); err != nil {
		return err
	}
	if err := ValidateTaskID(r.TaskID); err != nil {
		return err
	}
	if err := ValidateSHA256(r.SpecSHA256); err != nil {
		return fmt.Errorf("provider.exit spec_sha256: %w", err)
	}
	if err := ValidateSHA256(r.MetaSHA256); err != nil {
		return fmt.Errorf("provider.exit meta_sha256: %w", err)
	}
	if r.InvocationState != InvocationStarted && r.InvocationState != InvocationStartFailed {
		return fmt.Errorf("%w: invalid invocation state %q", ErrInvalidEnum, r.InvocationState)
	}
	if err := ValidateSHA256(r.ManifestSHA256); err != nil {
		return err
	}
	if err := ValidateRawManifest(r.RawManifest, r.ManifestSHA256); err != nil {
		return err
	}
	if err := ValidatePredicateRef(r.Predicate); err != nil {
		return err
	}
	return validateExitObservation(r)
}

func validateExitObservation(r *ProviderExitRecord) error {
	if r.ExitCode < -1 || r.ExitCode > 255 {
		return errors.New("exit code must be an observed process exit status")
	}
	if r.InvocationState == InvocationStartFailed && !nonblank(r.Error) {
		return errors.New("start failure requires a captured error")
	}
	return validateTimestamp(r.ClosedAt)
}

func validateOutcomePayload(r *OutcomeRecord) error {
	if err := ValidateSHA256(r.Payload.SHA256); err != nil {
		return fmt.Errorf("outcome.json payload sha256: %w", err)
	}
	if r.Payload.Length <= 0 {
		return fmt.Errorf("%w: terminal payload must be nonempty", ErrInvariantFault)
	}
	if r.Verdict == VerdictCommitted && r.Payload.Basename != "result.txt" {
		return fmt.Errorf("%w: committed outcome must have payload basename result.txt, got %q", ErrInvariantFault, r.Payload.Basename)
	}
	if r.Verdict == VerdictRejected && r.Payload.Basename != "publish.reject" {
		return fmt.Errorf("%w: rejected outcome must have payload basename publish.reject, got %q", ErrInvariantFault, r.Payload.Basename)
	}
	return nil
}

// ValidateOutcomeRecord validates an outcome.json record.
func ValidateOutcomeRecord(r *OutcomeRecord) error {
	if r == nil {
		return errors.New("nil outcome record")
	}
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: outcome.json schema version %d", ErrUnsupportedSchema, r.SchemaVersion)
	}
	if err := ValidateRootID(r.RootID); err != nil {
		return err
	}
	if err := ValidateTaskID(r.TaskID); err != nil {
		return err
	}
	if err := ValidateSHA256(r.SpecSHA256); err != nil {
		return fmt.Errorf("outcome.json spec_sha256: %w", err)
	}
	if err := ValidateSHA256(r.MetaSHA256); err != nil {
		return fmt.Errorf("outcome.json meta_sha256: %w", err)
	}
	if r.Verdict != VerdictCommitted && r.Verdict != VerdictRejected {
		return fmt.Errorf("%w: invalid verdict %q", ErrInvalidEnum, r.Verdict)
	}
	if err := ValidateSHA256(r.EvidenceSHA256); err != nil {
		return fmt.Errorf("outcome.json evidence_sha256: %w", err)
	}
	if err := ValidatePredicateRef(r.Predicate); err != nil {
		return err
	}
	if err := ValidateUsageMetadataList(r.Usage); err != nil {
		return err
	}
	return validateOutcomePayload(r)
}

// ValidateSessionClaimRecord validates a session claim record.
func ValidateSessionClaimRecord(r *SessionClaimRecord) error {
	if r == nil {
		return errors.New("nil session claim record")
	}
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: session claim schema %d", ErrUnsupportedSchema, r.SchemaVersion)
	}
	if err := ValidateRootID(r.RootID); err != nil {
		return err
	}
	if err := ValidateTaskID(r.TaskID); err != nil {
		return err
	}
	if !nonblank(r.Provider) || !nonblank(r.ConversationID) {
		return errors.New("missing provider or conversation_id in session claim")
	}
	return validateSessionFields(r.Provider, r.ClaimedAt)
}

// ValidateSessionReleaseRecord validates a session release record.
func ValidateSessionReleaseRecord(r *SessionReleaseRecord) error {
	if r == nil {
		return errors.New("nil session release record")
	}
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: session release schema %d", ErrUnsupportedSchema, r.SchemaVersion)
	}
	if err := ValidateRootID(r.RootID); err != nil {
		return err
	}
	if err := ValidateTaskID(r.TaskID); err != nil {
		return err
	}
	if !nonblank(r.Provider) || !nonblank(r.ConversationID) {
		return errors.New("missing provider or conversation_id in session release")
	}
	if err := ValidateSHA256(r.PredecessorEvidenceSHA256); err != nil {
		return fmt.Errorf("session release evidence digest: %w", err)
	}
	return validateSessionFields(r.Provider, r.ReleasedAt)
}
