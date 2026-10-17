package task

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

// ReviewTarget identifies the repository state a native code review examines.
// Value is empty only for uncommitted changes.
type ReviewTarget struct {
	Kind  string `json:"kind"`
	Value string `json:"value,omitempty"`
}

const (
	ReviewTargetUncommitted = "uncommitted"
	ReviewTargetBase        = "base"
	ReviewTargetCommit      = "commit"
)

// TaskConfig captures requested configuration parameters.
type TaskConfig struct {
	Interactive   bool          `json:"interactive,omitempty"`
	Model         string        `json:"model,omitempty"`
	Effort        string        `json:"effort,omitempty"`
	Permission    string        `json:"permission,omitempty"`
	Budget        string        `json:"budget,omitempty"`
	NativeTimeout string        `json:"native_timeout,omitempty"`
	ReviewTarget  *ReviewTarget `json:"review_target,omitempty"`
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

// SupervisorRef records the private supervisor config and endpoint plus the
// executable and resolver provenance selected by the dispatching CLI.
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

// SameSupervisorIdentity reports whether two refs point to the same
// supervisor configuration and endpoint, independent of installation details.
func SameSupervisorIdentity(left, right SupervisorRef) bool {
	return left.ConfigPath == right.ConfigPath &&
		left.ConfigDigest == right.ConfigDigest &&
		left.ResolvedConfigSHA256 == right.ResolvedConfigSHA256 &&
		left.Endpoint == right.Endpoint
}

// RootRecord is stored in root.json.
type RootRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RootID        string `json:"root_id"`
	CreatedAt     string `json:"created_at"`
}

// TaskRecord is stored in task.json (the immutable task request).
type TaskRecord struct {
	SchemaVersion   int               `json:"schema_version"`
	RootID          string            `json:"root_id"`
	TaskID          string            `json:"task_id"`
	Provider        string            `json:"provider"`
	Mode            string            `json:"mode"`
	CanonicalCwd    string            `json:"canonical_cwd"`
	RequestedConfig TaskConfig        `json:"requested_config"`
	BudgetNanos     int64             `json:"budget_nanos"`
	PriorSession    *PriorSession     `json:"prior_session,omitempty"`
	BriefSHA256     string            `json:"brief_sha256"`
	BriefLength     int64             `json:"brief_length"`
	Workspace       *WorkspaceBinding `json:"workspace,omitempty"`
}

// WorkspaceBinding identifies the resource usage owned by this request. The
// workspace journal owns checkout and capture state; task.json only pins use.
type WorkspaceBinding struct {
	ID            string `json:"id"`
	Reservation   uint64 `json:"reservation"`
	RequestSHA256 string `json:"request_sha256"`
	BudgetNanos   int64  `json:"budget_nanos"`
}
