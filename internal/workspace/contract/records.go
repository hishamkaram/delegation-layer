package contract

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	DefaultBudget       = 5 * time.Minute
	ArtifactLimit int64 = task.MaxWorkspaceArtifactSize

	OperationProvision = "provision"
	OperationCapture   = "capture"
	OperationVerify    = "verify"
	OperationRemove    = "remove"
	OperationForget    = "forget"

	SubjectTask      = "task"
	SubjectOperation = "operation"
	SubjectAdmission = "admission"
)

var (
	ErrBusy     = errors.New("workspace is in use")
	ErrReleased = errors.New("workspace has been released")
	ErrIdentity = errors.New("workspace identity changed")
	ErrPending  = errors.New("workspace operation is pending")
)

// Subject identifies whose edits a capture observes, without attributing Lead
// verification or release edits to an earlier provider turn.
type Subject struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

// ResourceRequest binds an admission to its one optional managed workspace.
type ResourceRequest struct {
	SchemaVersion int    `json:"schema_version"`
	RootID        string `json:"root_id"`
	WorkspaceID   string `json:"workspace_id"`
	TaskID        string `json:"task_id"`
	RequestSHA256 string `json:"request_sha256"`
	SourceCwd     string `json:"source_cwd"`
	BaseRef       string `json:"base_ref"`
	Container     string `json:"container"`
	BudgetNanos   int64  `json:"budget_nanos"`
}

// DirectoryIdentity pins one owned directory through portable file identity
// evidence. The application obtains and compares these fields at the edge.
type DirectoryIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// Registration records Git's validated administrative and checkout coordinates.
type Registration struct {
	SchemaVersion         int               `json:"schema_version"`
	WorkspaceID           string            `json:"workspace_id"`
	SourceRoot            string            `json:"source_root"`
	CommonDir             string            `json:"common_dir"`
	GitDir                string            `json:"git_dir"`
	AdminDir              string            `json:"admin_dir"`
	AdminBacklink         string            `json:"admin_backlink"`
	CheckoutPath          string            `json:"checkout_path"`
	CanonicalCwd          string            `json:"canonical_cwd"`
	RelativeCwd           string            `json:"relative_cwd"`
	BaseOID               string            `json:"base_oid"`
	ObjectFormat          string            `json:"object_format"`
	BaseRef               string            `json:"base_ref"`
	SourceChangesExcluded bool              `json:"source_changes_excluded"`
	ContainerIdentity     DirectoryIdentity `json:"container_identity"`
	CheckoutIdentity      DirectoryIdentity `json:"checkout_identity"`
}

// Reservation is an immutable, serial workspace usage claim. Closure is a
// separate record and never depends on native conversation resumability.
type Reservation struct {
	SchemaVersion int     `json:"schema_version"`
	WorkspaceID   string  `json:"workspace_id"`
	Sequence      uint64  `json:"sequence"`
	Subject       Subject `json:"subject"`
}

// ReservationClosed records canonical lifetime closure and capture disposition.
type ReservationClosed struct {
	SchemaVersion int    `json:"schema_version"`
	WorkspaceID   string `json:"workspace_id"`
	Sequence      uint64 `json:"sequence"`
	Disposition   string `json:"disposition"`
	CaptureID     string `json:"capture_id,omitempty"`
}

// OperationRequest authorizes one finite helper, independently of task budget.
type OperationRequest struct {
	SchemaVersion    int                `json:"schema_version"`
	RootID           string             `json:"root_id"`
	OperationID      string             `json:"operation_id"`
	WorkspaceID      string             `json:"workspace_id"`
	Kind             string             `json:"kind"`
	Subject          Subject            `json:"subject"`
	Deadline         string             `json:"deadline"`
	WorkerExecutable string             `json:"worker_executable"`
	WorkerSHA256     string             `json:"worker_sha256"`
	Supervisor       task.SupervisorRef `json:"supervisor"`
	Arguments        []string           `json:"arguments,omitempty"`
	Discard          bool               `json:"discard,omitempty"`
}

