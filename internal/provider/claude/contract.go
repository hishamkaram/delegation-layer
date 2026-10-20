// Package claude contains the bounded Claude print adapter.
package claude

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	Provider                      = "claude:print"
	Mode                          = config.ModeReadOnly
	WorkspaceWriteMode            = config.ModeWorkspaceWrite
	Version                       = "runtime-reported"
	ProfileRevision               = "claude-read-only-v1"
	WorkspaceWriteProfileRevision = "claude-workspace-write-v1"
	// The v1 read-only and v2 workspace-write predicates retain the parser
	// used before issue47. The previous current revisions remain registered
	// with their original runtime probe; fresh tasks use the relaxed probe.
	historicalNativePredicateVersion               = "native-permissions-v1"
	historicalNativeWorkspaceWritePredicateVersion = "native-permissions-v2"
	legacyNativeWorkspaceWritePredicateVersion     = "native-permissions-v1"
	previousNativeProbePredicateVersion            = "native-permissions-v2"
	previousNativeProbeWorkspaceWriteVersion       = "native-permissions-v3"
	// The pre-attribution revisions bind the relaxed probe and the
	// single-result completion policy. Fresh tasks use the attributed policy.
	preAttributionPredicateVersion               = "native-permissions-v3"
	preAttributionWorkspaceWritePredicateVersion = "native-permissions-v4"
	// The scoped revisions share their version strings with the retained
	// unscoped revisions and differ by digest. Both are frozen for replay.
	scopedPredicateVersion               = "native-permissions-v4"
	scopedWorkspaceWritePredicateVersion = "native-permissions-v5"
	// The current revisions bind the advisory wire projection under version
	// strings no earlier digest of the same mode uses.
	predicateVersion               = "native-permissions-v5"
	workspaceWritePredicateVersion = "native-permissions-v6"

	// The legacy reference keeps already-sealed read-only evidence collectible
	// after release-version gates were removed. It is never used for admission.
	legacyPredicateVersion = "2.1.270"
	legacyPredicateSHA256  = "b92db06be971c081c653322a6cc9d12ff6996066019df05e0dd6989ab62b3021"

	// legacyNativePredicateSHA256 is the immutable native read-only predicate
	// recorded before the provider-owned plan permission was adopted. It remains
	// registered so pending tasks admitted with dontAsk can be replayed exactly.
	legacyNativePredicateSHA256 = "c69794753071f23db76e4b5b00cd0c054c0b9f00d30c4fcae093e29f6989ef2c"

	maxEventLineBytes        = task.MaxControlRecordSize
	answerChunkBytes         = 32 * 1024
	maxModelRecords          = 128
	maxModelNameBytes        = 256
	maxCurrentToolCount      = 4096
	maxInteractionInputBytes = 256 * 1024
	maxInteractionTextBytes  = 16 * 1024
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

func previousNativeProbeContractForMode(mode string) string {
	if mode == Mode {
		contract := strings.Replace(nativeContractV1, `"version":"native-permissions-v1"`, `"version":"native-permissions-v2"`, 1)
		contract = strings.Replace(contract, "provider-owned apiKeySource, tools, and mcp_servers fields are accepted when structurally valid", "provider-owned apiKeySource, tools (at most 4096 names), and mcp_servers fields are accepted when structurally valid", 1)
		contract = strings.Replace(contract, "one UTF-8 JSON object per line, each line at most 1048576 bytes;", "one UTF-8 JSON object per line;", 1)
		contract = strings.Replace(contract, "the parser retains one bounded line and bounded scalar fields only; modelUsage has at most 128 model records", "the parser retains offsets, hashes, and bounded scalar fields only; final result text is replayed from sealed offsets; modelUsage remains optional accounting", 1)
		return contract
	}
	if mode == WorkspaceWriteMode {
		contract := strings.Replace(nativeWorkspaceWriteContractV2, `"version":"native-permissions-v2"`, `"version":"native-permissions-v3"`, 1)
		contract = strings.Replace(contract, "provider-owned apiKeySource, tools, and mcp_servers fields are accepted when structurally valid", "provider-owned apiKeySource, tools (at most 4096 names), and mcp_servers fields are accepted when structurally valid", 1)
		contract = strings.Replace(contract, "the same strict UTF-8 JSONL, identity, message, result, usage, and bounded-output rules", "the same strict UTF-8 JSONL, identity, message, result, usage, and offset-streamed answer rules", 1)
		return contract
	}
	return ""
}

