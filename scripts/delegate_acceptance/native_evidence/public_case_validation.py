"""Canonical public case predicates shared by the release and candidate envelopes.

One owner validates every public case member, its privacy keys and the
aggregate reconciliation. An envelope supplies its trusted facts through an
immutable ``CaseContext`` and keeps its own identity, release cleanup and
fallback rules.
"""

from __future__ import annotations

from dataclasses import dataclass

from .block_reason import encode_block_reason, parse_block_reason
from .contract import (
    BLOCK_REASON_KEY,
    BlockReason,
    CaseDefinition,
    EvidenceError,
    MatrixRow,
    _aggregate_exit,
    _case_result_verdict,
    _validate_case_completion,
    _validate_case_retention,
    _validate_case_sessions,
    _validate_requested_options,
)
from .public_contract import (
    CANDIDATE_CASE_SCHEMA_VERSION,
    CASE_DEFINITIONS,
    CASE_IDS,
    OPAQUE_TEXT,
    PUBLIC_SCHEMA_VERSION,
    PUBLIC_TEXT,
    TASK_ID,
    CaseOutcome,
    PublicProofError,
    parse_canonical,
    privacy_walk,
    public_binaries,
    require_case_schema_version,
    require_object,
    require_sha256,
)
from ..release_verification.contract import BinaryDigests


_CASE_KEYS = {
    "schema_version", "kind", "case_id", "provider", "permission", "scenario",
    "status", "exit_code", "evidence_status", "source_compact_sha256",
    "source_proof", "cleanup", "natural_daemon_shutdown",
}
_PROOF_KEYS = {
    "schema_version", "kind", "state", "final_status", "source_status", "source_receipt_sha256",
    "gate", "provider", "permission", "mode", "scenario", "requested_options_state",
    "requested_model", "requested_effort", "task_ids", "task_session_bindings",
    "diagnostic_task_ids", "session_ids", "binary_identities", "lifecycle_proof",
}
_BINARY_KEYS = {"state", "sha256", "task_ids"}
_CLEANUP_KEYS = {
    "observation", "raw_before", "raw_after", "raw_deleted", "terminal_state",
    "journal_sha256", "authorization_sha256",
}
_OBSERVATION_KEYS = {
    "status", "source_status", "supervisor_shutdown_observed", "raw_process_captures",
    "method", "cleanup_receipt_sha256",
}
_SUMMARY_KEYS = {"root_count", "file_count", "byte_count", "complete", "count", "bytes"}
_SESSION_KEYS = {"state", "values", "by_alias"}
_LIFECYCLE_KEYS = {
    "state", "cleanup_status", "supervisor_shutdown_observed",
    "natural_daemon_shutdown", "failure_diagnostics_sha256",
}
_SESSION_FACTS = tuple[list[dict[str, str]], list[str], list[str], dict[str, object]]


@dataclass(frozen=True)
class CaseContext:
    """Trusted facts every case is checked against.

    ``case_receipts`` and ``cleanup_journals`` are (case id, SHA-256) pairs
    from the proof's source manifest; the models and binaries come from the
    envelope's trusted expectation. ``case_schema_version`` is the envelope's
    public case format: the release keeps version 1, the candidate requires 2.
    """

    opencode_model: str
    ccr_model: str | None
    binaries: BinaryDigests
    case_receipts: tuple[tuple[str, str], ...]
    cleanup_journals: tuple[tuple[str, str], ...]
    case_schema_version: int = PUBLIC_SCHEMA_VERSION

    def __post_init__(self) -> None:
        require_case_schema_version(self.case_schema_version)

    def case_receipt(self, case_id: str) -> str | None:
        return dict(self.case_receipts).get(case_id)

    def cleanup_journal(self, case_id: str) -> str | None:
        return dict(self.cleanup_journals).get(case_id)


def _case_keys(case_schema_version: int) -> set[str]:
    """The exact member keys of one public case in the given format."""
    if require_case_schema_version(case_schema_version) == CANDIDATE_CASE_SCHEMA_VERSION:
        return _CASE_KEYS | {BLOCK_REASON_KEY}
    return _CASE_KEYS


