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

	ModeReadOnly       = config.ModeReadOnly
	ModeWorkspaceWrite = config.ModeWorkspaceWrite
	Mode               = ModeReadOnly

	// ProfileRevision identifies the local effective-policy shape. It is
	// adapter metadata and is never compared with a Pi version or digest.
	ProfileRevision = "pi-direct-cli-v1"

	maxEventLineBytes = task.MaxControlRecordSize
	readBufferBytes   = 32 * 1024
	answerChunkBytes  = 32 * 1024
	maxUsageRows      = 1024

	// predicateVersion identifies the current interpreter contract, independent
	// of the installed Pi CLI release.
	// v4 is retained for the pre-issue47 current parser. Fresh tasks use v5,
	// which keeps offsets and text digests instead of retaining native blobs.
	predicateVersion        = "4"
	currentPredicateVersion = "5"
	historicalPredicateV3   = "3"
	historicalPredicateV2   = "2"
	legacyPredicateVersion  = "1"
	Version                 = currentPredicateVersion
)

const readOnlyTools = "read,grep,find,ls"

// readOnlyContractV2 retains the admitted read-only v2 bytes. Historical
// interpreters use it when collecting already-admitted v2 evidence.
const readOnlyContractV2 = `{"adapter":"pi:json","mode":"read-only","version":"2","native":"Pi JSON event stream","permissions":"read-only maps the native --tools allowlist to read,grep,find,ls; provider configuration, extensions, authentication, and startup behavior remain provider-owned. Pi workspace-write is not advertised because its built-in write and edit tools accept arbitrary absolute paths without a native workspace boundary","session":"one session event with a UUID id; native session discovery and continuation use the exact recorded id","events":"one agent_start, one or more turn_start/turn_end pairs, and one agent_end in order; message events may occur between turn_start and turn_end; unknown telemetry is tolerated while unknown lifecycle events reject","result":"the final turn_end.message and final assistant message in agent_end.messages must agree; assistant stopReason=toolUse is allowed only for an intermediate tool turn and stopReason=stop is required for the final turn; text content blocks are concatenated in order","failure":"malformed or oversized JSONL, invalid UTF-8, conflicting session identity, missing lifecycle completion, unsuccessful assistant stop reasons (length, error, or aborted), provider error, nonzero exit, empty final text, or more than 1024 retained usage rows reject","tools":"read,grep,find,ls"}` + "\n"

// readOnlyContractV3 retains the exact bytes admitted before the current
// interpreter changed its retry-marker semantics.
const readOnlyContractV3 = `{"adapter":"pi:json","mode":"read-only","version":"3","native":"Pi JSON event stream","permissions":"read-only maps the native --tools allowlist to read,grep,find,ls; provider configuration, extensions, authentication, and startup behavior remain provider-owned","session":"one session event with a UUID id; native session discovery and continuation use the exact recorded id","events":"one or more sequential complete native agent cycles may occur within one CLI invocation for retries, compaction, or extension followups; each cycle has agent_start, one or more turn_start/turn_end pairs, and agent_end in order; message events may occur between turn_start and turn_end; auto_retry_start requires a subsequent complete cycle; auto_retry_end is completion telemetry and does not require another cycle; retry markers do not impose provider retry policy; unknown telemetry is tolerated while unknown lifecycle events reject","result":"the final cycle must complete successfully: its final turn_end.message and final assistant message in agent_end.messages must agree; assistant stopReason=toolUse is allowed only for an intermediate tool turn and stopReason=stop is required for the final turn; text content blocks are concatenated in order; agent_settled is optional for older native releases and terminal when present","failure":"malformed or oversized JSONL, invalid UTF-8, conflicting session identity, overlapping or incomplete cycles, an auto_retry_start without a subsequent complete cycle, unsuccessful final assistant stop reasons (length, error, or aborted), provider error, nonzero exit, empty final text, or more than 1024 retained usage rows reject","tools":"read,grep,find,ls"}` + "\n"

// readOnlyContractV4 is the current read-only evidence policy. A single CLI
// invocation may contain several complete native agent cycles; the adapter
// validates their lifecycle without reimplementing Pi's retry policy.
const readOnlyContractV4 = `{"adapter":"pi:json","mode":"read-only","version":"4","native":"Pi JSON event stream","permissions":"read-only maps the native --tools allowlist to read,grep,find,ls; provider configuration, extensions, authentication, and startup behavior remain provider-owned","session":"one session event with a UUID id; native session discovery and continuation use the exact recorded id","events":"one or more sequential complete native agent cycles may occur within one CLI invocation for retries, compaction, or extension followups; each cycle has agent_start, one or more turn_start/turn_end pairs, and agent_end in order; auto_retry_start requires a subsequent complete cycle; auto_retry_end is accepted only within an active cycle, and a trailing marker after a successful agent_end is malformed; retry markers do not impose provider retry policy; unknown telemetry is tolerated while unknown lifecycle events reject","result":"the final cycle must complete successfully: its final turn_end.message and final assistant message in agent_end.messages must agree; assistant stopReason=toolUse is allowed only for an intermediate tool turn and stopReason=stop is required for the final turn; text content blocks are concatenated in order; agent_settled is optional for older native releases and terminal when present","failure":"malformed or oversized JSONL, invalid UTF-8, conflicting session identity, overlapping or incomplete cycles, an auto_retry_start without a subsequent complete cycle, unsuccessful final assistant stop reasons (length, error, or aborted), provider error, nonzero exit, empty final text, or more than 1024 retained usage rows reject","tools":"read,grep,find,ls"}` + "\n"