func preAttributionNativeContractForMode(mode string) string {
	contract := previousNativeProbeContractForMode(mode)
	if contract == "" {
		return ""
	}
	if mode == Mode {
		contract = strings.Replace(contract, `"version":"native-permissions-v2"`, `"version":"native-permissions-v3"`, 1)
	} else {
		contract = strings.Replace(contract, `"version":"native-permissions-v3"`, `"version":"native-permissions-v4"`, 1)
	}
	return strings.Replace(contract, `,"success":`, `,"runtime_probe":{"required_flags":["--print"]},"success":`, 1)
}

// unscopedCompletionAttributionContract is the policy text of the retained
// read-only (native-permissions-v4) and workspace-write (native-permissions-v5)
// revisions sealed before message scope existed: it opens a follow-up cycle for
// every assistant or user event after a result, whatever its
// parent_tool_use_id. Its bytes are frozen with those revisions' digests.
const unscopedCompletionAttributionContract = `"completion_attribution":"the first result answers the caller turn: its origin is absent or kind=human, and its result_index, when present, is 0; later results in the same invocation are accepted only as provider-native background notifications: every result then carries result_index, indexes are consecutive without gap or duplicate, and each later result has origin kind=task-notification without subkind; another or unknown origin, any subkind, a missing or mixed counter, a second result without counters, or a later failed, refused, interrupted, or truncated result rejects; for every result, terminal_reason when present must be completed, documented max_turns, api_error, aborted_streaming, and aborted_tools reasons reject as failures, and any other value is malformed; queued_turn_count when present must be 0 and a positive count rejects as queued human input; an explicit null result_index, origin, terminal_reason, or queued_turn_count is malformed; an empty or missing follow-up answer settles only as a coalesced batch acknowledgement with num_turns=0, which stays pending until a later non-empty attributed result, and any other empty follow-up or a pending acknowledgement at EOF rejects; a follow-up cycle may repeat system/init only before its messages and only with the same session, version, cwd, permission mode, apiKeySource, model, tools, and MCP server count; assistant or user events after a result open a follow-up cycle that must end with an attributed result before EOF; only the principal result is selected as the answer, follow-up answers are never selected, and principal failure, refusal, empty answer, or identity conflict rejects regardless of later follow-ups; main-agent usage, num_turns, and duration come from the principal result, while cumulative whole-tree total_cost_usd comes from the last attributed result and is never summed"`

// scopedCompletionAttributionContract is the frozen policy text of the
// retained scoped read-only (native-permissions-v4) and workspace-write
// (native-permissions-v5) revisions, which CCR v5 also embeds: leading
// acknowledgements may precede the caller's result, root messages open
// follow-up cycles, nested agent messages are validated and excluded, and an
// invalid message scope is malformed. Its bytes are frozen with those
// revisions' digests; the current advisory revisions derive from it through
// advisoryCompletionEdits.
const scopedCompletionAttributionContract = `"completion_attribution":"the first result that is not a leading acknowledgement answers the caller turn: its origin is absent or kind=human, and its result_index, when present, is 0, or, after leading acknowledgements, is the next consecutive index and is then required; a leading acknowledgement precedes the caller's result and is a result with origin kind=task-notification without subkind, subtype success, is_error false, an empty string result, num_turns explicitly 0, and result_index starting at 0 and consecutive, accepted after the initial system/init and before any root assistant or user message since that init or the previous acknowledgement, with one identical repeated system/init permitted once after an acknowledgement and before root messages; an acknowledgement is never the answer, never contributes usage or a cost baseline, and never closes an interactive input; a failed, refused, aborted, or queued acknowledgement always rejects and reports its canonical refusal even when the caller's result succeeds or never arrives, unless a higher-priority malformed or incomplete fault takes precedence, which still rejects; a stream of only acknowledgements is incomplete, and a nonempty, missing, mistyped, or wrongly counted acknowledgement, a duplicate or unmatched repeated init, or a changed identity is malformed; later results in the same invocation are accepted only as provider-native background notifications: every result then carries result_index, indexes are consecutive without gap or duplicate, and each later result has origin kind=task-notification without subkind; another or unknown origin, any subkind, a missing or mixed counter, a second result without counters, or a later failed, refused, interrupted, or truncated result rejects; for every result, terminal_reason when present must be completed, documented max_turns, api_error, aborted_streaming, and aborted_tools reasons reject as failures, and any other value is malformed; queued_turn_count when present must be 0 and a positive count rejects as queued human input; an explicit null result_index, origin, terminal_reason, or queued_turn_count is malformed; an empty or missing follow-up answer settles only as a coalesced batch acknowledgement with num_turns=0, which stays pending until a later non-empty attributed result, and any other empty follow-up or a pending acknowledgement at EOF rejects; a follow-up cycle may repeat system/init only before its root messages and only with the same session, version, cwd, permission mode, apiKeySource, model, tools, and MCP server count; root assistant or user events after a result, those whose parent_tool_use_id is absent or null, open a follow-up cycle that must end with an attributed result before EOF; nested agent messages, those whose parent_tool_use_id is a nonblank string of at most 1024 bytes without NUL or invalid UTF-8, stay structurally validated and session-bound but never open, advance, or settle a cycle or batch, are never selected as an answer, and never affect accounting; any other parent_tool_use_id value, including an empty, blank, oversized, mistyped, or case-aliased field, is malformed before or after the principal result; only the principal result is selected as the answer, follow-up answers are never selected, and principal failure, refusal, empty answer, or identity conflict rejects regardless of later follow-ups; main-agent usage, num_turns, and duration come from the principal result, while cumulative whole-tree total_cost_usd comes from the last attributed result and is never summed"`

