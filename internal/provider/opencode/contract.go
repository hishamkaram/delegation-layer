// Package opencode contains the bounded direct OpenCode CLI adapter.
//
// The adapter invokes `opencode run --format json` and interprets only the
// finite JSONL stream emitted by that command.  It does not own process
// lifetime, capture, or publication; those responsibilities remain in the
// execution core.
package opencode

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	Provider           = config.ProviderOpenCodeRun
	ModeReadOnly       = config.ModeReadOnly
	ModeWorkspaceWrite = config.ModeWorkspaceWrite
	// Mode is the provider's default/primary profile and is kept as an alias
	// for callers that use the single-mode provider convention.
	Mode                 = ModeWorkspaceWrite
	ProfileRevision      = "opencode-direct-cli-v1"
	OutputWriterContract = ""

	predicateVersion = "1"

	maxEventLineBytes = task.MaxControlRecordSize
	maxAnswerBytes    = task.MaxBriefSize
	answerChunkBytes  = 32 * 1024
	maxUsageRows      = 1024
)

// contractReadOnly and contractWorkspaceWrite are immutable descriptions of
// the JSONL result contract.  They describe adapter behavior, not an
// OpenCode release and therefore remain valid as the CLI evolves.
const contractReadOnly = `{"adapter":"opencode:run","mode":"read-only","version":"1","native":"OpenCode run JSONL producer","permissions":"read-only selects the adapter-owned delegation-layer-read-only agent, runs in pure mode, disables project configuration and automatic compaction, passes inline OPENCODE_CONFIG_CONTENT with explicit deny rules at both global and agent scope for mutation, shell, subagent, network, and unknown tools while allowing read, grep, glob, and list, and verifies the effective debug configuration before admission and launch","events":{"line":"one UTF-8 JSON object per line, at most 1048576 bytes; final line may omit newline","identity":"every event has one exact sessionID in the ses_ form; exactly one session is accepted and fresh or resumed identity is checked","steps":"step_start opens one step; step_finish closes it; reason=tool-calls permits another step and reason=stop is the sole terminal result","text":"text part.text strings are accumulated only inside an active step; reasoning and completed tool events are structurally checked but are not answers","tool":"tool_use state completed or error is structurally checked; an error may be followed by a successful retry","error":"error events are provider failures","unknown":"unknown event types and malformed or conflicting event shapes reject","bounds":"captured event lines, retained answer text, and at most 1024 usage rows are bounded"},"success":"exit_code=0, sealed error empty, one matching session identity, at least one step_start, a terminal step_finish with reason stop, and non-whitespace text","failure":"malformed or oversized JSONL, invalid UTF-8, missing or conflicting identity, missing terminal step_finish, empty answer, provider error, nonzero exit, start failure, seal error, read-only policy drift, or usage row bound exceeded rejects","usage":"each completed step's provider-event accounting is exposed with unknown scope; no conversation total is synthesized"}` + "\n"

const contractWorkspaceWrite = `{"adapter":"opencode:run","mode":"workspace-write","version":"1","native":"OpenCode run JSONL producer","permissions":"workspace-write uses an adapter-owned agent with explicit read, search, and edit allows plus deny rules for shell, subagents, network, and unknown tools, keeps external_directory:deny for path-based tools, disables project configuration and automatic compaction, runs in pure mode, and passes the caller-selected native --auto approval; workspace-write requires the selected workspace to be the Git checkout root because native patch moves can widen destinations within a checkout","events":{"line":"one UTF-8 JSON object per line, at most 1048576 bytes; final line may omit newline","identity":"every event has one exact sessionID in the ses_ form; exactly one session is accepted and fresh or resumed identity is checked","steps":"step_start opens one step; step_finish closes it; reason=tool-calls permits another step and reason=stop is the sole terminal result","text":"text part.text strings are accumulated only inside an active step; reasoning and completed tool events are structurally checked but are not answers","error":"error events are provider failures","unknown":"unknown event types and malformed or conflicting event shapes reject","bounds":"captured event lines, retained answer text, and at most 1024 usage rows are bounded"},"success":"exit_code=0, sealed error empty, one matching session identity, at least one step_start, a terminal step_finish with reason stop, and non-whitespace text","failure":"malformed or oversized JSONL, invalid UTF-8, missing or conflicting identity, missing terminal step_finish, empty answer, provider error, nonzero exit, start failure, seal error, workspace policy drift, or usage row bound exceeded rejects","usage":"each completed step's part.tokens and part.cost are copied as an independent provider-event record with unknown scope; no conversation total is synthesized"}` + "\n"

// Contract returns the default workspace-write predicate contract.
func Contract() string { return contractWorkspaceWrite }

// ContractDigest returns the digest of the default workspace-write contract.
func ContractDigest() string { return task.ComputeSHA256([]byte(contractWorkspaceWrite)) }

// ContractForMode returns the immutable contract bytes for a supported mode.
func ContractForMode(mode string) string {
	if mode == ModeReadOnly {
		return contractReadOnly
	}
	if mode == ModeWorkspaceWrite {
		return contractWorkspaceWrite
	}
	return ""
}

// Reference returns the predicate reference for the default workspace-write
// profile.
func Reference() task.PredicateRef { return ReferenceForMode(ModeWorkspaceWrite) }

// ReferenceForMode returns the exact predicate reference for mode. An
// unsupported mode returns the zero reference and is rejected by registration
// or request validation before launch.
func ReferenceForMode(mode string) task.PredicateRef {
	contract := ContractForMode(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{
		Adapter: Provider,
		Mode:    mode,
		Version: predicateVersion,
		SHA256:  task.ComputeSHA256([]byte(contract)),
	}
}

func validSessionID(id string) bool {
	if len(id) < 5 || len(id) > 128 || !strings.HasPrefix(id, "ses_") {
		return false
	}
	for _, value := range id[4:] {
		if (value < 'a' || value > 'z') && (value < 'A' || value > 'Z') && (value < '0' || value > '9') && value != '_' && value != '-' {
			return false
		}
	}
	return true
}
