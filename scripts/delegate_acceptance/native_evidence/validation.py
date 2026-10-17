"""Validation for native acceptance evidence."""

from __future__ import annotations

from pathlib import Path
from ..directories.journal import cleanup_journal_digest
from ..directories.journal import read_directory_cleanup
from .contract import _validate_aggregate_result, _validate_aggregate_evidence, _validate_case_fields, _validate_case_proof_identity, _validate_case_sessions, _validate_case_binaries, _validate_case_completion, AGGREGATE_KIND, CASE_ROOT_NAME, COMPACT_RECEIPT_ROOT_NAME, EXPECTED_CASES, EvidenceError, HEX_DIGEST, MAX_AGGREGATE_BYTES, MAX_RECEIPT_BYTES, MAX_RETENTION_COUNT, RETENTION_CLEANUP_ROOT_NAME, SCHEMA_VERSION, _case_cleanup_authorization, _digest_bytes, _empty_summary, _validate_summary
from .filesystem import (
    _digest_file,
    _identity_matches,
    _inside,
    _read_json,
)
from .retention_records import (
    _current_retention_entry,
)


def _validate_case_receipt(root: Path, case: dict[str, object]) -> None:
    _validate_case_fields(case)
    receipt_path = _inside(root, Path(case["receipt_path"]))
    if not HEX_DIGEST.fullmatch(str(case["receipt_sha256"])):
        raise EvidenceError("aggregate_case_digest_invalid")
    if _digest_file(receipt_path) != case["receipt_sha256"]:
        raise EvidenceError("aggregate_case_digest_mismatch")
    value, _ = _read_json(receipt_path, MAX_RECEIPT_BYTES)
    if not isinstance(value, dict) or value.get("kind") != "native-acceptance-case":
        raise EvidenceError("aggregate_case_receipt_invalid")
    for key in ("case_id", "provider", "permission", "scenario", "status", "exit_code"):
        if value.get(key) != case[key]:
            raise EvidenceError("aggregate_case_receipt_mismatch")
    if value.get("evidence_status") != case["evidence_status"]:
        raise EvidenceError("aggregate_case_receipt_mismatch")
    proof = _validate_case_proof_identity(case, value)
    sessions = _validate_case_sessions(proof)
    binaries = _validate_case_binaries(case, proof)
    _validate_case_completion(case, proof, sessions, binaries)
    _validate_case_cleanup(root, value)


def _validate_case_cleanup(root: Path, value: dict[str, object]) -> None:
    identity = value.get("creation_identity")
    if value.get("evidence_status") == "RESOLVED":
        cleanup = value.get("cleanup")
        raw_after = cleanup.get("raw_after") if isinstance(cleanup, dict) else None
        _validate_summary(raw_after)
        if (not isinstance(identity, dict)
                or not isinstance(identity.get("path"), str)
                or not isinstance(identity.get("parent"), str)
                or identity.get("parent") != str((root / CASE_ROOT_NAME).absolute())
                or any(type(identity.get(key)) is not int for key in
                       ("device", "inode", "parent_device", "parent_inode"))
                or raw_after != _empty_summary()
                or value.get("cleanup", {}).get("raw_deleted") is not True
                or value.get("terminal_state") != "published-after-cleanup"):
            raise EvidenceError("aggregate_case_cleanup_identity_invalid")
        deletion = value.get("raw_deletion")
        if (not isinstance(deletion, dict)
                or deletion.get("authority") != "DirectoryCleanup"
                or deletion.get("authorization_sha256") != _case_cleanup_authorization(value)
                or not isinstance(deletion.get("journal_path"), str)
                or not isinstance(deletion.get("journal_sha256"), str)
                or HEX_DIGEST.fullmatch(deletion["journal_sha256"]) is None):
            raise EvidenceError("aggregate_case_cleanup_journal_invalid")
        journal = _inside(
            root / COMPACT_RECEIPT_ROOT_NAME,
            Path(deletion["journal_path"]),
        )
        try:
            cleanup_record = read_directory_cleanup(
                journal, deletion["authorization_sha256"],
            )
            journal_digest = cleanup_journal_digest(journal)
        except (OSError, RuntimeError, ValueError) as error:
            raise EvidenceError("aggregate_case_cleanup_journal_invalid") from error
        if (cleanup_record.get("state") != "complete"
                or cleanup_record.get("target") != identity
                or journal_digest != deletion["journal_sha256"]):
            raise EvidenceError("aggregate_case_cleanup_journal_invalid")