// attributedNativeContractWithPolicy derives an attributed revision from the
// frozen pre-attribution bytes and the given completion policy text.
func attributedNativeContractWithPolicy(mode, policy string) string {
	contract := preAttributionNativeContractForMode(mode)
	if contract == "" {
		return ""
	}
	if mode == Mode {
		contract = strings.Replace(contract, `"version":"native-permissions-v3"`, `"version":"native-permissions-v4"`, 1)
		contract = strings.Replace(contract, "exactly one result object is required after init;", "one caller-attributed principal result is required after init;", 1)
		contract = strings.Replace(contract, "substantive assistant/user continuation after result, conflicting results, and identity conflicts reject", "unattributed assistant/user continuation after result, unattributed or conflicting results, and identity conflicts reject", 1)
		contract = strings.Replace(contract, "valid init, one successful result, and a non-whitespace result string", "valid init, one successful principal result with a non-whitespace result string, and settled attributed follow-ups", 1)
	} else {
		contract = strings.Replace(contract, `"version":"native-permissions-v4"`, `"version":"native-permissions-v5"`, 1)
		contract = strings.Replace(contract, "valid write-mode init, one successful non-whitespace result", "valid write-mode init, one successful non-whitespace principal result, and settled attributed follow-ups", 1)
	}
	return strings.Replace(contract, `,"success":`, `,`+policy+`,"success":`, 1)
}

// scopedNativeContract returns the frozen bytes of the scoped revision for
// mode: read-only native-permissions-v4 (digest
// b3c26a3eae4e39d6a68f55e191b7ac2aa7f05476103ee2d111de0fd873880f0d) and
// workspace-write native-permissions-v5 (digest
// 27bf2e76be4185559ecaa8cfbd404d81744281298b6a29b6bf99ab4e1f4b1b29). A later
// system/init is governed by completion_attribution, so the read-only init
// clause states only the initial one.
func scopedNativeContract(mode string) string {
	contract := attributedNativeContractWithPolicy(mode, scopedCompletionAttributionContract)
	if mode == Mode {
		contract = strings.Replace(contract, "exactly one system/init object is required;", "one initial system/init object is required, and any later system/init is governed only by completion_attribution;", 1)
	}
	return contract
}

// contractEdit replaces one exact clause of a frozen contract.
type contractEdit struct{ old, new string }

// applyContractEdits replaces each clause once, in order. The pinned digests
// of the derived revisions detect any clause that no longer matches.
func applyContractEdits(contract string, edits []contractEdit) string {
	for _, edit := range edits {
		contract = strings.Replace(contract, edit.old, edit.new, 1)
	}
	return contract
}

// advisoryCompletionEdits rewrite the scoped completion_attribution text into
// the advisory policy: an unknown completion reason is unsupported, and a
// repeated system/init is compared only on its authority fields.
func advisoryCompletionEdits() []contractEdit {
	return []contractEdit{
		{
			"with one identical repeated system/init permitted once after an acknowledgement",
			"with one repeated system/init of the same session and permission mode permitted once after an acknowledgement",
		},
		{
			"documented max_turns, api_error, aborted_streaming, and aborted_tools reasons reject as failures, and any other value is malformed",
			"documented max_turns, api_error, aborted_streaming, and aborted_tools reasons reject as failures, and any other string of at most 128 bytes is an unsupported completion that rejects as unsupported-completion with diagnostic unsupported_completion naming the field, never as success, provider failure, or malformed output, while a null, mistyped, or oversized value is malformed",
		},
		{
			"only with the same session, version, cwd, permission mode, apiKeySource, model, tools, and MCP server count",
			"only with the same session and permission mode; advisory init metadata may change",
		},
	}
}