// workspaceWriteContract is the immutable evidence policy for native
// workspace-write execution. Pi owns the configured tools, extensions,
// authentication, and permission behavior; the adapter does not impose an
// independent tool allowlist or workspace boundary.
const workspaceWriteContractV3 = `{"adapter":"pi:json","mode":"workspace-write","version":"3","native":"Pi JSON event stream","permissions":"workspace-write uses Pi's provider-native configured tools, extensions, authentication, and permission behavior; the adapter omits --tools and does not impose an independent workspace boundary","session":"one session event with a UUID id; native session discovery and continuation use the exact recorded id","events":"one or more sequential complete native agent cycles may occur within one CLI invocation for retries, compaction, or extension followups; each cycle has agent_start, one or more turn_start/turn_end pairs, and agent_end in order; auto_retry_start requires a subsequent complete cycle; auto_retry_end is completion telemetry and does not require another cycle; retry markers do not impose provider retry policy; unknown telemetry is tolerated while unknown lifecycle events reject","result":"the final cycle must complete successfully: its final turn_end.message and final assistant message in agent_end.messages must agree; assistant stopReason=toolUse is allowed only for an intermediate tool turn and stopReason=stop is required for the final turn; text content blocks are concatenated in order; agent_settled is optional for older native releases and terminal when present","failure":"malformed or oversized JSONL, invalid UTF-8, conflicting session identity, overlapping or incomplete cycles, an auto_retry_start without a subsequent complete cycle, unsuccessful final assistant stop reasons (length, error, or aborted), provider error, nonzero exit, empty final text, or more than 1024 retained usage rows reject","tools":"provider-native configured tools"}` + "\n"

const workspaceWriteContractV4 = `{"adapter":"pi:json","mode":"workspace-write","version":"4","native":"Pi JSON event stream","permissions":"workspace-write uses Pi's provider-native configured tools, extensions, authentication, and permission behavior; the adapter omits --tools and does not impose an independent workspace boundary","session":"one session event with a UUID id; native session discovery and continuation use the exact recorded id","events":"one or more sequential complete native agent cycles may occur within one CLI invocation for retries, compaction, or extension followups; each cycle has agent_start, one or more turn_start/turn_end pairs, and agent_end in order; auto_retry_start requires a subsequent complete cycle; auto_retry_end is accepted only within an active cycle, and a trailing marker after a successful agent_end is malformed; retry markers do not impose provider retry policy; unknown telemetry is tolerated while unknown lifecycle events reject","result":"the final cycle must complete successfully: its final turn_end.message and final assistant message in agent_end.messages must agree; assistant stopReason=toolUse is allowed only for an intermediate tool turn and stopReason=stop is required for the final turn; text content blocks are concatenated in order; agent_settled is optional for older native releases and terminal when present","failure":"malformed or oversized JSONL, invalid UTF-8, conflicting session identity, overlapping or incomplete cycles, an auto_retry_start without a subsequent complete cycle, unsuccessful final assistant stop reasons (length, error, or aborted), provider error, nonzero exit, empty final text, or more than 1024 retained usage rows reject","tools":"provider-native configured tools"}` + "\n"

// legacyContractV1 retains the exact predicate bytes used before native
// provider configuration and startup behavior became part of the provider's
// own contract.
const legacyContractV1 = `{"adapter":"pi:json","mode":"read-only","version":"1","native":"Pi JSON event stream","permissions":"--no-extensions and --offline prevent extension execution and startup package/network work; the native tool allowlist is read,grep,find,ls. Read-only admission rejects project .pi/commands migration state and a native agent directory overlapping the workspace. Pi workspace-write is not advertised because its built-in write and edit tools accept arbitrary absolute paths without a native workspace boundary","session":"one session event with a UUID id; --session-dir binds storage to the resolved native directory and continuation uses the exact recorded id","events":"one agent_start, one or more turn_start/turn_end pairs, and one agent_end in order; message events may occur between turn_start and turn_end; unknown telemetry is tolerated while unknown lifecycle events reject","result":"the final turn_end.message and final assistant message in agent_end.messages must agree; assistant stopReason=toolUse is allowed only for an intermediate tool turn and stopReason=stop is required for the final turn; text content blocks are concatenated in order","failure":"malformed or oversized JSONL, invalid UTF-8, conflicting session identity, missing lifecycle completion, unsuccessful assistant stop reasons (length, error, or aborted), provider error, nonzero exit, empty final text, startup migration that could mutate the workspace, or more than 1024 retained usage rows reject","tools":"read,grep,find,ls"}` + "\n"