def case_privacy_keys(value: object, *, case_schema_version: int = PUBLIC_SCHEMA_VERSION) -> None:
    if not isinstance(value, dict):
        privacy_walk(value)
        return
    if not set(value).issubset(_case_keys(case_schema_version)):
        raise PublicProofError("public_privacy_field_denied")
    proof = value.get("source_proof")
    if isinstance(proof, dict):
        if not set(proof).issubset(_PROOF_KEYS):
            raise PublicProofError("public_privacy_field_denied")
        sessions = proof.get("session_ids")
        if isinstance(sessions, dict) and not set(sessions).issubset(_SESSION_KEYS):
            raise PublicProofError("public_privacy_field_denied")
        binaries = proof.get("binary_identities")
        if isinstance(binaries, dict) and not set(binaries).issubset(_BINARY_KEYS):
            raise PublicProofError("public_privacy_field_denied")
        binary_hashes = binaries.get("sha256") if isinstance(binaries, dict) else None
        if isinstance(binary_hashes, dict) and not set(binary_hashes).issubset({
                "provider_sha256", "delegate_sha256", "runner_sha256", "pueue_sha256", "pueued_sha256",
        }):
            raise PublicProofError("public_privacy_field_denied")
        lifecycle = proof.get("lifecycle_proof")
        if isinstance(lifecycle, dict) and not set(lifecycle).issubset(_LIFECYCLE_KEYS):
            raise PublicProofError("public_privacy_field_denied")
        bindings = proof.get("task_session_bindings")
        if isinstance(bindings, list):
            if any(isinstance(item, dict)
                   and not set(item).issubset({"alias", "task_id", "session_id"})
                   for item in bindings):
                raise PublicProofError("public_privacy_field_denied")
    cleanup = value.get("cleanup")
    if isinstance(cleanup, dict):
        if not set(cleanup).issubset(_CLEANUP_KEYS):
            raise PublicProofError("public_privacy_field_denied")
        observation = cleanup.get("observation")
        if isinstance(observation, dict) and not set(observation).issubset(_OBSERVATION_KEYS):
            raise PublicProofError("public_privacy_field_denied")
        for key in ("raw_before", "raw_after"):
            summary = cleanup.get(key)
            if isinstance(summary, dict) and not set(summary).issubset(_SUMMARY_KEYS):
                raise PublicProofError("public_privacy_field_denied")
    privacy_walk(value)


def validate_aggregate_shape(value: object) -> dict[str, object]:
    aggregate = require_object(value, {
        "status", "exit_code", "case_results_verdict", "pass_count", "blocked_count",
        "failed_count", "unresolved_case_count", "expected_case_count",
        "require_at_least_one_pass",
    }, "public_aggregate_invalid")
    if (not isinstance(aggregate.get("status"), str)
            or aggregate.get("status") not in {"PASS", "BLOCKED", "FAIL"}
            or not isinstance(aggregate.get("case_results_verdict"), str)
            or aggregate.get("case_results_verdict") not in {"PASS", "BLOCKED", "FAIL"}
            or type(aggregate.get("exit_code")) is not int
            or not 0 <= aggregate["exit_code"] <= 255
            or type(aggregate.get("require_at_least_one_pass")) is not bool):
        raise PublicProofError("public_aggregate_invalid")
    for key in ("pass_count", "blocked_count", "failed_count", "unresolved_case_count"):
        if type(aggregate.get(key)) is not int or not 0 <= aggregate[key] <= len(CASE_IDS):
            raise PublicProofError("public_aggregate_count_invalid")
    if aggregate.get("expected_case_count") != len(CASE_IDS):
        raise PublicProofError("public_aggregate_count_invalid")
    return aggregate


def _validate_case_shape(value: object, expected_case: CaseDefinition,
                         case_schema_version: int) -> dict[str, object]:
    case = require_object(value, _case_keys(case_schema_version), "public_case_invalid")
    if (type(case.get("schema_version")) is not int
            or case.get("schema_version") != case_schema_version
            or case.get("kind") != "public-native-acceptance-case"):
        raise PublicProofError("public_case_version_invalid")
    for key, expected in (("case_id", expected_case.case_id),
                          ("provider", expected_case.provider),
                          ("permission", expected_case.permission),
                          ("scenario", expected_case.scenario)):
        if case.get(key) != expected:
            raise PublicProofError("public_case_identity_mismatch")
    if (not isinstance(case.get("status"), str)
            or case.get("status") not in {"PASS", "BLOCKED", "FAIL"}
            or not isinstance(case.get("evidence_status"), str)
            or case.get("evidence_status") not in {"RESOLVED", "UNRESOLVED"}
            or type(case.get("exit_code")) is not int
            or not 0 <= case["exit_code"] <= 255
            or type(case.get("natural_daemon_shutdown")) not in {bool, type(None)}):
        raise PublicProofError("public_case_verdict_invalid")
    expected_exit = {"PASS": 0, "BLOCKED": 2}.get(case["status"])
    if (expected_exit is not None and case["exit_code"] != expected_exit
            or case["status"] == "FAIL" and case["exit_code"] in {0, 2}):
        raise PublicProofError("public_case_exit_mismatch")
    require_sha256(case.get("source_compact_sha256"))
    return case