// advisoryReadOnlyEdits rewrite the scoped read-only line, init, message, and
// stop_reason clauses into the advisory wire projection.
func advisoryReadOnlyEdits() []contractEdit {
	return []contractEdit{
		{`"version":"native-permissions-v4"`, `"version":"native-permissions-v5"`},
		{
			"duplicate keys, including case-folded aliases, reject at every nesting level",
			"every line is complete JSON; duplicate keys, including case-folded aliases, reject among the selected authority and control fields of an object, while opaque tool, content, and extension payloads are validated as JSON syntax only",
		},
		{
			"its session_id is a canonical UUID, claude_code_version is a non-empty valid CLI-reported version string, permissionMode is plan, and provider-owned apiKeySource, tools (at most 4096 names), and mcp_servers fields are accepted when structurally valid",
			"its session_id is a canonical UUID and permissionMode is plan, and these are its only authority; claude_code_version, apiKeySource, cwd, model, tools, mcp_servers, and other init metadata are advisory: a missing, malformed, or ambiguous advisory field is ignored and never defaulted, and only a valid model name is retained for usage",
		},
		{
			"message content blocks are structurally validated;",
			"known message content blocks are structurally validated and unknown content block types are opaque valid JSON;",
		},
		{"refusal, unknown stop_reason, unattributed", "refusal, unattributed"},
		{
			"null, end_turn, and stop_sequence stop reasons may complete;",
			"null, end_turn, and stop_sequence stop reasons may complete, and any other string stop_reason of at most 128 bytes is an unsupported completion that rejects as unsupported-completion;",
		},
	}
}

// advisoryWorkspaceWriteEdits rewrite the scoped workspace-write version and
// init clause; its other rules refer to the read-only interpreter.
func advisoryWorkspaceWriteEdits() []contractEdit {
	return []contractEdit{
		{`"version":"native-permissions-v5"`, `"version":"native-permissions-v6"`},
		{
			"permissionMode=bypassPermissions; provider-owned apiKeySource, tools (at most 4096 names), and mcp_servers fields are accepted when structurally valid",
			"session_id and permissionMode=bypassPermissions are the only authority; other init metadata is advisory as in the native read-only interpreter",
		},
	}
}

// advisoryNativeContract is the current contract for mode: the frozen scoped
// bytes with the advisory wire clauses.
func advisoryNativeContract(mode string) string {
	var edits []contractEdit
	switch mode {
	case Mode:
		edits = advisoryReadOnlyEdits()
	case WorkspaceWriteMode:
		edits = advisoryWorkspaceWriteEdits()
	default:
		return ""
	}
	return applyContractEdits(scopedNativeContract(mode), append(edits, advisoryCompletionEdits()...))
}

// unscopedNativeContract returns the exact contract bytes of the retained
// unscoped revision for mode: the attributed bytes sealed before message scope
// existed, with the original completion text and without the scoped read-only
// init clause. Read-only is native-permissions-v4 (digest
// f7c1053edce279071b9caa46f727cf196d5998fc05d07afdf9c5378acd9eb25c) and
// workspace-write is native-permissions-v5 (digest
// 333a689a864dfb03ecd4a6eace97bcf6964fea96768ddbbb86a92d98c4a7a76b).
func unscopedNativeContract(mode string) string {
	return attributedNativeContractWithPolicy(mode, unscopedCompletionAttributionContract)
}

// Contract returns the current native canonical bytes whose digest identifies
// the interpreter.
func Contract() string { return advisoryNativeContract(Mode) }

// ContractDigest returns the lowercase SHA-256 digest bound into Reference.
func ContractDigest() string { return task.ComputeSHA256([]byte(Contract())) }

// Reference returns the exact predicate reference for Claude print output.
func Reference() task.PredicateRef {
	return ReferenceForMode(Mode)
}

// WorkspaceWriteContract returns the current native workspace-write contract.
func WorkspaceWriteContract() string { return advisoryNativeContract(WorkspaceWriteMode) }

// WorkspaceWriteReference returns a predicate reference bound to the native
// bypassPermissions profile. Keeping a distinct reference prevents a read-only
// result from satisfying a workspace-write task.
func WorkspaceWriteReference() task.PredicateRef {
	return ReferenceForMode(WorkspaceWriteMode)
}

// ContractForMode returns the immutable output contract for mode.
func ContractForMode(mode string) string {
	return advisoryNativeContract(mode)
}

// ScopedNativeContractForMode returns the frozen scoped native contract that
// ContractForMode exposed before the advisory wire revision. Adapters whose
// retained predicates embed it use these exact bytes for reconstruction.
func ScopedNativeContractForMode(mode string) string {
	return scopedNativeContract(mode)
}

