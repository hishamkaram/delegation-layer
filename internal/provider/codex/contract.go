// Package codex interprets sealed evidence emitted by the Codex exec JSONL
// interface. It does not start a provider or own capture and publication.
package codex

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	Provider                      = "codex:exec"
	ReviewProvider                = "codex:review"
	Mode                          = config.ModeReadOnly
	WorkspaceWriteMode            = config.ModeWorkspaceWrite
	OutputName                    = "codex-last-message.txt"
	ProfileRevision               = "codex-read-only-v1"
	WorkspaceWriteProfileRevision = "codex-workspace-write-v1"
	OutputWriterContract          = task.OutputWriterProcessExitEOF

	// predicateVersion identifies this interpreter's output contract. It is
	// independent of the installed Codex CLI release.
	predicateVersion             = "runtime-reported-v4"
	historicalV3PredicateVersion = "runtime-reported-v3"
	// historicalCurrentPredicateVersion identifies the v0.1.22 current-runtime
	// contract. It remains registered for already sealed tasks and recovery.
	historicalCurrentPredicateVersion = "runtime-reported-v2"
	// historicalPredicateVersion identifies the older current-runtime contract.
	// It remains registered for already sealed tasks; it is not used for new
	// preparation.
	historicalPredicateVersion = "runtime-reported"

	// legacyPredicateVersion and legacyPredicateSHA256 keep already-sealed
	// read-only evidence collectible after the runtime-capability migration.
	// They identify the historical predicate bytes only; they are never used to
	// admit or reject a provider executable.
	legacyPredicateVersion = "0.154.0"
	legacyPredicateSHA256  = "7d40d9c4627e58d8906e816d3fc6e1eed7f0d6b14ab5fc1f18b4c704415d3a8d"

	maxEventLineBytes = task.MaxControlRecordSize
	readBufferBytes   = 32 * 1024
	answerChunkBytes  = 32 * 1024

	// Item lifecycle state is bounded independently of the per-line bound. A
	// native turn normally has only a small number of items; exceeding this
	// ceiling is an ambiguous stream and is rejected after capture drains.
	maxTrackedItems  = 1024
	maxItemIDBytes   = 256
	maxItemTypeBytes = 128

	maxInteractionQuestions    = 128
	maxInteractionQuestionID   = 256
	maxInteractionQuestionText = 64 * 1024
	maxInteractionOptions      = 128
	maxInteractionOptionLabel  = 8 * 1024
	maxInteractionNativeID     = 256
)

// contractV1 is the previous current-runtime evidence policy. Its bytes,
// including the final newline, remain immutable for replay of already sealed
// tasks.
const contractV1 = `{"adapter":"codex:exec","mode":"read-only","version":"runtime-reported","native":"Codex exec JSONL producer","events":{"line":"one UTF-8 JSON object per line, each line at most 1048576 bytes; final line may omit newline","duplicates":"duplicate keys, including case-folded aliases, reject at every nesting level","thread":"exactly one thread.started with a UUID-shaped thread_id","turn":"one turn.started followed by one successful turn.completed; turn.failed, interruption, duplicate completion, and missing terminal reject","items":"only the last top-level item.completed agent_message.text in the turn is an answer candidate; reasoning, plan, command, tool, and nested collaboration text are ignored; completion updates a started item with the same id; at most 1024 item identities are retained, each id is at most 256 bytes, each type is at most 128 bytes, and only the current candidate text is retained","unknown":"unknown telemetry and item event types are tolerated; unknown thread./turn. lifecycle names reject as terminally ambiguous","errors":"earlier error events are diagnostics and may be followed by a successful completion"},"success":"exit_code=0, sealed error empty, exact expected and recorded thread identity, successful turn.completed, final agent message non-whitespace, and exact optional output-file bytes when present","output":"codex-last-message.txt is optional evidence; ErrEvidenceAbsent falls back to stdout; present empty or conflicting bytes reject; no newline is synthesized","failure":"malformed or oversized JSONL, invalid UTF-8, conflicting identities, terminal failure/interruption, missing final, nonzero exit, seal error, or an exceeded item-state bound rejects","usage":"turn.completed usage copies provider thread totals; scope conversation-cumulative/provider-event; cached input maps to cache_read_tokens and cache writes to cache_creation_tokens; reasoning maps to thinking_tokens; missing counters remain null and total_tokens is never synthesized"}` + "\n"

