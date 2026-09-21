// Package pi contains the direct Pi JSON event-stream adapter.
//
// Pi is invoked as a normal child process. The adapter only constructs the
// finite command and interprets the bounded JSONL evidence captured by the
// execution core; it does not own process lifetime, capture, or publication.
package pi

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	Provider = config.ProviderPiJSON

	ModeReadOnly = config.ModeReadOnly
	Mode         = ModeReadOnly

	// ProfileRevision identifies the local effective-policy shape. It is
	// adapter metadata and is never compared with a Pi version or digest.
	ProfileRevision = "pi-direct-cli-v1"

	maxEventLineBytes = task.MaxControlRecordSize
	readBufferBytes   = 32 * 1024
	answerChunkBytes  = 32 * 1024
	maxUsageRows      = 1024

	// predicateVersion identifies the interpreter contract, independent of the
	// installed Pi CLI release.
	predicateVersion       = "2"
	legacyPredicateVersion = "1"
	Version                = predicateVersion
)

const readOnlyTools = "read,grep,find,ls"

const contract = `{"adapter":"pi:json","mode":"read-only","version":"2","native":"Pi JSON event stream","permissions":"read-only maps the native --tools allowlist to read,grep,find,ls; provider configuration, extensions, authentication, and startup behavior remain provider-owned. Pi workspace-write is not advertised because its built-in write and edit tools accept arbitrary absolute paths without a native workspace boundary","session":"one session event with a UUID id; native session discovery and continuation use the exact recorded id","events":"one agent_start, one or more turn_start/turn_end pairs, and one agent_end in order; message events may occur between turn_start and turn_end; unknown telemetry is tolerated while unknown lifecycle events reject","result":"the final turn_end.message and final assistant message in agent_end.messages must agree; assistant stopReason=toolUse is allowed only for an intermediate tool turn and stopReason=stop is required for the final turn; text content blocks are concatenated in order","failure":"malformed or oversized JSONL, invalid UTF-8, conflicting session identity, missing lifecycle completion, unsuccessful assistant stop reasons (length, error, or aborted), provider error, nonzero exit, empty final text, or more than 1024 retained usage rows reject","tools":"read,grep,find,ls"}` + "\n"

// legacyContractV1 retains the exact predicate bytes used before native
// provider configuration and startup behavior became part of the provider's
// own contract.
const legacyContractV1 = `{"adapter":"pi:json","mode":"read-only","version":"1","native":"Pi JSON event stream","permissions":"--no-extensions and --offline prevent extension execution and startup package/network work; the native tool allowlist is read,grep,find,ls. Read-only admission rejects project .pi/commands migration state and a native agent directory overlapping the workspace. Pi workspace-write is not advertised because its built-in write and edit tools accept arbitrary absolute paths without a native workspace boundary","session":"one session event with a UUID id; --session-dir binds storage to the resolved native directory and continuation uses the exact recorded id","events":"one agent_start, one or more turn_start/turn_end pairs, and one agent_end in order; message events may occur between turn_start and turn_end; unknown telemetry is tolerated while unknown lifecycle events reject","result":"the final turn_end.message and final assistant message in agent_end.messages must agree; assistant stopReason=toolUse is allowed only for an intermediate tool turn and stopReason=stop is required for the final turn; text content blocks are concatenated in order","failure":"malformed or oversized JSONL, invalid UTF-8, conflicting session identity, missing lifecycle completion, unsuccessful assistant stop reasons (length, error, or aborted), provider error, nonzero exit, empty final text, startup migration that could mutate the workspace, or more than 1024 retained usage rows reject","tools":"read,grep,find,ls"}` + "\n"

// Contract returns the immutable evidence policy for mode. An empty or
// unsupported mode returns an empty string.
func Contract(mode ...string) string {
	selected := ModeReadOnly
	if len(mode) > 0 && mode[0] != "" {
		selected = mode[0]
	}
	if selected != ModeReadOnly {
		return ""
	}
	return contract
}

// ContractDigest returns the digest bound into the mode-specific predicate.
func ContractDigest(mode ...string) string {
	contract := Contract(mode...)
	if contract == "" {
		return ""
	}
	return task.ComputeSHA256([]byte(contract))
}

// ReferenceForMode returns the exact predicate reference for one Pi mode.
func ReferenceForMode(mode string) task.PredicateRef {
	if mode != ModeReadOnly {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: predicateVersion, SHA256: ContractDigest(mode)}
}

// Reference returns the read-only predicate for compatibility with provider
// packages that expose one default reference.
func Reference() task.PredicateRef { return ReferenceForMode(ModeReadOnly) }

// LegacyReference returns the historical read-only predicate reference so
// sealed evidence produced before the native-profile revision remains
// collectible.
func LegacyReference() task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: ModeReadOnly, Version: legacyPredicateVersion, SHA256: task.ComputeSHA256([]byte(legacyContractV1))}
}

func validMode(mode string) bool {
	return mode == ModeReadOnly
}

// validSessionID accepts the UUID-shaped IDs Pi emits in its session header.
// Identity matching remains exact after this shape check.
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