// PreAttributionNativeContractForMode returns the frozen relaxed-probe native
// contract bound to the single-result completion policy. Adapters whose
// retained predicates embed it use these exact bytes for reconstruction.
func PreAttributionNativeContractForMode(mode string) string {
	return preAttributionNativeContractForMode(mode)
}

// PreviousNativeContractForMode returns the exact native contract exposed by
// ContractForMode before the relaxed runtime-probe revision. Adapters with a
// retained predicate that embeds Claude's contract use these frozen bytes for
// reconstruction.
func PreviousNativeContractForMode(mode string) string {
	return previousNativeProbeContractForMode(mode)
}

// HistoricalNativeContractForMode returns the pre-issue47 native contract
// bytes for adapters that reuse Claude's stream interpreter under their own
// predicate identity. Fresh tasks must use ContractForMode; this helper is
// only for replaying sealed evidence whose parser retained native payloads.
func HistoricalNativeContractForMode(mode string) string {
	switch mode {
	case Mode:
		return nativeContractV1
	case WorkspaceWriteMode:
		return nativeWorkspaceWriteContractV2
	default:
		return ""
	}
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

// scopedReference identifies the frozen scoped revision of mode. It is
// replay-only and a direct recovery source of the current revision.
func scopedReference(mode string) task.PredicateRef {
	contract := scopedNativeContract(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	version := scopedPredicateVersion
	if mode == WorkspaceWriteMode {
		version = scopedWorkspaceWritePredicateVersion
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: task.ComputeSHA256([]byte(contract))}
}

func preAttributionReferenceForMode(mode string) task.PredicateRef {
	contract := preAttributionNativeContractForMode(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	version := preAttributionPredicateVersion
	if mode == WorkspaceWriteMode {
		version = preAttributionWorkspaceWritePredicateVersion
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: task.ComputeSHA256([]byte(contract))}
}

// unscopedReference identifies the retained unscoped attributed revision of
// mode. It shares its version string with the scoped reference of the same
// mode and differs by digest. Both modes were admitted under the unscoped
// policy; the references are replay-only and never a fresh default, recovery
// source, or recovery target. An unsupported mode has no revision.
func unscopedReference(mode string) task.PredicateRef {
	contract := unscopedNativeContract(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	version := scopedPredicateVersion
	if mode == WorkspaceWriteMode {
		version = scopedWorkspaceWritePredicateVersion
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: task.ComputeSHA256([]byte(contract))}
}

// isUnscopedReference reports whether reference is the retained unscoped
// revision for a request mode. The explicit mode guard is required because the
// zero reference equals the zero reference an unsupported mode yields.
func isUnscopedReference(mode string, reference task.PredicateRef) bool {
	return (mode == Mode || mode == WorkspaceWriteMode) && reference.Equal(unscopedReference(mode))
}

func previousNativeProbeReferenceForMode(mode string) task.PredicateRef {
	contract := previousNativeProbeContractForMode(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	version := previousNativeProbePredicateVersion
	if mode == WorkspaceWriteMode {
		version = previousNativeProbeWorkspaceWriteVersion
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: task.ComputeSHA256([]byte(contract))}
}

func legacyNativeReferenceForMode(mode string) task.PredicateRef {
	switch mode {
	case Mode:
		return task.PredicateRef{Adapter: Provider, Mode: mode, Version: historicalNativePredicateVersion, SHA256: legacyNativePredicateSHA256}
	case WorkspaceWriteMode:
		return task.PredicateRef{Adapter: Provider, Mode: mode, Version: legacyNativeWorkspaceWritePredicateVersion, SHA256: task.ComputeSHA256([]byte(nativeWorkspaceWriteContractV1))}
	default:
		return task.PredicateRef{}
	}
}

// historicalNativeReferenceForMode identifies the pre-issue47 current native
// parser. It is distinct from legacyNativeReferenceForMode, whose policy was
// dontAsk/acceptEdits and whose read-only digest predates native policy.
func historicalNativeReferenceForMode(mode string) task.PredicateRef {
	switch mode {
	case Mode:
		return task.PredicateRef{Adapter: Provider, Mode: mode, Version: historicalNativePredicateVersion, SHA256: task.ComputeSHA256([]byte(nativeContractV1))}
	case WorkspaceWriteMode:
		return task.PredicateRef{Adapter: Provider, Mode: mode, Version: historicalNativeWorkspaceWritePredicateVersion, SHA256: task.ComputeSHA256([]byte(nativeWorkspaceWriteContractV2))}
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