const workspaceWriteContractV1 = `{"adapter":"codex:exec","mode":"workspace-write","version":"runtime-reported","native":"Codex exec JSONL producer","sandbox":"workspace-write","events":"the same strict JSONL thread, turn, item, identity, usage, and terminal-result rules as the read-only interpreter","output":"codex-last-message.txt is optional evidence; present empty or conflicting bytes reject; no newline is synthesized","failure":"malformed or oversized JSONL, conflicting identities, terminal failure/interruption, missing final, nonzero exit, seal error, or exceeded item-state bound rejects"}` + "\n"

// contractV2 is the current native evidence policy. Keep contractV1 and its
// reference available for sealed tasks created before the providerjson
// selected-field parser was introduced.
const contractV2 = `{"adapter":"codex:exec","mode":"read-only","version":"runtime-reported-v2","native":"Codex exec JSONL producer","events":{"line":"one UTF-8 JSON object per line; final line may omit newline; no provider record or answer size cap","duplicates":"duplicate keys, including case-folded aliases, reject for selected authority, identity, lifecycle, failure, usage, and answer fields; unrelated opaque fields are structurally validated without retention","thread":"exactly one thread.started with a UUID-shaped thread_id","turn":"one turn.started followed by one successful turn.completed; turn.failed, interruption, duplicate completion, and missing terminal reject","items":"only relevant agent_message identities and the last top-level item.completed agent_message text are retained; reasoning, plan, command, tool, web_search, and nested collaboration payloads are ignored; at most 1024 agent-message identities are retained","unknown":"unknown telemetry and item types are tolerated; unknown thread./turn. lifecycle names reject as terminally ambiguous","errors":"earlier error events are diagnostics and may be followed by a successful completion"},"success":"exit_code=0, sealed error empty, exact expected and recorded thread identity, successful turn.completed, final agent message non-whitespace, and exact optional output-file bytes when present","output":"codex-last-message.txt is optional evidence; ErrEvidenceAbsent falls back to stdout; final text is compared by streamed length and SHA-256; no newline is synthesized","failure":"malformed JSONL, invalid UTF-8, conflicting identities, terminal failure/interruption, missing final, nonzero exit, seal error, or an exceeded relevant item-state bound rejects","usage":"turn.completed usage copies provider thread totals when reliable; malformed, duplicate, null, or unknown usage is retained as unreliable nullable metadata; no total is synthesized"}` + "\n"

const workspaceWriteContractV2 = `{"adapter":"codex:exec","mode":"workspace-write","version":"runtime-reported-v2","native":"Codex exec JSONL producer","sandbox":"workspace-write","events":"the same selected-field JSONL thread, turn, item, identity, usage, and terminal-result rules as the read-only interpreter; no provider record or answer size cap","output":"codex-last-message.txt is optional evidence; present empty or conflicting bytes reject; final text is compared by streamed length and SHA-256; no newline is synthesized","failure":"malformed JSONL, conflicting identities, terminal failure/interruption, missing final, nonzero exit, seal error, or exceeded relevant item-state bound rejects"}` + "\n"

// contractV3 is the corrected native evidence policy. It preserves the v2
// contract above for replay while accepting Codex streams that complete an
// agent_message without emitting a preceding item.started event.
const contractV3 = `{"adapter":"codex:exec","mode":"read-only","version":"runtime-reported-v3","native":"Codex exec JSONL producer","events":{"line":"one UTF-8 JSON object per line; final line may omit newline; no provider record or answer size cap","duplicates":"duplicate keys, including case-folded aliases, reject for selected authority, identity, lifecycle, failure, usage, and answer fields; unrelated opaque fields are structurally validated without retention","thread":"exactly one thread.started with a UUID-shaped thread_id","turn":"one turn.started followed by one successful turn.completed; turn.failed, interruption, duplicate completion, and missing terminal reject","items":"only relevant agent_message identities and the last top-level item.completed agent_message text are retained; an agent_message may complete without item.started, item.updated still requires item.started, duplicate completion rejects, and reasoning, plan, command, tool, web_search, and nested collaboration payloads are ignored; at most 1024 agent-message identities are retained","unknown":"unknown telemetry and item types are tolerated; unknown thread./turn. lifecycle names reject as terminally ambiguous","errors":"earlier error events are diagnostics and may be followed by a successful completion"},"success":"exit_code=0, sealed error empty, exact expected and recorded thread identity, successful turn.completed, final agent message non-whitespace, and exact optional output-file bytes when present","output":"codex-last-message.txt is optional evidence; ErrEvidenceAbsent falls back to stdout; final text is compared by streamed length and SHA-256; no newline is synthesized","failure":"malformed JSONL, invalid UTF-8, conflicting identities, terminal failure/interruption, missing final, nonzero exit, seal error, or an exceeded relevant item-state bound rejects","usage":"turn.completed usage copies provider thread totals when reliable; malformed, duplicate, null, or unknown usage is retained as unreliable nullable metadata; no total is synthesized"}` + "\n"

