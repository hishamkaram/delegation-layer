// Package claude contains the bounded Claude print adapter.
package claude

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	Provider                       = "claude:print"
	Mode                           = config.ModeReadOnly
	WorkspaceWriteMode             = config.ModeWorkspaceWrite
	Version                        = "runtime-reported"
	ProfileRevision                = "claude-read-only-v1"
	WorkspaceWriteProfileRevision  = "claude-workspace-write-v1"
	predicateVersion               = "native-permissions-v1"
	workspaceWritePredicateVersion = "native-permissions-v2"

	// The legacy reference keeps already-sealed read-only evidence collectible
	// after release-version gates were removed. It is never used for admission.
	legacyPredicateVersion = "2.1.270"
	legacyPredicateSHA256  = "b92db06be971c081c653322a6cc9d12ff6996066019df05e0dd6989ab62b3021"

	// legacyNativePredicateSHA256 is the immutable native read-only predicate
	// recorded before the provider-owned plan permission was adopted. It remains
	// registered so pending tasks admitted with dontAsk can be replayed exactly.
	legacyNativePredicateSHA256 = "c69794753071f23db76e4b5b00cd0c054c0b9f00d30c4fcae093e29f6989ef2c"

	maxEventLineBytes = task.MaxControlRecordSize
	answerChunkBytes  = 32 * 1024
	maxModelRecords   = 128
	maxModelNameBytes = 256
)

// nativeContractV1 is the current evidence policy for Claude's stream-json
// producer. Claude owns its configured tools, MCP servers, authentication,
// hooks, plugins, skills, and policy; this predicate validates their reported
// shape and the requested permission mode without imposing an inventory.
const nativeContractV1 = `{"adapter":"claude:print","mode":"read-only","version":"native-permissions-v1","native":"Claude Code print stream-json producer with provider-owned configuration and permissions","events":{"line":"one UTF-8 JSON object per line, each line at most 1048576 bytes; the final line may omit its newline; duplicate keys, including case-folded aliases, reject at every nesting level","init":"exactly one system/init object is required; its session_id is a canonical UUID, claude_code_version is a non-empty valid CLI-reported version string, permissionMode is plan, and provider-owned apiKeySource, tools, and mcp_servers fields are accepted when structurally valid","messages":"assistant and user events occur only after init; message content blocks are structurally validated; tool names and tool results are provider-owned and structurally validated; invalid content and pre-init messages reject; assistant text is ignored for answer selection","result":"exactly one result object is required after init; subtype=success, is_error=false, and a non-whitespace string result are required; any errors entry, interruption, max_tokens truncation, refusal, unknown stop_reason, substantive assistant/user continuation after result, conflicting results, and identity conflicts reject; null, end_turn, and stop_sequence stop reasons may complete; permission_denials, when present, are an array of non-null objects; benign trailing system events are allowed","selection":"only the top-level result.result string is an answer; assistant text and nested or subagent messages are never selected","unknown":"unknown telemetry is tolerated; unknown result subtypes and unknown thread/session/turn lifecycle events are terminally ambiguous and reject","bounds":"the parser retains one bounded line and bounded scalar fields only; modelUsage has at most 128 model records"},"success":"invocation started, exit_code=0, sealed error empty, exact expected and recorded session identity, valid init, one successful result, and a non-whitespace result string","failure":"malformed or oversized JSONL, invalid UTF-8, invalid init permission mode, missing result, result error subtype, provider refusal or limit, nonzero exit, start failure, seal error, and operational reader or writer faults never publish","usage":"result.usage is main-agent provider-envelope accounting; result.modelUsage is whole-tree provider-envelope accounting; total_cost_usd is a separate whole-tree provider estimate; missing counters remain null and totals are never synthesized or differenced; rejected-result accounting is unreliable","identity":"every event session_id is bound before init and must remain exact; fresh sessions use UUIDv5 with the decoded root ID as namespace and canonical task ID as the ASCII name; continuation uses the exact recorded session UUID"}` + "\n"