def _case_block_reason(case: dict[str, object]) -> BlockReason | None:
    """Return the typed reason of a shape-valid candidate case through the canonical codec."""
    carried = case[BLOCK_REASON_KEY]
    try:
        reason = None if carried is None else parse_block_reason(carried)
        encode_block_reason(str(case["status"]), reason)
    except EvidenceError:
        raise PublicProofError("public_case_block_reason_invalid") from None
    return reason


def _validate_public_proof_fields(
        case: dict[str, object], expected: CaseContext) -> tuple[dict[str, object], _SESSION_FACTS]:
    proof = require_object(case["source_proof"], _PROOF_KEYS, "public_case_proof_invalid")
    if (type(proof.get("schema_version")) is not int
            or proof.get("schema_version") != PUBLIC_SCHEMA_VERSION
            or proof.get("kind") != "public-native-acceptance-case-proof"):
        raise PublicProofError("public_case_proof_version_invalid")
    for key, expected_value in (("final_status", case["status"]), ("gate", "acceptance-native"),
                                ("provider", case["provider"]), ("permission", case["permission"]),
                                ("mode", case["permission"]), ("scenario", case["scenario"])):
        if proof.get(key) != expected_value:
            raise PublicProofError("public_case_proof_identity_mismatch")
    if proof.get("state") not in {"complete", "blocked-before-runtime", "unresolved"}:
        raise PublicProofError("public_case_proof_state_invalid")
    if proof.get("source_status") not in {None, "pass", "passed", "blocked", "block", "fail", "failed"}:
        raise PublicProofError("public_case_source_status_invalid")
    source_digest = proof.get("source_receipt_sha256")
    if source_digest is not None:
        require_sha256(source_digest)
    options = {key: proof[key] for key in (
        "requested_options_state", "requested_model", "requested_effort",
    )}
    _validate_requested_options(options)
    for key in ("requested_model", "requested_effort"):
        if options[key] is not None and PUBLIC_TEXT.fullmatch(str(options[key])) is None:
            raise PublicProofError("public_case_option_invalid")
    model = options["requested_model"]
    if expected_case_provider(case) == "opencode" and model != expected.opencode_model:
        raise PublicProofError("public_opencode_model_mismatch")
    if expected_case_provider(case) == "ccr":
        if expected.ccr_model is None:
            if case["status"] != "BLOCKED" or model is not None:
                raise PublicProofError("public_ccr_absence_not_blocked")
        elif model != expected.ccr_model:
            raise PublicProofError("public_ccr_model_mismatch")
    sessions = _validate_case_sessions(proof)
    ids = [*proof["task_ids"], *proof["diagnostic_task_ids"]]
    ids.extend(binding["task_id"] for binding in proof["task_session_bindings"])
    if any(not isinstance(item, str) or TASK_ID.fullmatch(item) is None for item in ids):
        raise PublicProofError("public_case_task_id_invalid")
    session_values = [*proof["session_ids"]["values"],
                      *proof["session_ids"]["by_alias"].values()]
    if any(not isinstance(item, str) or OPAQUE_TEXT.fullmatch(item) is None
           or item.startswith("/") for item in session_values):
        raise PublicProofError("public_case_session_id_invalid")
    return proof, sessions


def expected_case_provider(case: dict[str, object]) -> str:
    provider = case.get("provider")
    if not isinstance(provider, str):
        raise PublicProofError("public_case_provider_invalid")
    return provider.split(":", 1)[0]