const workspaceWriteContractV3 = `{"adapter":"codex:exec","mode":"workspace-write","version":"runtime-reported-v3","native":"Codex exec JSONL producer","sandbox":"workspace-write","events":"the same completion-tolerant selected-field JSONL thread, turn, item, identity, usage, and terminal-result rules as the read-only interpreter; no provider record or answer size cap","output":"codex-last-message.txt is optional evidence; present empty or conflicting bytes reject; final text is compared by streamed length and SHA-256; no newline is synthesized","failure":"malformed JSONL, conflicting identities, terminal failure/interruption, missing final, nonzero exit, seal error, or exceeded relevant item-state bound rejects"}` + "\n"

// contractV4 adds non-Git launch support and a sealed-fact startup diagnostic.
// Keep all historical contract bytes immutable.
const contractV4 = `{"adapter":"codex:exec","mode":"read-only","version":"runtime-reported-v4","native":"Codex exec JSONL producer","events":{"line":"one UTF-8 JSON object per line; final line may omit newline; no provider record or answer size cap","duplicates":"duplicate keys, including case-folded aliases, reject for selected authority, identity, lifecycle, failure, usage, and answer fields; unrelated opaque fields are structurally validated without retention","thread":"exactly one thread.started with a UUID-shaped thread_id","turn":"one turn.started followed by one successful turn.completed; turn.failed, interruption, duplicate completion, and missing terminal reject","items":"only relevant agent_message identities and the last top-level item.completed agent_message text are retained; an agent_message may complete without item.started, item.updated still requires item.started, duplicate completion rejects, and reasoning, plan, command, tool, web_search, and nested collaboration payloads are ignored; at most 1024 agent-message identities are retained","unknown":"unknown telemetry and item types are tolerated; unknown thread./turn. lifecycle names reject as terminally ambiguous","errors":"earlier error events are diagnostics and may be followed by a successful completion"},"success":"exit_code=0, sealed error empty, exact expected and recorded thread identity, successful turn.completed, final agent message non-whitespace, and exact optional output-file bytes when present","output":"codex-last-message.txt is optional evidence; ErrEvidenceAbsent falls back to stdout; final text is compared by streamed length and SHA-256; no newline is synthesized","failure":"malformed JSONL, invalid UTF-8, conflicting identities, terminal failure/interruption, missing final, nonzero exit, seal error, or an exceeded relevant item-state bound rejects","usage":"turn.completed usage copies provider thread totals when reliable; malformed, duplicate, null, or unknown usage is retained as unreliable nullable metadata; no total is synthesized","launch":"ordinary exec and exact-session resume include --skip-git-repo-check independently of the selected native sandbox","diagnostics":"a started process with positive exit status, empty sealed runner error, and zero sealed stdout bytes reports provider_startup_failed without interpreting stderr"}` + "\n"

const workspaceWriteContractV4 = `{"adapter":"codex:exec","mode":"workspace-write","version":"runtime-reported-v4","native":"Codex exec JSONL producer","sandbox":"workspace-write","events":"the same completion-tolerant selected-field JSONL thread, turn, item, identity, usage, and terminal-result rules as the read-only interpreter; no provider record or answer size cap","output":"codex-last-message.txt is optional evidence; present empty or conflicting bytes reject; final text is compared by streamed length and SHA-256; no newline is synthesized","failure":"malformed JSONL, conflicting identities, terminal failure/interruption, missing final, nonzero exit, seal error, or exceeded relevant item-state bound rejects","launch":"ordinary exec and exact-session resume include --skip-git-repo-check independently of the selected native sandbox","diagnostics":"a started process with positive exit status, empty sealed runner error, and zero sealed stdout bytes reports provider_startup_failed without interpreting stderr"}` + "\n"

