"""Contract for native acceptance evidence."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from ..evidence.digests import sha as _digest_bytes
import json
import posixpath
import re


SCHEMA_VERSION = 1


AGGREGATE_KIND = "native-acceptance-aggregate"


AGGREGATE_RECEIPT_NAME = "aggregate-receipt.json"


CASE_MATRIX_NAME = "case-matrix.tsv"


CASE_ROOT_NAME = "cases"


COMPACT_RECEIPT_ROOT_NAME = "receipts"


CASE_OWNER_ROOT_NAME = "case-owners"


CASE_CLEANUP_ROOT_NAME = "case-cleanup"


RETENTION_PLAN_NAME = "retention-plan.json"


RETENTION_CLEANUP_ROOT_NAME = "retention-cleanup"


MAX_INPUT_BYTES = 1 << 20


MAX_RECEIPT_BYTES = 32 * 1024


MAX_MATRIX_BYTES = 64 * 1024


MAX_WALK_ENTRIES = 4096


MAX_WALK_BYTES = 16 * 1024 * 1024


DEFAULT_RETENTION_COUNT = 3


MAX_RETENTION_COUNT = 32


HEX_DIGEST = re.compile(r"^[0-9a-f]{64}$")


SAFE_TEXT = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:+/-]{0,127}$")


MATRIX_HEADER = (
    "case_id\tprovider\tpermission\tscenario\texit_code\tstatus\tcase_output\n"
)


class EvidenceError(Exception):
    """A bounded, caller-actionable evidence error."""


@dataclass(frozen=True)
class CaseDefinition:
    case_id: str
    provider: str
    permission: str
    scenario: str
    case_output: str


@dataclass(frozen=True)
class MatrixRow:
    definition: CaseDefinition
    exit_code: int
    status: str


def _slug(value: str) -> str:
    return re.sub(r"[^A-Za-z0-9]+", "-", value).strip("-").lower()


def _case_definition(index: int, provider: str, permission: str, scenario: str) -> CaseDefinition:
    suffix = "-".join((_slug(provider), _slug(permission), _slug(scenario)))
    case_id = f"case-{index:02d}-{suffix}"
    return CaseDefinition(case_id, provider, permission, scenario, f"{CASE_ROOT_NAME}/{case_id}")


def expected_cases() -> tuple[CaseDefinition, ...]:
    """Return the release gate's complete, ordered case matrix."""
    definitions: list[CaseDefinition] = []
    providers = (
        "antigravity:print", "claude:print", "ccr:print", "codex:exec", "pi:json", "opencode:run",
    )
    index = 1
    for provider in providers:
        definitions.append(_case_definition(index, provider, "workspace-write", "write"))
        index += 1
    definitions.append(_case_definition(index, "codex:exec", "workspace-write", "timeout-resume"))
    index += 1
    for provider in providers:
        definitions.append(_case_definition(index, provider, "read-only", "read-only"))
        index += 1
    definitions.append(_case_definition(index, "codex:review", "read-only", "read-only"))
    definitions.append(_case_definition(index + 1, "codex:exec", "read-only", "non-git"))
    definitions.append(_case_definition(index + 2, "codex:exec", "workspace-write", "non-git"))
    return tuple(definitions)


EXPECTED_CASES = expected_cases()


# One bounded case reference per matrix row, plus the aggregate envelope.
MAX_AGGREGATE_BYTES = (len(EXPECTED_CASES) + 1) * MAX_RECEIPT_BYTES


