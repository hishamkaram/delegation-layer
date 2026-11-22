// Package workspacefiles observes selected filesystem scopes without creating
// Native roots, tasks, approvals, pins or cursor state. A call owns its rooted
// descriptors; saved local approvals remain the authority for workspace access.
package workspacefiles

import "time"

const (
	SchemaVersion    = 1
	MaxEntries       = 100
	MaxReadBytes     = 65536
	MaxResponseBytes = 1 << 20
)

type Operation string

const (
	List    Operation = "list"
	Read    Operation = "read"
	Inspect Operation = "inspect"
	Git     Operation = "git"
	Scopes  Operation = "scopes"
)

type ScopeKind string

const (
	Workspace  ScopeKind = "workspace"
	UserTemp   ScopeKind = "user_temp"
	SharedTemp ScopeKind = "shared_temp"
)

// Scope is a local-only selection. Directory and ExpectedWorkspace must come
// from the connector's saved approval, never from a remote raw pathname.
type Scope struct {
	Kind              ScopeKind `json:"kind"`
	Directory         string    `json:"directory,omitempty"`
	ExpectedWorkspace string    `json:"expected_workspace,omitempty"`
}

type Request struct {
	ExpectedRoot       string `json:"expected_root,omitempty"`
	Scope              Scope  `json:"scope,omitempty"`
	RelativePath       string `json:"relative_path,omitempty"`
	RelativePathBase64 string `json:"relative_path_base64,omitempty"`
	Cursor             string `json:"cursor,omitempty"`
	Limit              int    `json:"limit,omitempty"`
	Offset             int64  `json:"offset,omitempty"`
	ExpectedVersion    string `json:"expected_version,omitempty"`
}

// Result is a tagged union: exactly the pointer matching Operation is set.
// Git and Inspect both return Inspect. ObservedAt is a dated observation, not a
// filesystem snapshot or authority over subsequent path replacement.
type Result struct {
	SchemaVersion int            `json:"schema_version"`
	Operation     Operation      `json:"operation"`
	ObservedAt    time.Time      `json:"observed_at"`
	List          *ListResult    `json:"list,omitempty"`
	Read          *ReadResult    `json:"read,omitempty"`
	Inspect       *InspectResult `json:"inspect,omitempty"`
	Scopes        *ScopesResult  `json:"scopes,omitempty"`
}

type EntryKind string

const (
	RegularFile EntryKind = "file"
	Directory   EntryKind = "directory"
	Symlink     EntryKind = "symlink"
	FIFO        EntryKind = "fifo"
	Socket      EntryKind = "socket"
	Device      EntryKind = "device"
	Other       EntryKind = "other"
)

// Base64 fields use standard padded base64 and preserve arbitrary Unix names.
// Text is present only for valid UTF-8. No filename or content filtering occurs.
type Entry struct {
	Name               *string    `json:"name,omitempty"`
	NameBase64         string     `json:"name_base64"`
	RelativePath       *string    `json:"relative_path,omitempty"`
	RelativePathBase64 string     `json:"relative_path_base64"`
	Kind               EntryKind  `json:"kind"`
	Size               int64      `json:"size"`
	ModifiedAt         time.Time  `json:"modified_at"`
	Version            string     `json:"version"`
	Git                *GitResult `json:"git,omitempty"`
}

type ListResult struct {
	Entries    []Entry `json:"entries"`
	Version    string  `json:"version"`
	NextCursor string  `json:"next_cursor,omitempty"`
	EOF        bool    `json:"eof"`
}

type ReadResult struct {
	DataBase64 string  `json:"data_base64"`
	Text       *string `json:"text,omitempty"`
	Offset     int64   `json:"offset"`
	NextOffset int64   `json:"next_offset"`
	EOF        bool    `json:"eof"`
	Version    string  `json:"version"`
}

type InspectResult struct {
	Entry Entry      `json:"entry"`
	Git   *GitResult `json:"git,omitempty"`
}

type GitStatus string

const (
	GitCandidate     GitStatus = "candidate"
	GitValidated     GitStatus = "validated"
	GitPartial       GitStatus = "partial"
	GitNotRepository GitStatus = "not_repository"
	GitFault         GitStatus = "fault"
)

type GitKind string

const (
	GitWorktree       GitKind = "worktree"
	GitLinkedWorktree GitKind = "linked_worktree"
	GitBare           GitKind = "bare"
)

// GitResult reports structural evidence. A marker is only a candidate. Partial
// means metadata could not be inspected within this same selected root.
type GitResult struct {
	Status             GitStatus `json:"status"`
	Kind               GitKind   `json:"kind,omitempty"`
	Reason             string    `json:"reason,omitempty"`
	Marker             string    `json:"marker,omitempty"`
	MetadataPathBase64 string    `json:"metadata_path_base64,omitempty"`
	HEAD               *string   `json:"head,omitempty"`
	HEADBase64         string    `json:"head_base64,omitempty"`
	HEADShape          string    `json:"head_shape,omitempty"`
}

type ObservedScope struct {
	ID        string    `json:"id"`
	Kind      ScopeKind `json:"kind"`
	Name      string    `json:"name"`
	Directory string    `json:"directory"`
}

type ScopesResult struct {
	Scopes []ObservedScope `json:"scopes"`
}

type ErrorCode string

const (
	NotFound         ErrorCode = "not_found"
	NotDirectory     ErrorCode = "not_directory"
	NotRegularFile   ErrorCode = "not_regular_file"
	OutsideScope     ErrorCode = "outside_scope"
	PermissionDenied ErrorCode = "os_permission_denied"
	Changed          ErrorCode = "changed"
	Timeout          ErrorCode = "timeout"
	InvalidRequest   ErrorCode = "invalid_request"
	Unsupported      ErrorCode = "unsupported_feature"
	ResourceLimit    ErrorCode = "resource_limit"
	TempUnavailable  ErrorCode = "temp_unavailable"
)

// Error deliberately excludes file names, contents and raw OS/Git diagnostics.
// The wrapped cause is retained for local errors.Is inspection.
type Error struct {
	Code      ErrorCode `json:"code"`
	Stage     string    `json:"stage"`
	Retryable bool      `json:"retryable"`
	cause     error
}

func (e *Error) Error() string { return "filesystem " + e.Stage + ": " + string(e.Code) }
func (e *Error) Unwrap() error { return e.cause }
