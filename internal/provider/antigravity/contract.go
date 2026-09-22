package antigravity

import (
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	Provider           = config.ProviderAntigravityPrint
	ModeReadOnly       = config.ModeReadOnly
	ModeWorkspaceWrite = config.ModeWorkspaceWrite
	Mode               = ModeWorkspaceWrite
	Version            = "1.2.2"

	StatusSuccess = "SUCCESS"
	StatusError   = "ERROR"
	TimeoutMarker = "[agy] print timeout"
)

// printV122Contract is immutable interpretation policy for the agy
// author-envelope shape. Its revision is independent of the installed CLI;
// a changed field, status, or refusal rule gets a new predicate digest rather
// than silently changing recovery semantics.
const printV122Contract = `{"adapter":"antigravity:print","mode":"workspace-write","version":"1.2.2","top_level":{"conversation_id":"uuid-string","status":"SUCCESS|ERROR","response":"string","error":"string?","duration_seconds":"nonnegative-finite-number?","num_turns":"nonnegative-integer?","usage":{"input_tokens":"nonnegative-integer?","output_tokens":"nonnegative-integer?","thinking_tokens":"nonnegative-integer?","cache_read_tokens":"nonnegative-integer?","total_tokens":"nonnegative-integer?"}},"object":"one strict object; duplicate and unknown keys rejected; JSON whitespace is allowed between tokens but not inside strings or escapes","success":"status=SUCCESS,error absent,exit_code=0,conversation_id valid,response non-whitespace,no timeout marker","failure":"known ERROR status becomes provider-failed; sealed capture errors become provider-failed","identity":"UUID-shaped conversation_id; exact expected and recorded IDs","timeout":"exact case-sensitive stderr marker; incomplete-output","usage":"conversation-cumulative/provider-envelope; reported on success, unreliable on failure/incomplete","invalid":"malformed, truncated, invalid UTF-8, wrong types, unknown status, duplicate/trailing data","response":"decoded UTF-8 bytes streamed without adapter length cap"}` + "\n"

// Contract returns the canonical bytes whose digest identifies this
// interpreter's behavior.
func Contract() string { return printV122Contract }

// ContractDigest returns the lowercase SHA-256 digest bound into Reference.
func ContractDigest() string { return task.ComputeSHA256([]byte(printV122Contract)) }

func knownStatus(status string) bool {
	switch status {
	case StatusSuccess, StatusError:
		return true
	default:
		return false
	}
}

// currentPrintContract references the complete legacy rules and changes only
// refusal precedence. Legacy sealed evidence keeps its original interpreter.
const currentPrintContract = `{"version":"1.2.2-auth2","base_sha256":"9aca4a6a639c18b8c4515445d8f004f2788c0f627264695cbb66357b5285d366","change":"after structural and known-status validation, ERROR is provider-failed before conversation identity validation; no session is published; SUCCESS identity requirements unchanged"}` + "\n"
