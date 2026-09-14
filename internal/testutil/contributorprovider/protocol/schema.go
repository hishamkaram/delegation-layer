// Package protocol defines the synthetic contributor-proof wire and fixture
// configuration schema. It imports no application, task, provider, or
// certification code so the fixture executable cannot depend on its future
// catalog registration.
package protocol

const (
	SchemaVersion = 1
	Protocol      = "contributor-proof/v1"

	RuntimeInputName = "runtime.json"
	ToolsInputName   = "tools.json"
	OutputName       = "answer.txt"

	CasePresent      = "present"
	CaseAbsent       = "absent"
	CaseEmpty        = "empty"
	CaseConflict     = "conflict"
	CaseRejected     = "rejected"
	CaseMalformed    = "malformed"
	CaseWrongTask    = "wrong-task"
	CaseWrongSession = "wrong-session"
	CaseResume       = "resume"
	CaseNonzero      = "nonzero"
)

// MaxEnvelopeBytes stays within the shared strict JSON decoder's bound.
// Complete raw evidence remains owned and sealed by the core.
const MaxEnvelopeBytes = 1 << 20

// MaxBriefBytes reserves serialization headroom before admission. A JSON
// string can expand to six bytes per input byte, and envelope/session metadata
// needs fewer than 512 additional bytes. Every admitted brief therefore fits
// the strict output and session-record limits, including escaped answers.
const MaxBriefBytes = (MaxEnvelopeBytes - 512) / 6

// Brief is the finite JSON request sent to the fixture over stdin. Answer and
// nonce are omitted on a resume case so the provider must recover the nonce
// from its private runtime session file.
type Brief struct {
	Case   string `json:"case"`
	Answer string `json:"answer,omitempty"`
	Nonce  string `json:"nonce,omitempty"`
}

// Envelope is the complete one-turn stdout record. Strict decoding rejects
// missing, duplicate, unknown, trailing, and incorrectly typed fields.
type Envelope struct {
	Protocol  string `json:"protocol"`
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Answer    string `json:"answer"`
}

// RuntimeConfig is the non-secret provider runtime configuration materialized
// by the core. The directory must be outside the workspace and task state.
type RuntimeConfig struct {
	SchemaVersion int    `json:"schema_version"`
	RuntimeDir    string `json:"runtime_dir"`
}

// ToolsConfig is the minimal non-secret policy configuration materialized by
// the core. The fixture performs no tool operation; workspace writes are
// always forbidden.
type ToolsConfig struct {
	SchemaVersion  int      `json:"schema_version"`
	AllowedTools   []string `json:"allowed_tools"`
	WorkspaceWrite bool     `json:"workspace_write"`
}
