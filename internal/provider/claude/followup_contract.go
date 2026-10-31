package claude

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// AdvisoryNativeContractForMode freezes the principal-only advisory bytes for
// adapters whose historical references embed the Claude contract.
func AdvisoryNativeContractForMode(mode string) string { return advisoryNativeContract(mode) }

func advisoryReference(mode string) task.PredicateRef {
	contract := advisoryNativeContract(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	version := advisoryPredicateVersion
	if mode == WorkspaceWriteMode {
		version = advisoryWorkspaceWriteVersion
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: task.ComputeSHA256([]byte(contract))}
}

// followUpProjectionPolicy is the frozen follow-up answer clause of the
// projection revisions, sealed before failed final requests and quota
// exhaustion were classified. Its bytes are bound into their pinned digests.
const followUpProjectionPolicy = "without accepted nonempty root follow-up answers the principal payload is byte-exact; otherwise the principal and every accepted nonempty root follow-up answer appear once in original native result_index order, preserving identical bodies, UTF-8 bytes, whitespace and trailing newlines; presentation starts with [delegate responses v1] followed by a newline, and each section has [delegate response result_index=N origin=caller|task-notification bytes=K] followed by a newline, exactly K decoded UTF-8 bytes, and a newline separator; empty zero-turn acknowledgements and nested assistant text are not sections; a prefix alone is not machine authority; one full validation pass precedes one sequential projection pass through the canonical reducer with constant retained answer metadata and no answer-count cap"

// followUpSelectionPolicy is the follow-up answer and failed-request policy
// clause shared by the current print and interactive contracts: the frozen
// projection clause followed by the failed-request and quota clause. Its bytes
// are bound into each revision's pinned digest.
const followUpSelectionPolicy = followUpProjectionPolicy + "; a result with subtype success and is_error true is the documented failed final request and rejects as a provider failure, never as success or malformed output; when the latest same-session rate_limit_event after system/init states a rate_limit_info object whose status is exactly rejected and the next root result is a failed request with terminal_reason api_error, and that result's failure is the reported refusal of an otherwise valid identity-matched stream, the rejection reports the constant diagnostic provider_quota_exhausted with field rate_limit_info and no provider value; every root result resets that correlation, and rate-limit telemetry alone never rejects or states a quota when its info is absent, null, mistyped, ambiguous, or has any other status"

func followUpNativeContract(mode string) string {
	return followUpNativeContractWithPolicy(mode, followUpSelectionPolicy)
}

// projectionNativeContract returns the frozen bytes of the projection
// revision for mode: read-only native-permissions-v6 (digest
// d750a584e223e4c80f2cc165c6ee4cc6d7b664b26a740119671990c966cc36cb) and
// workspace-write native-permissions-v7 (digest
// 35853e32272b55b35b2276309ae7f57909d2d462ebed65b88e30f26f62616f11).
func projectionNativeContract(mode string) string {
	return followUpNativeContractWithPolicy(mode, followUpProjectionPolicy)
}

// ProjectionNativeContractForMode freezes the projection bytes for adapters
// whose recorded references embed the Claude contract.
func ProjectionNativeContractForMode(mode string) string { return projectionNativeContract(mode) }

// projectionReference identifies the frozen projection revision of mode. It
// shares its version string with the current reference of the same mode and
// differs by digest. It is replay-only and a successor source of the current
// revision, never a recovery source or target.
func projectionReference(mode string) task.PredicateRef {
	contract := projectionNativeContract(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	version := predicateVersion
	if mode == WorkspaceWriteMode {
		version = workspaceWritePredicateVersion
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: task.ComputeSHA256([]byte(contract))}
}

// followUpNativeContractWithPolicy derives a follow-up revision of mode from
// the frozen advisory bytes and the given follow-up selection clause.
func followUpNativeContractWithPolicy(mode, policy string) string {
	contract := advisoryNativeContract(mode)
	if contract == "" {
		return ""
	}
	oldVersion, version := advisoryPredicateVersion, predicateVersion
	if mode == WorkspaceWriteMode {
		oldVersion, version = advisoryWorkspaceWriteVersion, workspaceWritePredicateVersion
	}
	contract = strings.Replace(contract, `"version":"`+oldVersion+`"`, `"version":"`+version+`"`, 1)
	return strings.Replace(contract, "only the principal result is selected as the answer, follow-up answers are never selected", policy, 1)
}
