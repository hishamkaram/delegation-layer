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

// followUpSelectionPolicy is the follow-up answer and failed-request policy
// clause shared by the current print and interactive contracts. Its bytes are
// bound into each revision's pinned digest.
const followUpSelectionPolicy = "without accepted nonempty root follow-up answers the principal payload is byte-exact; otherwise the principal and every accepted nonempty root follow-up answer appear once in original native result_index order, preserving identical bodies, UTF-8 bytes, whitespace and trailing newlines; presentation starts with [delegate responses v1] followed by a newline, and each section has [delegate response result_index=N origin=caller|task-notification bytes=K] followed by a newline, exactly K decoded UTF-8 bytes, and a newline separator; empty zero-turn acknowledgements and nested assistant text are not sections; a prefix alone is not machine authority; one full validation pass precedes one sequential projection pass through the canonical reducer with constant retained answer metadata and no answer-count cap; a result with subtype success and is_error true is the documented failed final request and rejects as a provider failure, never as success or malformed output; when the latest same-session rate_limit_event after system/init states a rate_limit_info object whose status is exactly rejected and the next root result is a failed request with terminal_reason api_error, and that result's failure is the reported refusal of an otherwise valid identity-matched stream, the rejection reports the constant diagnostic provider_quota_exhausted with field rate_limit_info and no provider value; every root result resets that correlation, and rate-limit telemetry alone never rejects or states a quota when its info is absent, null, mistyped, ambiguous, or has any other status"

func followUpNativeContract(mode string) string {
	contract := advisoryNativeContract(mode)
	if contract == "" {
		return ""
	}
	oldVersion, version := advisoryPredicateVersion, predicateVersion
	if mode == WorkspaceWriteMode {
		oldVersion, version = advisoryWorkspaceWriteVersion, workspaceWritePredicateVersion
	}
	contract = strings.Replace(contract, `"version":"`+oldVersion+`"`, `"version":"`+version+`"`, 1)
	return strings.Replace(contract, "only the principal result is selected as the answer, follow-up answers are never selected", followUpSelectionPolicy, 1)
}