// Artifact describes complete optional bytes, or their explicit omission.
type Artifact struct {
	Name      string `json:"name"`
	Length    int64  `json:"length"`
	SHA256    string `json:"sha256,omitempty"`
	Omitted   string `json:"omitted,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// CaptureCandidate is durable before any immutable tree ref is published.
type CaptureCandidate struct {
	SchemaVersion int     `json:"schema_version"`
	WorkspaceID   string  `json:"workspace_id"`
	OperationID   string  `json:"operation_id"`
	Subject       Subject `json:"subject"`
	BaseOID       string  `json:"base_oid"`
	ObservedHEAD  string  `json:"observed_head"`
	TreeOID       string  `json:"tree_oid"`
	SnapshotRef   string  `json:"snapshot_ref"`
	ObjectFormat  string  `json:"object_format"`
}

// CaptureReceipt is core evidence, never provider completion authority.
type CaptureReceipt struct {
	LifetimeClosure   task.LifetimeClosureKind `json:"lifetime_closure,omitempty"`
	SchemaVersion     int                      `json:"schema_version"`
	WorkspaceID       string                   `json:"workspace_id"`
	OperationID       string                   `json:"operation_id"`
	Subject           Subject                  `json:"subject"`
	Status            string                   `json:"status"`
	ReasonCode        string                   `json:"reason_code,omitempty"`
	Candidate         *CaptureCandidate        `json:"candidate,omitempty"`
	Artifacts         []Artifact               `json:"artifacts,omitempty"`
	TerminationSHA256 string                   `json:"termination_sha256,omitempty"`
	WorkerWaitSHA256  string                   `json:"worker_wait_sha256,omitempty"`
	UnlaunchedSHA256  string                   `json:"unlaunched_sha256,omitempty"`
}

// OperationResult is the managed worker's candidate, qualified by the guardian
// before the coordinator accepts it as a completed workspace operation.
type OperationResult struct {
	SchemaVersion int        `json:"schema_version"`
	OperationID   string     `json:"operation_id"`
	RequestSHA256 string     `json:"request_sha256"`
	Status        string     `json:"status"`
	ReasonCode    string     `json:"reason_code,omitempty"`
	CommandExit   *int       `json:"command_exit,omitempty"`
	Artifacts     []Artifact `json:"artifacts,omitempty"`
}

func ValidateSubject(subject Subject) error {
	if subject.Kind != SubjectTask && subject.Kind != SubjectOperation && subject.Kind != SubjectAdmission {
		return task.ErrIdentityMismatch
	}
	return errors.Join(task.ValidateTaskID(subject.ID), task.ValidateSHA256(subject.SHA256))
}

func ValidateOperation(request OperationRequest, rootID string) (time.Time, error) {
	if request.SchemaVersion != task.SchemaVersion || request.RootID != rootID {
		return time.Time{}, task.ErrIdentityMismatch
	}
	if err := errors.Join(task.ValidateRootID(rootID), task.ValidateTaskID(request.OperationID), task.ValidateTaskID(request.WorkspaceID), ValidateSubject(request.Subject)); err != nil {
		return time.Time{}, err
	}
	switch request.Kind {
	case OperationProvision, OperationCapture, OperationVerify, OperationRemove, OperationForget:
	default:
		return time.Time{}, task.ErrIdentityMismatch
	}
	deadline, err := time.Parse(time.RFC3339Nano, request.Deadline)
	if err != nil || deadline.IsZero() || deadline.UTC().Format(time.RFC3339Nano) != request.Deadline {
		return time.Time{}, task.ErrIdentityMismatch
	}
	if !filepath.IsAbs(request.WorkerExecutable) || filepath.Clean(request.WorkerExecutable) != request.WorkerExecutable || strings.ContainsRune(request.WorkerExecutable, 0) {
		return time.Time{}, task.ErrIdentityMismatch
	}
	if err = task.ValidateSHA256(request.WorkerSHA256); err != nil {
		return time.Time{}, err
	}
	if err = task.ValidateFreshSupervisorRef(request.Supervisor); err != nil {
		return time.Time{}, err
	}
	if err = ValidateOperationArguments(request); err != nil {
		return time.Time{}, err
	}
	return deadline, nil
}

func ValidateOperationArguments(request OperationRequest) error {
	if request.Kind == OperationVerify {
		if len(request.Arguments) == 0 || request.Arguments[0] == "" {
			return task.ErrIdentityMismatch
		}
	} else if len(request.Arguments) != 0 {
		return task.ErrIdentityMismatch
	}
	for _, argument := range request.Arguments {
		if strings.ContainsRune(argument, 0) {
			return task.ErrIdentityMismatch
		}
	}
	if request.Discard && request.Kind != OperationRemove {
		return task.ErrIdentityMismatch
	}
	return nil
}
