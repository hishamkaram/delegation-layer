// Package claude contains the bounded Claude print adapter.
package claude

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	Provider = "claude:print"
	Mode     = "read-only"
	Version  = "2.1.270"

	maxEventLineBytes = task.MaxControlRecordSize
	answerChunkBytes  = 32 * 1024
	maxModelRecords   = 128
	maxModelNameBytes = 256
)

// contractV1 is the immutable evidence policy for the inspected Claude
// stream-json producer. Its bytes, including the final newline, are the
// predicate digest. A changed interpretation rule requires a new version.
const contractV1 = `{"adapter":"claude:print","mode":"read-only","version":"2.1.270","native":"Claude Code print stream-json producer 2.1.270","events":{"line":"one UTF-8 JSON object per line, each line at most 1048576 bytes; the final line may omit its newline; duplicate keys, including case-folded aliases, reject at every nesting level","init":"exactly one system/init object is required; its session_id is a canonical UUID, claude_code_version is 2.1.270, apiKeySource is none, permissionMode is dontAsk, tools contains Read, Glob, and Grep and no other tool except EndConversation, and mcp_servers is an empty array","messages":"assistant and user events occur only after init; message content blocks are structurally validated; assistant tool_use names are restricted to Read, Glob, Grep, and EndConversation; unsupported tool_use, invalid content, and pre-init messages reject; assistant text is ignored for answer selection","result":"exactly one result object is required after init; subtype=success, is_error=false, and a non-whitespace string result are required; any errors entry, interruption, max_tokens truncation, refusal, unknown stop_reason, substantive assistant/user continuation after result, conflicting results, and identity conflicts reject; null, end_turn, and stop_sequence stop reasons may complete; permission_denials, when present, are an array of non-null objects; benign trailing system events are allowed","selection":"only the top-level result.result string is an answer; assistant text and nested or subagent messages are never selected","unknown":"unknown telemetry is tolerated; unknown result subtypes and unknown thread/session/turn lifecycle events are terminally ambiguous and reject","bounds":"the parser retains one bounded line and bounded scalar fields only; modelUsage has at most 128 model records"},"success":"invocation started, exit_code=0, sealed error empty, exact expected and recorded session identity, valid init, one successful result, and a non-whitespace result string","failure":"malformed or oversized JSONL, invalid UTF-8, invalid init policy, missing result, result error subtype, provider refusal or limit, nonzero exit, start failure, seal error, and operational reader or writer faults never publish","usage":"result.usage is main-agent provider-envelope accounting; result.modelUsage is whole-tree provider-envelope accounting; total_cost_usd is a separate whole-tree provider estimate; missing counters remain null and totals are never synthesized or differenced; rejected-result accounting is unreliable","identity":"every event session_id is bound before init and must remain exact; fresh sessions use UUIDv5 with the decoded root ID as namespace and canonical task ID as the ASCII name; continuation uses the exact recorded session UUID"}` + "\n"

// Contract returns the canonical bytes whose digest identifies this
// interpreter.
func Contract() string { return contractV1 }

// ContractDigest returns the lowercase SHA-256 digest bound into Reference.
func ContractDigest() string { return task.ComputeSHA256([]byte(contractV1)) }

// Reference returns the exact predicate reference for Claude print output.
func Reference() task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: Mode, Version: Version, SHA256: ContractDigest()}
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
