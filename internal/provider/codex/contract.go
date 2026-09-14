// Package codex interprets sealed evidence emitted by the Codex exec JSONL
// interface. It does not start a provider or own capture and publication.
package codex

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	Provider   = "codex:exec"
	Mode       = "read-only"
	OutputName = "codex-last-message.txt"

	// predicateVersion is tied to the pinned Codex 0.154.0 event producer. A
	// changed interpretation rule gets a new predicate reference and keeps this
	// implementation available for historical sealed evidence.
	predicateVersion = "0.154.0"

	maxEventLineBytes = task.MaxControlRecordSize
	readBufferBytes   = 32 * 1024
	answerChunkBytes  = 32 * 1024

	// Item lifecycle state is bounded independently of the per-line bound. A
	// native turn normally has only a small number of items; exceeding this
	// ceiling is an ambiguous stream and is rejected after capture drains.
	maxTrackedItems  = 1024
	maxItemIDBytes   = 256
	maxItemTypeBytes = 128
)

// contractV1 is immutable evidence policy. Its bytes, including the final
// newline, are the predicate digest. The native producer is Codex 0.154.0;
// the versioned predicate records the JSONL and output-file rules that make a
// sealed result publishable.
const contractV1 = `{"adapter":"codex:exec","mode":"read-only","version":"0.154.0","native":"Codex exec JSONL producer 0.154.0","events":{"line":"one UTF-8 JSON object per line, each line at most 1048576 bytes; final line may omit newline","duplicates":"duplicate keys, including case-folded aliases, reject at every nesting level","thread":"exactly one thread.started with a UUID-shaped thread_id","turn":"one turn.started followed by one successful turn.completed; turn.failed, interruption, duplicate completion, and missing terminal reject","items":"only the last top-level item.completed agent_message.text in the turn is an answer candidate; reasoning, plan, command, tool, and nested collaboration text are ignored; completion updates a started item with the same id; at most 1024 item identities are retained, each id is at most 256 bytes, each type is at most 128 bytes, and only the current candidate text is retained","unknown":"unknown telemetry and item event types are tolerated; unknown thread./turn. lifecycle names reject as terminally ambiguous","errors":"earlier error events are diagnostics and may be followed by a successful completion"},"success":"exit_code=0, sealed error empty, exact expected and recorded thread identity, successful turn.completed, final agent message non-whitespace, and exact optional output-file bytes when present","output":"codex-last-message.txt is optional evidence; ErrEvidenceAbsent falls back to stdout; present empty or conflicting bytes reject; no newline is synthesized","failure":"malformed or oversized JSONL, invalid UTF-8, conflicting identities, terminal failure/interruption, missing final, nonzero exit, seal error, or an exceeded item-state bound rejects","usage":"turn.completed usage copies provider thread totals; scope conversation-cumulative/provider-event; cached input maps to cache_read_tokens and cache writes to cache_creation_tokens; reasoning maps to thinking_tokens; missing counters remain null and total_tokens is never synthesized"}` + "\n"

// Contract returns the canonical bytes whose digest identifies this
// interpreter.
func Contract() string { return contractV1 }

// ContractDigest returns the lowercase SHA-256 digest bound into Reference.
func ContractDigest() string { return task.ComputeSHA256([]byte(contractV1)) }

// Reference returns the exact predicate reference for Codex exec JSONL.
func Reference() task.PredicateRef {
	return task.PredicateRef{
		Adapter: Provider,
		Mode:    Mode,
		Version: predicateVersion,
		SHA256:  ContractDigest(),
	}
}

// validThreadID accepts the canonical UUID-shaped identifiers emitted by
// Codex. Matching against expected and recorded identities remains exact.
func validThreadID(id string) bool {
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