def _validate_public_binary_facts(
        case: dict[str, object], proof: dict[str, object],
        context: CaseContext) -> tuple[dict[str, object], list[str]]:
    binaries = require_object(proof["binary_identities"], _BINARY_KEYS,
                              "public_case_binary_invalid")
    hash_names = {"provider_sha256", "delegate_sha256", "runner_sha256", "pueue_sha256", "pueued_sha256"}
    if (binaries.get("state") not in {"complete", "unavailable"}
            or not isinstance(binaries.get("sha256"), dict)
            or not set(binaries["sha256"]).issubset(hash_names)):
        raise PublicProofError("public_case_binary_invalid")
    hashes = binaries["sha256"]
    for digest in hashes.values():
        require_sha256(digest)
    bindings = {f"{name}_sha256": digest for name, digest in public_binaries(context.binaries).items()}
    if any(key in hashes and hashes[key] != digest for key, digest in bindings.items()):
        raise PublicProofError("public_case_binary_binding_mismatch")
    ids = binaries.get("task_ids")
    if (not isinstance(ids, list) or any(not isinstance(item, str) or TASK_ID.fullmatch(item) is None
                                        for item in ids)
            or ids != sorted(set(ids))):
        raise PublicProofError("public_case_binary_tasks_invalid")
    if (case["status"] == "PASS" and case["evidence_status"] == "RESOLVED"
            and (binaries["state"] != "complete" or not ids or set(hashes) != hash_names
                 or any(hashes[key] != digest for key, digest in bindings.items()))):
        raise PublicProofError("public_case_binary_incomplete")
    return binaries, ids


def _validate_lifecycle(proof: dict[str, object], case: dict[str, object]) -> dict[str, object]:
    lifecycle = require_object(proof.get("lifecycle_proof"), _LIFECYCLE_KEYS,
                               "public_case_lifecycle_invalid")
    if (lifecycle.get("state") not in {"complete", "unresolved"}
            or lifecycle.get("cleanup_status") not in {
                "verified", "unverified", "missing", "malformed", "unreadable", None,
            }
            or type(lifecycle.get("supervisor_shutdown_observed")) not in {bool, type(None)}
            or type(lifecycle.get("natural_daemon_shutdown")) not in {bool, type(None)}):
        raise PublicProofError("public_case_lifecycle_invalid")
    digest = lifecycle.get("failure_diagnostics_sha256")
    if digest is not None:
        require_sha256(digest)
    if lifecycle["natural_daemon_shutdown"] != case["natural_daemon_shutdown"]:
        raise PublicProofError("public_case_lifecycle_mismatch")
    return lifecycle


def _summary(value: object) -> dict[str, object]:
    summary = require_object(value, _SUMMARY_KEYS, "public_cleanup_summary_invalid")
    integer_keys = ("root_count", "file_count", "byte_count", "count", "bytes")
    if (any(type(summary.get(key)) is not int or summary[key] < 0 for key in integer_keys)
            or type(summary.get("complete")) is not bool
            or summary["bytes"] != summary["byte_count"]):
        raise PublicProofError("public_cleanup_summary_invalid")
    return summary


def _validate_cleanup(case: dict[str, object], context: CaseContext) -> dict[str, object]:
    cleanup = require_object(case.get("cleanup"), _CLEANUP_KEYS, "public_cleanup_invalid")
    observation = require_object(cleanup.get("observation"), _OBSERVATION_KEYS,
                                 "public_cleanup_observation_invalid")
    if observation.get("status") not in {
            "verified", "unverified", "missing", "malformed", "unreadable"}:
        raise PublicProofError("public_cleanup_observation_invalid")
    for key in ("supervisor_shutdown_observed",):
        if type(observation.get(key)) not in {bool, type(None)}:
            raise PublicProofError("public_cleanup_observation_invalid")
    for key, allowed in (("source_status", {None, "removed", "complete"}),
                         ("raw_process_captures", {None, "removed", "retained"}),
                         ("method", {None, "blocked-before-runtime-cleanup"})):
        if observation.get(key) not in allowed:
            raise PublicProofError("public_cleanup_observation_invalid")
    receipt_digest = observation.get("cleanup_receipt_sha256")
    if receipt_digest is not None:
        require_sha256(receipt_digest)
    _summary(cleanup.get("raw_before"))
    after = _summary(cleanup.get("raw_after"))
    journal_digest, authorization = cleanup.get("journal_sha256"), cleanup.get("authorization_sha256")
    _validate_case_retention(case["evidence_status"], cleanup["terminal_state"], cleanup["raw_deleted"],
                             journal_digest is not None or authorization is not None
                             or context.cleanup_journal(case["case_id"]) is not None)
    if case["evidence_status"] == "RESOLVED":
        require_sha256(journal_digest)
        require_sha256(authorization)
        if (journal_digest != context.cleanup_journal(case["case_id"])
                or after != {"root_count": 0, "file_count": 0, "byte_count": 0,
                             "complete": True, "count": 0, "bytes": 0}):
            raise PublicProofError("public_cleanup_binding_mismatch")
    return cleanup