// Contract returns the canonical bytes whose digest identifies this
// interpreter.
func Contract() string { return contractV4 }

// ContractDigest returns the lowercase SHA-256 digest bound into Reference.
func ContractDigest() string { return task.ComputeSHA256([]byte(contractV4)) }

// Reference returns the exact predicate reference for Codex exec JSONL.
func Reference() task.PredicateRef {
	return task.PredicateRef{
		Adapter: Provider,
		Mode:    Mode,
		Version: predicateVersion,
		SHA256:  ContractDigest(),
	}
}

// WorkspaceWriteReference returns the output predicate for Codex's native
// workspace-write sandbox. It intentionally has a separate reference so a
// read-only result can never satisfy a write-mode task (or vice versa).
func WorkspaceWriteReference() task.PredicateRef {
	return ReferenceForMode(WorkspaceWriteMode)
}

// ContractForMode returns the immutable output contract for mode.
func ContractForMode(mode string) string {
	if mode == Mode {
		return contractV4
	}
	if mode == WorkspaceWriteMode {
		return workspaceWriteContractV4
	}
	return ""
}

// ReferenceForMode returns the predicate reference bound to mode.
func ReferenceForMode(mode string) task.PredicateRef {
	contract := ContractForMode(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: predicateVersion, SHA256: task.ComputeSHA256([]byte(contract))}
}

const reviewContract = `{"adapter":"codex:review","mode":"read-only","version":"review-runtime-v1","native":"Codex exec review JSONL producer","events":"the same completion-tolerant selected-field JSONL thread, turn, item, identity, usage, and terminal-result rules as Codex exec runtime-reported-v3","output":"the native review target supplies the review scope; the final answer is selected from stdout and no output-file artifact is declared","success":"exit_code=0, sealed error empty, exact expected and recorded thread identity, successful turn.completed, and final agent message non-whitespace","failure":"malformed JSONL, conflicting identities, terminal failure/interruption, missing final, nonzero exit, seal error, or an exceeded relevant item-state bound rejects"}` + "\n"

func reviewReference() task.PredicateRef {
	return task.PredicateRef{Adapter: ReviewProvider, Mode: Mode, Version: "review-runtime-v1", SHA256: task.ComputeSHA256([]byte(reviewContract))}
}

func historicalV3ReferenceForMode(mode string) task.PredicateRef {
	var contract string
	switch mode {
	case Mode:
		contract = contractV3
	case WorkspaceWriteMode:
		contract = workspaceWriteContractV3
	default:
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: historicalV3PredicateVersion, SHA256: task.ComputeSHA256([]byte(contract))}
}

func historicalCurrentRuntimeContractForMode(mode string) string {
	if mode == Mode {
		return contractV2
	}
	if mode == WorkspaceWriteMode {
		return workspaceWriteContractV2
	}
	return ""
}

func historicalCurrentRuntimeReferenceForMode(mode string) task.PredicateRef {
	contract := historicalCurrentRuntimeContractForMode(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: historicalCurrentPredicateVersion, SHA256: task.ComputeSHA256([]byte(contract))}
}

func historicalRuntimeContractForMode(mode string) string {
	if mode == Mode {
		return contractV1
	}
	if mode == WorkspaceWriteMode {
		return workspaceWriteContractV1
	}
	return ""
}

func historicalRuntimeReferenceForMode(mode string) task.PredicateRef {
	contract := historicalRuntimeContractForMode(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: historicalPredicateVersion, SHA256: task.ComputeSHA256([]byte(contract))}
}

// LegacyReference returns the historical read-only predicate reference. It is
// retained solely so sealed tasks created before runtime capability checks can
// still be collected and interpreted.
func LegacyReference() task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: Mode, Version: legacyPredicateVersion, SHA256: legacyPredicateSHA256}
}

// WorkspaceWriteContract returns the canonical workspace-write output
// contract bytes for callers that need to inspect the registered predicate.
func WorkspaceWriteContract() string { return workspaceWriteContractV4 }

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