const nativeWorkspaceWriteContractV1 = `{"adapter":"claude:print","mode":"workspace-write","version":"native-permissions-v1","native":"Claude Code print stream-json producer with provider-owned configuration and permissions","init":"permissionMode=acceptEdits; provider-owned apiKeySource, tools, and mcp_servers fields are accepted when structurally valid","events":"the same strict UTF-8 JSONL, identity, message, result, usage, and bounded-output rules as the native read-only interpreter","success":"invocation started, exit_code=0, sealed error empty, exact expected and recorded session identity, valid write-mode init, one successful non-whitespace result","failure":"malformed, truncated, conflicting, identity-mismatched, provider-failed, or non-terminal output never publishes"}` + "\n"

// nativeWorkspaceWriteContractV2 is the current native workspace-write
// predicate. The V1 bytes above remain immutable for admitted acceptEdits
// tasks that need to be replayed.
const nativeWorkspaceWriteContractV2 = `{"adapter":"claude:print","mode":"workspace-write","version":"native-permissions-v2","native":"Claude Code print stream-json producer with provider-owned configuration and permissions","init":"permissionMode=bypassPermissions; provider-owned apiKeySource, tools, and mcp_servers fields are accepted when structurally valid","events":"the same strict UTF-8 JSONL, identity, message, result, usage, and bounded-output rules as the native read-only interpreter","success":"invocation started, exit_code=0, sealed error empty, exact expected and recorded session identity, valid write-mode init, one successful non-whitespace result","failure":"malformed, truncated, conflicting, identity-mismatched, provider-failed, or non-terminal output never publishes"}` + "\n"

// contractV1 is the immutable historical portable evidence policy for
// Claude's stream-json producer. Its bytes, including the final newline, are
// retained for reconstruction of tasks admitted before native policy ownership.
// producer. Its bytes, including the final newline, are the predicate digest.
// The predicate revision is independent of the installed CLI release; the
// init version is only required to be a valid non-empty reported value.
const contractV1 = `{"adapter":"claude:print","mode":"read-only","version":"runtime-reported","native":"Claude Code print stream-json producer","events":{"line":"one UTF-8 JSON object per line, each line at most 1048576 bytes; the final line may omit its newline; duplicate keys, including case-folded aliases, reject at every nesting level","init":"exactly one system/init object is required; its session_id is a canonical UUID, claude_code_version is a non-empty valid CLI-reported version string, apiKeySource is none, permissionMode is dontAsk, tools contains Read, Glob, and Grep and no other tool except EndConversation, and mcp_servers is an empty array","messages":"assistant and user events occur only after init; message content blocks are structurally validated; assistant tool_use names are restricted to Read, Glob, Grep, and EndConversation; unsupported tool_use, invalid content, and pre-init messages reject; assistant text is ignored for answer selection","result":"exactly one result object is required after init; subtype=success, is_error=false, and a non-whitespace string result are required; any errors entry, interruption, max_tokens truncation, refusal, unknown stop_reason, substantive assistant/user continuation after result, conflicting results, and identity conflicts reject; null, end_turn, and stop_sequence stop reasons may complete; permission_denials, when present, are an array of non-null objects; benign trailing system events are allowed","selection":"only the top-level result.result string is an answer; assistant text and nested or subagent messages are never selected","unknown":"unknown telemetry is tolerated; unknown result subtypes and unknown thread/session/turn lifecycle events are terminally ambiguous and reject","bounds":"the parser retains one bounded line and bounded scalar fields only; modelUsage has at most 128 model records"},"success":"invocation started, exit_code=0, sealed error empty, exact expected and recorded session identity, valid init, one successful result, and a non-whitespace result string","failure":"malformed or oversized JSONL, invalid UTF-8, invalid init policy, missing result, result error subtype, provider refusal or limit, nonzero exit, start failure, seal error, and operational reader or writer faults never publish","usage":"result.usage is main-agent provider-envelope accounting; result.modelUsage is whole-tree provider-envelope accounting; total_cost_usd is a separate whole-tree provider estimate; missing counters remain null and totals are never synthesized or differenced; rejected-result accounting is unreliable","identity":"every event session_id is bound before init and must remain exact; fresh sessions use UUIDv5 with the decoded root ID as namespace and canonical task ID as the ASCII name; continuation uses the exact recorded session UUID"}` + "\n"