def canonical_json(value: object) -> bytes:
    try:
        return (json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n").encode()
    except (TypeError, ValueError) as error:
        raise EvidenceError("receipt_json_invalid") from error


def _empty_summary() -> dict[str, int | bool]:
    return {"root_count": 0, "file_count": 0, "byte_count": 0, "complete": True,
            "count": 0, "bytes": 0}


def _safe_source_status(value: object) -> bool:
    return isinstance(value, str) and len(value) <= 64 and not any(ord(char) < 32 for char in value)


def _source_status_matches(raw: dict[str, object], status: str) -> bool:
    source_status = raw.get("status")
    if not _safe_source_status(source_status):
        return False
    normalized = source_status.lower()
    return {
        "PASS": normalized in {"pass", "passed"},
        "BLOCKED": normalized in {"blocked", "block"},
        "FAIL": normalized in {"fail", "failed"},
    }[status]


def _compact_provider_versions(raw: dict[str, object]) -> dict[str, str] | None:
    value = raw.get("provider_versions")
    if not isinstance(value, dict):
        return None
    result: dict[str, str] = {}
    for key, version in value.items():
        if (isinstance(key, str) and SAFE_TEXT.fullmatch(key) is not None
                and isinstance(version, str) and SAFE_TEXT.fullmatch(version) is not None):
            result[key] = version
    return dict(sorted(result.items())) or None


def _safe_runtime_executable(value: object) -> str | None:
    if (not isinstance(value, str) or not value or len(value) > 4096
            or not posixpath.isabs(value) or any(ord(character) < 32 for character in value)):
        return None
    return value


def _compact_provider_runtime_identities(
        raw: dict[str, object]) -> dict[str, dict[str, str]] | None:
    value = raw.get("provider_runtime_identities")
    if not isinstance(value, dict) or not value or len(value) > 64:
        return None
    result: dict[str, dict[str, str]] = {}
    for task_id, identity in value.items():
        if (not isinstance(task_id, str) or re.fullmatch(r"[0-9a-f]{32}", task_id) is None
                or not isinstance(identity, dict)
                or set(identity) != {"executable", "version", "sha256"}):
            return None
        executable = _safe_runtime_executable(identity.get("executable"))
        version = _safe_proof_text(identity.get("version"))
        sha256 = identity.get("sha256")
        if executable is None or version is None or not isinstance(sha256, str) \
                or HEX_DIGEST.fullmatch(sha256) is None:
            return None
        result[task_id] = {"executable": executable, "version": version, "sha256": sha256}
    return dict(sorted(result.items()))


def _safe_proof_text(value: object, *, allow_empty: bool = False) -> str | None:
    if not isinstance(value, str) or len(value) > 128 or any(ord(char) < 32 for char in value):
        return None
    if not allow_empty and not value:
        return None
    return value


_REQUESTED_OPTION_KEYS = ("requested_model", "requested_effort")


_REQUESTED_OPTION_STATES = frozenset({"recorded", "missing", "invalid"})


def _invalid_requested_options() -> dict[str, object]:
    return {
        "requested_options_state": "invalid",
        "requested_model": None,
        "requested_effort": None,
    }


def _normalize_requested_options(value: dict[str, object] | None) -> dict[str, object]:
    """Normalize requested options while preserving explicit provider defaults."""
    if value is None or any(key not in value for key in _REQUESTED_OPTION_KEYS):
        return {
            "requested_options_state": "missing",
            "requested_model": None,
            "requested_effort": None,
        }
    values: dict[str, str | None] = {}
    for key in _REQUESTED_OPTION_KEYS:
        option = value[key]
        if option is None:
            values[key] = None
            continue
        safe = _safe_proof_text(option)
        if safe is None:
            return _invalid_requested_options()
        values[key] = safe
    return {
        "requested_options_state": "recorded",
        "requested_model": values["requested_model"],
        "requested_effort": values["requested_effort"],
    }


def _source_requested_options(
        source: dict[str, object],
        binding: dict[str, object] | None,
        *,
        require_binding: bool,
        binding_error: str | None = None) -> dict[str, object]:
    """Return source options only after a successful case matches its binding."""
    source_options = _normalize_requested_options(source)
    if source_options["requested_options_state"] != "recorded" or not require_binding:
        return source_options
    if binding_error is not None:
        return _invalid_requested_options()
    binding_options = _normalize_requested_options(binding)
    if binding_options["requested_options_state"] != "recorded":
        return binding_options
    if any(source_options[key] != binding_options[key] for key in _REQUESTED_OPTION_KEYS):
        return _invalid_requested_options()
    return source_options


def _validate_requested_options(value: object) -> None:
    if not isinstance(value, dict) or set(value) != {
            "requested_options_state", "requested_model", "requested_effort"}:
        raise EvidenceError("aggregate_case_requested_options_invalid")
    state = value["requested_options_state"]
    if not isinstance(state, str) or state not in _REQUESTED_OPTION_STATES:
        raise EvidenceError("aggregate_case_requested_options_invalid")
    if state == "recorded":
        normalized = _normalize_requested_options({
            key: value[key] for key in _REQUESTED_OPTION_KEYS
        })
        if normalized != value:
            raise EvidenceError("aggregate_case_requested_options_invalid")
    elif any(value[key] is not None for key in _REQUESTED_OPTION_KEYS):
        raise EvidenceError("aggregate_case_requested_options_invalid")


def _task_identifier_values(value: object, *, limit: int = 64) -> list[str]:
    result: list[str] = []
    if isinstance(value, dict):
        for item in value.values():
            if isinstance(item, str):
                safe = _safe_proof_text(item)
                if safe is not None:
                    result.append(safe)
            elif isinstance(item, (dict, list)) and len(result) < limit:
                result.extend(_task_identifier_values(item, limit=limit - len(result)))
            if len(result) >= limit:
                break
    elif isinstance(value, list):
        for item in value[:limit]:
            if isinstance(item, str):
                safe = _safe_proof_text(item)
                if safe is not None:
                    result.append(safe)
            elif isinstance(item, (dict, list)) and len(result) < limit:
                result.extend(_task_identifier_values(item, limit=limit - len(result)))
            if len(result) >= limit:
                break
    return sorted(set(result))[:limit]


def _expected_task_aliases(provider: str, scenario: str) -> tuple[str, ...]:
    """Return the authoritative task turns required by one passing case."""
    if scenario == "interactive":
        return ("interactive",)
    if scenario == "timeout-resume":
        return ("timeout_source", "timeout_resume")
    if provider == "codex:review":
        return ("fresh",)
    return ("fresh", "resume")


def _case_cleanup_authorization(value: dict[str, object]) -> str:
    cleanup = value.get("cleanup")
    immutable_cleanup = None
    if isinstance(cleanup, dict):
        immutable_cleanup = {
            "observation": cleanup.get("observation"),
            "raw_before": cleanup.get("raw_before"),
        }
    payload = {
        key: value.get(key) for key in (
            "schema_version", "kind", "gate", "scope", "case_id", "provider",
            "permission", "scenario", "status", "exit_code", "evidence_status",
            "source_receipt_sha256", "source_proof", "creation_identity",
        )
    }
    payload["cleanup"] = immutable_cleanup
    return _digest_bytes(canonical_json(payload))


def _summary_with_removed(
        after: dict[str, object], victims: list[dict[str, object]]) -> dict[str, object]:
    summaries = [victim.get("before") for victim in victims]
    if any(not isinstance(summary, dict) or summary.get("complete") is not True
           for summary in summaries):
        return {"root_count": 0, "file_count": 0, "byte_count": 0,
                "complete": False, "count": 0, "bytes": 0}
    root_count = int(after["root_count"]) + sum(int(summary["root_count"]) for summary in summaries)
    file_count = int(after["file_count"]) + sum(int(summary["file_count"]) for summary in summaries)
    byte_count = int(after["byte_count"]) + sum(int(summary["byte_count"]) for summary in summaries)
    return {
        "root_count": root_count,
        "file_count": file_count,
        "byte_count": byte_count,
        "complete": after.get("complete") is True,
        "count": root_count,
        "bytes": byte_count,
    }


def _case_result_verdict(rows: tuple[MatrixRow, ...], unresolved: bool) -> str:
    if unresolved or any(row.status == "FAIL" for row in rows):
        return "FAIL"
    if any(row.status == "PASS" for row in rows):
        return "PASS"
    return "BLOCKED"


def _aggregate_exit(status: str, result_verdict: str, require_pass: bool) -> int:
    if status == "FAIL":
        return 1
    if result_verdict == "BLOCKED" and require_pass:
        return 2
    return 0


def _validate_summary(value: object) -> None:
    if not isinstance(value, dict):
        raise EvidenceError("aggregate_summary_invalid")
    if (type(value.get("root_count")) is not int or type(value.get("file_count")) is not int
            or type(value.get("byte_count")) is not int or type(value.get("complete")) is not bool
            or type(value.get("count")) is not int or type(value.get("bytes")) is not int
            or min(value["root_count"], value["file_count"], value["byte_count"]) < 0
            or min(value["count"], value["bytes"]) < 0
            or value["bytes"] != value["byte_count"]):
        raise EvidenceError("aggregate_summary_invalid")


def _parse_optional_revision(value: str | None) -> str | None:
    if value is None or value == "":
        return None
    if not re.fullmatch(r"[0-9a-f]{40,64}", value):
        raise EvidenceError("source_revision_invalid")
    return value

def _lifecycle_summary(
        source: dict[str, object], cleanup: dict[str, object],
        cleanup_ok: bool, diagnostics_digest: str | None) -> dict[str, object]:
    natural_shutdown = source.get("natural_daemon_shutdown")
    lifecycle: dict[str, object] = {
        "state": "complete" if cleanup_ok and natural_shutdown is True else "unresolved",
        "cleanup_status": cleanup.get("status"),
        "supervisor_shutdown_observed": cleanup.get("supervisor_shutdown_observed"),
        "natural_daemon_shutdown": natural_shutdown if isinstance(natural_shutdown, bool) else None,
    }
    if diagnostics_digest is not None and HEX_DIGEST.fullmatch(diagnostics_digest):
        lifecycle["failure_diagnostics_sha256"] = diagnostics_digest
    return lifecycle

def _validate_case_fields(case: dict[str, object]) -> None:
    required = {"case_id", "provider", "permission", "scenario", "status", "exit_code",
                "evidence_status", "receipt_path", "receipt_sha256", "binary_identities"}
    if not required.issubset(case):
        raise EvidenceError("aggregate_case_fields_missing")
    if not isinstance(case["case_id"], str) or not isinstance(case["receipt_path"], str):
        raise EvidenceError("aggregate_case_fields_invalid")
    if not isinstance(case["status"], str) or case["status"] not in {"PASS", "BLOCKED", "FAIL"}:
        raise EvidenceError("aggregate_case_status_invalid")
    exit_code = case["exit_code"]
    if type(exit_code) is not int or exit_code < 0 or exit_code > 255:
        raise EvidenceError("aggregate_case_exit_invalid")
    if ((case["status"] == "PASS" and exit_code != 0)
            or (case["status"] == "BLOCKED" and exit_code != 2)
            or (case["status"] == "FAIL" and exit_code in {0, 2})):
        raise EvidenceError("aggregate_case_exit_mismatch")
    if (not isinstance(case["evidence_status"], str)
            or case["evidence_status"] not in {"RESOLVED", "UNRESOLVED"}):
        raise EvidenceError("aggregate_case_evidence_status_invalid")


def _validate_case_proof_identity(case: dict[str, object], value: dict[str, object]) -> dict[str, object]:
    proof = value.get("source_proof")
    required_proof = {
        "state", "source_receipt_sha256", "final_status", "gate", "provider", "permission", "mode", "scenario",
        "requested_options_state", "requested_model", "requested_effort",
        "task_ids", "task_session_bindings", "diagnostic_task_ids", "session_ids",
        "binary_identities", "lifecycle_proof",
    }
    if not isinstance(proof, dict) or not required_proof.issubset(proof):
        raise EvidenceError("aggregate_case_proof_missing")
    _validate_requested_options({
        key: proof[key]
        for key in ("requested_options_state", "requested_model", "requested_effort")
    })
    if (proof.get("final_status") != case["status"]
            or proof.get("provider") != case["provider"]
            or proof.get("permission") != case["permission"]
            or proof.get("mode") != case["permission"]
            or proof.get("scenario") != case["scenario"]
            or proof.get("gate") != "acceptance-native"):
        raise EvidenceError("aggregate_case_proof_mismatch")
    if (proof.get("source_receipt_sha256") is not None
            and HEX_DIGEST.fullmatch(str(proof.get("source_receipt_sha256"))) is None):
        raise EvidenceError("aggregate_case_source_digest_invalid")
    return proof


def _validate_case_sessions(proof: dict[str, object]) -> tuple[list[dict[str, str]], list[str], list[str], dict[str, object]]:
    bindings = proof.get("task_session_bindings")
    if (not isinstance(bindings, list) or len(bindings) > 64
            or any(not isinstance(binding, dict)
                   or set(binding) != {"alias", "task_id", "session_id"}
                   or not isinstance(binding.get("alias"), str)
                   or SAFE_TEXT.fullmatch(binding["alias"]) is None
                   or _safe_proof_text(binding.get("task_id")) is None
                   or _safe_proof_text(binding.get("session_id")) is None
                   for binding in bindings)):
        raise EvidenceError("aggregate_case_task_session_bindings_invalid")
    aliases = [binding["alias"] for binding in bindings]
    if len(set(aliases)) != len(aliases):
        raise EvidenceError("aggregate_case_task_session_bindings_invalid")
    expected_task_ids = sorted({binding["task_id"] for binding in bindings})
    if (not isinstance(proof.get("task_ids"), list)
            or proof["task_ids"] != expected_task_ids
            or len(expected_task_ids) != len(bindings)):
        raise EvidenceError("aggregate_case_task_ids_invalid")
    diagnostic_task_ids = proof.get("diagnostic_task_ids")
    if (not isinstance(diagnostic_task_ids, list) or len(diagnostic_task_ids) > 64
            or any(_safe_proof_text(item) is None for item in diagnostic_task_ids)
            or diagnostic_task_ids != sorted(set(diagnostic_task_ids))):
        raise EvidenceError("aggregate_case_diagnostic_task_ids_invalid")
    sessions = proof.get("session_ids")
    if (not isinstance(sessions, dict) or not isinstance(sessions.get("state"), str)
            or sessions.get("state") not in {"recorded", "not-recorded"}
            or not isinstance(sessions.get("values"), list)
            or not all(isinstance(item, str) and item for item in sessions["values"])
            or not isinstance(sessions.get("by_alias"), dict)
            or any(not isinstance(key, str) or not isinstance(item, str) or not item
                   for key, item in sessions["by_alias"].items())
            or sessions["by_alias"] != {
                binding["alias"]: binding["session_id"] for binding in bindings
            }
            or sorted(set(sessions["by_alias"].values())) != sessions["values"]
            or (sessions.get("state") == "recorded") != bool(bindings)):
        raise EvidenceError("aggregate_case_session_ids_invalid")
    return bindings, aliases, expected_task_ids, sessions


def _validate_case_binaries(case: dict[str, object], proof: dict[str, object]) -> tuple[dict[str, object], list[str]]:
    binaries = proof.get("binary_identities")
    if (not isinstance(binaries, dict) or not isinstance(binaries.get("state"), str)
            or binaries.get("state") not in {"complete", "unavailable"}
            or not isinstance(binaries.get("sha256"), dict)
            or any(not isinstance(key, str) or not isinstance(value, str)
                   or HEX_DIGEST.fullmatch(value) is None for key, value in binaries["sha256"].items())):
        raise EvidenceError("aggregate_case_binary_identity_invalid")
    binary_task_ids = binaries.get("task_ids")
    if (not isinstance(binary_task_ids, list) or len(binary_task_ids) > 64
            or any(not isinstance(task_id, str)
                   or re.fullmatch(r"[0-9a-f]{32}", task_id) is None
                   for task_id in binary_task_ids)
            or binary_task_ids != sorted(set(binary_task_ids))):
        raise EvidenceError("aggregate_case_binary_task_ids_invalid")
    provider_runtimes = binaries.get("provider_runtime_identities")
    if (not isinstance(provider_runtimes, dict) or len(provider_runtimes) > 64
            or any(not isinstance(task_id, str)
                   or re.fullmatch(r"[0-9a-f]{32}", task_id) is None
                   or not isinstance(identity, dict)
                   or set(identity) != {"executable", "version", "sha256"}
                   or _safe_runtime_executable(identity.get("executable")) is None
                   or _safe_proof_text(identity.get("version")) is None
                   or not isinstance(identity.get("sha256"), str)
                   or HEX_DIGEST.fullmatch(identity["sha256"]) is None
                   for task_id, identity in provider_runtimes.items())):
        raise EvidenceError("aggregate_case_provider_runtime_identity_invalid")
    provider_binding = binaries.get("provider_binding")
    if provider_binding is not None and (
            not isinstance(provider_binding, dict)
            or set(provider_binding) != {"task_id", "executable", "version", "sha256"}
            or not isinstance(provider_binding.get("task_id"), str)
            or re.fullmatch(r"[0-9a-f]{32}", provider_binding["task_id"]) is None
            or _safe_runtime_executable(provider_binding.get("executable")) is None
            or _safe_proof_text(provider_binding.get("version")) is None
            or not isinstance(provider_binding.get("sha256"), str)
            or HEX_DIGEST.fullmatch(provider_binding["sha256"]) is None):
        raise EvidenceError("aggregate_case_provider_binding_invalid")
    if binaries.get("state") == "complete" and not {
            "provider_sha256", "delegate_sha256", "runner_sha256", "pueue_sha256", "pueued_sha256",
    }.issubset(binaries["sha256"]):
        raise EvidenceError("aggregate_case_binary_identity_incomplete")
    if binaries.get("state") == "complete" and (
            provider_binding is None
            or sorted(provider_runtimes) != binary_task_ids
            or provider_runtimes.get(provider_binding["task_id"]) != {
                "executable": provider_binding["executable"],
                "version": provider_binding["version"],
                "sha256": provider_binding["sha256"],
            }
            or binaries["sha256"].get("provider_sha256") != provider_binding["sha256"]):
        raise EvidenceError("aggregate_case_provider_runtime_coverage_invalid")
    if case.get("binary_identities") != binaries:
        raise EvidenceError("aggregate_case_binary_identity_mismatch")
    return binaries, binary_task_ids


def _validate_case_completion(
        case: dict[str, object], proof: dict[str, object],
        session_facts: tuple[list[dict[str, str]], list[str], list[str], dict[str, object]],
        binary_facts: tuple[dict[str, object], list[str]]) -> None:
    bindings, aliases, expected_task_ids, sessions = session_facts
    binaries, binary_task_ids = binary_facts
    lifecycle = proof.get("lifecycle_proof")
    if (not isinstance(lifecycle, dict) or not isinstance(lifecycle.get("state"), str)
            or lifecycle.get("state") not in {"complete", "unresolved"}):
        raise EvidenceError("aggregate_case_lifecycle_invalid")
    expected_aliases = set(_expected_task_aliases(case["provider"], case["scenario"]))
    if case["status"] == "PASS" and case["evidence_status"] == "RESOLVED" and (
            proof.get("state") != "complete" or set(aliases) != expected_aliases
            or sessions.get("state") != "recorded" or not sessions["values"]
            or binary_task_ids != expected_task_ids
            or HEX_DIGEST.fullmatch(str(proof.get("source_receipt_sha256", ""))) is None
            or binaries.get("state") != "complete" or lifecycle.get("state") != "complete"):
        raise EvidenceError("aggregate_case_proof_incomplete")
    if (case["status"] == "PASS" and case["evidence_status"] == "RESOLVED"
            and proof.get("requested_options_state") != "recorded"):
        raise EvidenceError("aggregate_case_requested_options_incomplete")
    if (case["status"] == "PASS" and case["evidence_status"] == "RESOLVED"
            and len(bindings) > 1
            and len({binding["session_id"] for binding in bindings}) != 1):
        raise EvidenceError("aggregate_case_session_continuity_invalid")
    if case["status"] == "BLOCKED" and proof.get("state") != "blocked-before-runtime":
        raise EvidenceError("aggregate_blocked_proof_scope_invalid")
    if case["status"] == "FAIL" and proof.get("state") != "unresolved":
        raise EvidenceError("aggregate_failure_proof_scope_invalid")


@dataclass(frozen=True)
class _CaseObservation:
    directory: Path
    source: dict[str, object] | None
    source_digest: str | None
    cleanup: dict[str, object]
    proof: dict[str, object]
    creation_identity: dict[str, object] | None
    error: str | None


def _source_proof_error(
        row: MatrixRow, source: dict[str, object] | None, proof: dict[str, object],
        evidence_error: str | None) -> str | None:
    if source is None and evidence_error is None:
        evidence_error = "source_receipt_missing"
    if (proof.get("gate") != "acceptance-native"
            or proof.get("provider") != row.definition.provider
            or proof.get("permission") != row.definition.permission
            or proof.get("mode") != row.definition.permission
            or proof.get("scenario") != row.definition.scenario):
        evidence_error = evidence_error or "source_proof_identity_mismatch"
    requested_options_state = proof.get("requested_options_state")
    if requested_options_state != "recorded":
        evidence_error = evidence_error or (
            "source_requested_options_missing"
            if requested_options_state == "missing"
            else "source_requested_options_invalid"
        )
    if row.status == "PASS" and proof.get("state") != "complete":
        evidence_error = evidence_error or "source_proof_incomplete"
    return evidence_error


def _unresolved_source_proof(row: MatrixRow, source_digest: str | None) -> dict[str, object]:
    return {
        "state": "unresolved", "final_status": row.status,
        "source_receipt_sha256": source_digest,
        "source_status": None, "gate": "acceptance-native",
        "provider": row.definition.provider, "permission": row.definition.permission,
        "mode": row.definition.permission, "scenario": row.definition.scenario,
        "requested_options_state": "missing", "requested_model": None,
        "requested_effort": None,
        "task_ids": [], "task_session_bindings": [], "diagnostic_task_ids": [],
        "session_ids": {
            "state": "not-recorded", "values": [], "by_alias": {},
        },
        "binary_identities": {
            "state": "unavailable", "sha256": {}, "task_ids": [],
            "provider_binding": None, "provider_runtime_identities": {},
        },
        "lifecycle_proof": {"state": "unresolved"},
    }


def _validate_aggregate_result(
        value: dict[str, object], cases: list[dict[str, object]]) -> int:
    statuses = [case["status"] for case in cases]
    if (value.get("case_count") != len(cases)
            or value.get("pass_count") != statuses.count("PASS")
            or value.get("blocked_count") != statuses.count("BLOCKED")
            or value.get("failed_count") != statuses.count("FAIL")):
        raise EvidenceError("aggregate_count_mismatch")
    evidence_unresolved = any(case["evidence_status"] == "UNRESOLVED" for case in cases)
    expected_unresolved = [case["case_id"] for case in cases if case["evidence_status"] == "UNRESOLVED"]
    unresolved_cases = value.get("unresolved_cases")
    unresolved_count = value.get("unresolved_case_count")
    if (not isinstance(unresolved_cases, list) or type(unresolved_count) is not int
            or unresolved_count != len(unresolved_cases)
            or any(not isinstance(item, str) for item in unresolved_cases)
            or len(set(unresolved_cases)) != len(unresolved_cases)
            or not set(expected_unresolved).issubset(
                set(item for item in unresolved_cases if isinstance(item, str)))):
        raise EvidenceError("aggregate_unresolved_count_mismatch")
    computed = _case_result_verdict(tuple(
        MatrixRow(CaseDefinition(
            str(case["case_id"]), str(case["provider"]), str(case["permission"]),
            str(case["scenario"]), ""), int(case["exit_code"]), str(case["status"]),
        ) for case in cases
    ), evidence_unresolved)
    if value.get("case_results_verdict") != computed:
        raise EvidenceError("aggregate_result_mismatch")
    status = value.get("status")
    if not isinstance(status, str) or status not in {"PASS", "BLOCKED", "FAIL"}:
        raise EvidenceError("aggregate_status_invalid")
    if status == "FAIL" and computed != "FAIL" and value.get("unresolved_case_count", 0) == 0:
        raise EvidenceError("aggregate_result_mismatch")
    if status in {"PASS", "BLOCKED"} and (computed != status or value.get("unresolved_case_count") != 0):
        raise EvidenceError("aggregate_result_mismatch")
    policy = value.get("policy")
    if not isinstance(policy, dict) or type(policy.get("require_at_least_one_pass")) is not bool:
        raise EvidenceError("aggregate_policy_invalid")
    expected_exit = _aggregate_exit(str(status), computed, bool(policy["require_at_least_one_pass"]))
    if value.get("exit_code") != expected_exit:
        raise EvidenceError("aggregate_exit_mismatch")
    if value.get("blocked_scope") != "authentication-or-prerequisite-only":
        raise EvidenceError("aggregate_blocked_scope_invalid")
    return unresolved_count


def _validate_aggregate_evidence(value: dict[str, object], unresolved_count: int) -> None:
    evidence = value.get("evidence")
    if (not isinstance(evidence, dict) or not isinstance(evidence.get("case_raw"), dict)
            or not isinstance(evidence.get("compact_receipts"), dict)):
        raise EvidenceError("aggregate_evidence_invalid")
    _validate_summary(evidence["case_raw"].get("before"))
    _validate_summary(evidence["case_raw"].get("after"))
    _validate_summary(evidence["compact_receipts"])
    raw_after = evidence["case_raw"]["after"]
    if (unresolved_count == 0 and (
            raw_after.get("complete") is not True
            or raw_after.get("file_count") != 0
            or raw_after.get("byte_count") != 0)):
        raise EvidenceError("aggregate_case_raw_cleanup_incomplete")