def _validate_one_case(
        data: bytes, expected_case: CaseDefinition,
        context: CaseContext) -> tuple[dict[str, object], MatrixRow, bool, BlockReason | None]:
    value = parse_canonical(data, "public_case_invalid")
    case = _validate_case_shape(value, expected_case, context.case_schema_version)
    candidate = context.case_schema_version == CANDIDATE_CASE_SCHEMA_VERSION
    reason = _case_block_reason(case) if candidate else None
    if case["source_compact_sha256"] != context.case_receipt(expected_case.case_id):
        raise PublicProofError("public_case_source_digest_mismatch")
    proof, sessions = _validate_public_proof_fields(case, context)
    binaries, binary_ids = _validate_public_binary_facts(case, proof, context)
    _validate_lifecycle(proof, case)
    _validate_cleanup(case, context)
    _validate_case_completion(case, proof, sessions,
                              (binaries, binary_ids))
    row = MatrixRow(expected_case, case["exit_code"], case["status"])
    unresolved = case["evidence_status"] == "UNRESOLVED"
    return case, row, unresolved, reason


@dataclass(frozen=True)
class CaseResults:
    """Every canonical case in order, with its matrix row and evidence state.

    ``outcomes`` are the typed case outcomes of the candidate format; the
    release format carries no block reason, so it has none.
    """

    cases: tuple[dict[str, object], ...]
    rows: tuple[MatrixRow, ...]
    unresolved_case_ids: tuple[str, ...]
    outcomes: tuple[CaseOutcome, ...] | None

    def counts(self) -> dict[str, int]:
        return {
            "pass_count": sum(row.status == "PASS" for row in self.rows),
            "blocked_count": sum(row.status == "BLOCKED" for row in self.rows),
            "failed_count": sum(row.status == "FAIL" for row in self.rows),
        }

    @property
    def verdict(self) -> str:
        return _case_result_verdict(self.rows, bool(self.unresolved_case_ids))


def validate_cases(entries: dict[str, bytes], context: CaseContext) -> CaseResults:
    """Validate every canonical case member against the trusted context."""
    cases: list[dict[str, object]] = []
    rows: list[MatrixRow] = []
    unresolved: list[str] = []
    outcomes: list[CaseOutcome] = []
    candidate = context.case_schema_version == CANDIDATE_CASE_SCHEMA_VERSION
    for definition in CASE_DEFINITIONS:
        case, row, is_unresolved, reason = _validate_one_case(
            entries[f"cases/{definition.case_id}.json"], definition, context)
        cases.append(case)
        rows.append(row)
        if candidate:
            outcomes.append(CaseOutcome(case_id=definition.case_id, status=row.status, block_reason=reason))
        if is_unresolved:
            unresolved.append(definition.case_id)
    return CaseResults(tuple(cases), tuple(rows), tuple(sorted(unresolved)),
                       tuple(outcomes) if candidate else None)


def require_case_counts(aggregate: dict[str, object], results: CaseResults) -> None:
    """The aggregate counts are exactly the validated cases."""
    if (any(aggregate[key] != value for key, value in results.counts().items())
            or aggregate["unresolved_case_count"] != len(results.unresolved_case_ids)):
        raise PublicProofError("public_aggregate_case_counts_mismatch")


def require_aggregate_verdict(aggregate: dict[str, object], results: CaseResults) -> str:
    """The aggregate verdict, status and exit code follow from the validated cases."""
    verdict = results.verdict
    if aggregate["case_results_verdict"] != verdict:
        raise PublicProofError("public_aggregate_verdict_mismatch")
    status = "FAIL" if verdict == "FAIL" else "PASS" if verdict == "PASS" else "BLOCKED"
    if aggregate["status"] != status or aggregate["exit_code"] != _aggregate_exit(
            status, verdict, aggregate["require_at_least_one_pass"]):
        raise PublicProofError("public_aggregate_status_mismatch")
    return verdict


def agy_case_model_bound(case: dict[str, object], requested: str | None) -> bool:
    """A PASS Antigravity case recorded exactly the requested model option.

    ``requested`` None is AGY's native default; the comparison is against the
    recorded requested option, never a backend alias. Other cases are bound.
    """
    if expected_case_provider(case) != "antigravity" or case.get("status") != "PASS":
        return True
    proof = case.get("source_proof")
    return isinstance(proof, dict) and proof.get("requested_model") == requested