func workspaceContractForVersion(version string) string {
	contract := workspaceWriteContractV4
	if version == historicalPredicateV3 {
		contract = workspaceWriteContractV3
	}
	if version != historicalPredicateV3 && version != predicateVersion {
		return ""
	}
	return strings.Replace(contract, "agent_end in order; auto_retry_start", "agent_end in order; message events may occur between turn_start and turn_end; auto_retry_start", 1)
}

func readOnlyContractV5Current() string {
	contract := strings.Replace(readOnlyContractV4Current(), `"version":"4"`, `"version":"5"`, 1)
	contract = strings.Replace(contract, "malformed or oversized JSONL", "malformed JSONL or invalid relevant control values", 1)
	contract = strings.Replace(contract, "or more than 1024 retained usage rows", "usage is optional and unreliable; malformed usage is ignored", 1)
	return contract
}

func workspaceContractV5Current() string {
	contract := strings.Replace(workspaceContractForVersion(predicateVersion), `"version":"4"`, `"version":"5"`, 1)
	contract = strings.Replace(contract, "malformed or oversized JSONL", "malformed JSONL or invalid relevant control values", 1)
	contract = strings.Replace(contract, "or more than 1024 retained usage rows", "usage is optional and unreliable; malformed usage is ignored", 1)
	return contract
}

func readOnlyContractV4Current() string {
	return strings.Replace(readOnlyContractV4, "agent_end in order; auto_retry_start", "agent_end in order; message events may occur between turn_start and turn_end; auto_retry_start", 1)
}

// Contract returns the immutable evidence policy for mode. An empty or
// unsupported mode returns an empty string.
func Contract(mode ...string) string {
	selected := ModeReadOnly
	if len(mode) > 0 && mode[0] != "" {
		selected = mode[0]
	}
	switch selected {
	case ModeReadOnly:
		return readOnlyContractV5Current()
	case ModeWorkspaceWrite:
		return workspaceContractV5Current()
	default:
		return ""
	}
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
	return referenceForVersion(mode, currentPredicateVersion)
}

// Reference returns the read-only predicate for compatibility with provider
// packages that expose one default reference.
func Reference() task.PredicateRef { return ReferenceForMode(ModeReadOnly) }

// LegacyReference returns the historical read-only predicate reference so
// sealed evidence produced before the native-profile revision remains
// collectible.
func LegacyReference() task.PredicateRef {
	return referenceForVersion(ModeReadOnly, legacyPredicateVersion)
}

func readOnlyV2Reference() task.PredicateRef {
	return referenceForVersion(ModeReadOnly, historicalPredicateV2)
}

func readOnlyV3Reference() task.PredicateRef {
	return referenceForVersion(ModeReadOnly, historicalPredicateV3)
}

func workspaceWriteV3Reference() task.PredicateRef {
	return referenceForVersion(ModeWorkspaceWrite, historicalPredicateV3)
}

func readOnlyV4Reference() task.PredicateRef {
	return referenceForVersion(ModeReadOnly, predicateVersion)
}

func workspaceWriteV4Reference() task.PredicateRef {
	return referenceForVersion(ModeWorkspaceWrite, predicateVersion)
}

func referenceForVersion(mode, version string) task.PredicateRef {
	contract := contractForVersion(mode, version)
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: task.ComputeSHA256([]byte(contract))}
}

func contractForVersion(mode, version string) string {
	switch mode {
	case ModeReadOnly:
		return readOnlyContractForVersion(version)
	case ModeWorkspaceWrite:
		if version == currentPredicateVersion {
			return workspaceContractV5Current()
		}
		return workspaceContractForVersion(version)
	default:
		return ""
	}
}

func readOnlyContractForVersion(version string) string {
	switch version {
	case legacyPredicateVersion:
		return legacyContractV1
	case historicalPredicateV2:
		return readOnlyContractV2
	case historicalPredicateV3:
		return readOnlyContractV3
	case predicateVersion:
		return readOnlyContractV4Current()
	case currentPredicateVersion:
		return readOnlyContractV5Current()
	default:
		return ""
	}
}

func validMode(mode string) bool {
	switch mode {
	case ModeReadOnly, ModeWorkspaceWrite:
		return true
	default:
		return false
	}
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