const workspaceWriteContractV1 = `{"adapter":"claude:print","mode":"workspace-write","version":"runtime-reported","native":"Claude Code print stream-json producer","init":"permissionMode=acceptEdits and tools contain Read, Edit, Write, Glob, and Grep, with no other tool except EndConversation; mcp_servers is empty and apiKeySource is none","events":"the same strict UTF-8 JSONL, identity, message, result, usage, and bounded-output rules as the read-only interpreter","success":"invocation started, exit_code=0, exact expected and recorded session identity, valid write-mode init, one successful non-whitespace result","failure":"malformed, truncated, conflicting, identity-mismatched, provider-failed, or non-terminal output never publishes"}` + "\n"

// Contract returns the current native canonical bytes whose digest identifies
// the interpreter.
func Contract() string { return nativeContractV1 }

// ContractDigest returns the lowercase SHA-256 digest bound into Reference.
func ContractDigest() string { return task.ComputeSHA256([]byte(nativeContractV1)) }

// Reference returns the exact predicate reference for Claude print output.
func Reference() task.PredicateRef {
	return ReferenceForMode(Mode)
}

// WorkspaceWriteContract returns the current native workspace-write contract.
func WorkspaceWriteContract() string { return nativeWorkspaceWriteContractV2 }

// WorkspaceWriteReference returns a predicate reference bound to the native
// bypassPermissions profile. Keeping a distinct reference prevents a read-only
// result from satisfying a workspace-write task.
func WorkspaceWriteReference() task.PredicateRef {
	return ReferenceForMode(WorkspaceWriteMode)
}

// ContractForMode returns the immutable output contract for mode.
func ContractForMode(mode string) string {
	if mode == Mode {
		return nativeContractV1
	}
	if mode == WorkspaceWriteMode {
		return nativeWorkspaceWriteContractV2
	}
	return ""
}

// ReferenceForMode returns the predicate reference bound to mode.
func ReferenceForMode(mode string) task.PredicateRef {
	contract := ContractForMode(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	version := predicateVersion
	if mode == WorkspaceWriteMode {
		version = workspaceWritePredicateVersion
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: task.ComputeSHA256([]byte(contract))}
}

func legacyNativeReferenceForMode(mode string) task.PredicateRef {
	switch mode {
	case Mode:
		return task.PredicateRef{Adapter: Provider, Mode: mode, Version: predicateVersion, SHA256: legacyNativePredicateSHA256}
	case WorkspaceWriteMode:
		return task.PredicateRef{Adapter: Provider, Mode: mode, Version: predicateVersion, SHA256: task.ComputeSHA256([]byte(nativeWorkspaceWriteContractV1))}
	default:
		return task.PredicateRef{}
	}
}

func legacyPortableContractForMode(mode string) string {
	switch mode {
	case Mode:
		return contractV1
	case WorkspaceWriteMode:
		return workspaceWriteContractV1
	default:
		return ""
	}
}

func legacyPortableReferenceForMode(mode string) task.PredicateRef {
	contract := legacyPortableContractForMode(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: Version, SHA256: task.ComputeSHA256([]byte(contract))}
}

// LegacyReference returns the historical read-only predicate reference solely
// for collection of tasks sealed before runtime capability checks existed.
func LegacyReference() task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: Mode, Version: legacyPredicateVersion, SHA256: legacyPredicateSHA256}
}

// validSessionID accepts the canonical UUID-shaped identifiers emitted by
// Claude. Matching against expected and recorded identities remains exact.
func validSessionID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for index := 0; index < len(id); index++ {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if id[index] != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", rune(id[index])) {
			return false
		}
	}
	return true
}