def _validate_retention(
        value: object,
        aggregate_root: Path,
        aggregate_receipt: dict[str, object]) -> None:
    aggregate_owner = aggregate_receipt.get("aggregate_owner")
    if (not isinstance(value, dict)
            or type(value.get("configured_count")) is not int
            or not 1 <= value["configured_count"] <= MAX_RETENTION_COUNT
            or value.get("status") != "complete"
            or value.get("errors") != []
            or not isinstance(value.get("pruned"), list)
            or not isinstance(value.get("plan_path"), str)
            or not isinstance(value.get("plan_sha256"), str)
            or HEX_DIGEST.fullmatch(value["plan_sha256"]) is None):
        raise EvidenceError("aggregate_retention_incomplete")
    before = value.get("before")
    after = value.get("after")
    _validate_summary(before)
    _validate_summary(after)
    if (before["complete"] is not True or after["complete"] is not True
            or before["count"] != before["root_count"]
            or after["count"] != after["root_count"]
            or after["root_count"] > value["configured_count"]
            or after["root_count"] > before["root_count"]
            or after["file_count"] > before["file_count"]
            or after["byte_count"] > before["byte_count"]):
        raise EvidenceError("aggregate_retention_incomplete")
    plan_path = _inside(aggregate_root, Path(value["plan_path"]))
    if _digest_file(plan_path, MAX_RECEIPT_BYTES) != value["plan_sha256"]:
        raise EvidenceError("aggregate_retention_plan_invalid")
    plan_value, _ = _read_json(plan_path, MAX_RECEIPT_BYTES)
    if (not isinstance(plan_value, dict)
            or plan_value.get("schema_version") != SCHEMA_VERSION
            or plan_value.get("kind") != "native-acceptance-retention-plan"
            or plan_value.get("state") != "complete"
            or plan_value.get("configured_count") != value["configured_count"]
            or plan_value.get("current_root") != str(aggregate_root.absolute())
            or plan_value.get("current_owner") != aggregate_owner
            or plan_value.get("current_counted") != (
                _current_retention_entry(aggregate_root, aggregate_receipt) is not None
            )
            or plan_value.get("current_order") != (
                aggregate_receipt.get("completed_at_ns")
                if plan_value.get("current_counted") is True else None
            )
            or plan_value.get("result") != {
                key: item for key, item in value.items()
                if key not in {"plan_path", "plan_sha256"}
            }):
        raise EvidenceError("aggregate_retention_plan_invalid")
    victims = plan_value.get("victims")
    if not isinstance(victims, list):
        raise EvidenceError("aggregate_retention_plan_invalid")
    for victim in victims:
        if (not isinstance(victim, dict)
                or not isinstance(victim.get("authorization_sha256"), str)
                or not isinstance(victim.get("journal_path"), str)
                or not isinstance(victim.get("journal_sha256"), str)):
            raise EvidenceError("aggregate_retention_plan_invalid")
        journal = _inside(
            aggregate_root / RETENTION_CLEANUP_ROOT_NAME,
            Path(victim["journal_path"]),
        )
        try:
            record = read_directory_cleanup(journal, victim["authorization_sha256"])
            digest = cleanup_journal_digest(journal)
        except (OSError, RuntimeError, ValueError) as error:
            raise EvidenceError("aggregate_retention_plan_invalid") from error
        if (record.get("state") != "complete" or record.get("target") != victim.get("identity")
                or digest != victim["journal_sha256"]):
            raise EvidenceError("aggregate_retention_plan_invalid")


def _validate_aggregate_cases(
        aggregate_root: Path, value: dict[str, object]) -> list[dict[str, object]]:
    cases = value.get("cases")
    if not isinstance(cases, list) or len(cases) != len(EXPECTED_CASES):
        raise EvidenceError("aggregate_matrix_incomplete")
    seen: set[str] = set()
    expected_by_id = {case.case_id: case for case in EXPECTED_CASES}
    for case in cases:
        if not isinstance(case, dict):
            raise EvidenceError("aggregate_case_invalid")
        _validate_case_receipt(aggregate_root, case)
        case_id = case["case_id"]
        if not isinstance(case_id, str) or case_id in seen:
            raise EvidenceError("aggregate_case_duplicate")
        expected = expected_by_id.get(case_id)
        if (expected is None or case.get("provider") != expected.provider
                or case.get("permission") != expected.permission
                or case.get("scenario") != expected.scenario):
            raise EvidenceError("aggregate_matrix_incomplete")
        seen.add(case_id)
    if seen != {case.case_id for case in EXPECTED_CASES}:
        raise EvidenceError("aggregate_matrix_incomplete")
    if value.get("matrix_complete") is not True or value.get("expected_case_count") != len(EXPECTED_CASES):
        raise EvidenceError("aggregate_matrix_incomplete")
    return cases


def validate_aggregate_receipt(
        path: Path,
        *,
        expected_tag: str | None = None,
        expected_source_revision: str | None = None) -> tuple[dict[str, object], str]:
    value, data = _read_json(path, MAX_AGGREGATE_BYTES)
    if not isinstance(value, dict) or value.get("schema_version") != SCHEMA_VERSION:
        raise EvidenceError("aggregate_receipt_invalid")
    if value.get("kind") != AGGREGATE_KIND or value.get("claim") != "native-acceptance-case-results":
        raise EvidenceError("aggregate_receipt_claim_invalid")
    if value.get("claims_scope") != "provider-permission-scenario-case-results-only":
        raise EvidenceError("aggregate_receipt_scope_invalid")
    if expected_tag is not None and value.get("tag") != expected_tag:
        raise EvidenceError("aggregate_tag_mismatch")
    if expected_source_revision is not None and value.get("source_revision") != expected_source_revision:
        raise EvidenceError("aggregate_source_revision_mismatch")
    aggregate_root_value = value.get("aggregate_root")
    if not isinstance(aggregate_root_value, str) or ".." in Path(aggregate_root_value).parts:
        raise EvidenceError("aggregate_root_invalid")
    aggregate_root = Path(aggregate_root_value).resolve()
    if aggregate_root != path.resolve().parent:
        raise EvidenceError("aggregate_root_mismatch")
    if not _identity_matches(aggregate_root, value.get("aggregate_owner")):
        raise EvidenceError("aggregate_owner_invalid")
    if value.get("terminal_evidence_published") is not True:
        raise EvidenceError("aggregate_terminal_evidence_missing")
    if value.get("harness_revision") != value.get("source_revision"):
        raise EvidenceError("aggregate_harness_revision_mismatch")
    cases = _validate_aggregate_cases(aggregate_root, value)
    unresolved_count = _validate_aggregate_result(value, cases)
    _validate_aggregate_evidence(value, unresolved_count)
    _validate_retention(value.get("retention"), aggregate_root, value)
    return value, _digest_bytes(data)
