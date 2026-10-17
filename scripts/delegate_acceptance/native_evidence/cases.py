"""Cases for native acceptance evidence."""

from __future__ import annotations

from pathlib import Path
from ..directories.journal import cleanup_journal_digest
from ..directories.journal import prepare_directory_cleanup
from ..directories.journal import read_directory_cleanup
from ..directories.journal import resume_directory_cleanup
from .contract import (
    EvidenceError,
    HEX_DIGEST,
    MAX_INPUT_BYTES,
    MAX_RECEIPT_BYTES,
    MatrixRow,
    SCHEMA_VERSION,
    _case_cleanup_authorization,
    _compact_provider_versions,
    _digest_bytes,
    _empty_summary,
    _source_status_matches,
)
from .filesystem import (
    _identity_shape,
    _inside,
    _read_json,
    _write_atomic,
)
from .validation import (
    _validate_case_receipt,
)


def _sanitize_case(
        *, row: MatrixRow,
        source: dict[str, object] | None,
        source_digest: str | None,
        cleanup: dict[str, object],
        before: dict[str, object],
        after: dict[str, object],
        evidence_status: str,
        evidence_error: str | None,
        source_proof: dict[str, object],
        creation_identity: dict[str, object] | None,
        terminal_state: str,
        raw_deleted: bool) -> dict[str, object]:
    result: dict[str, object] = {
        "schema_version": SCHEMA_VERSION,
        "kind": "native-acceptance-case",
        "gate": "acceptance-native",
        "scope": "provider-permission-scenario-case-result",
        "case_id": row.definition.case_id,
        "provider": row.definition.provider,
        "permission": row.definition.permission,
        "scenario": row.definition.scenario,
        "status": row.status,
        "exit_code": row.exit_code,
        "evidence_status": evidence_status,
        "credentials_in_receipt": False,
        "source_receipt_sha256": source_digest,
        "cleanup": {"observation": cleanup, "raw_before": before, "raw_after": after,
                     "raw_deleted": raw_deleted},
        "source_proof": source_proof,
        "terminal_state": terminal_state,
    }
    if creation_identity is not None:
        result["creation_identity"] = creation_identity
    if row.status == "BLOCKED":
        result.update({
            "blocked_scope": "authentication-or-prerequisite",
            "provider_execution_claim": "none",
        })
    elif row.status == "PASS":
        result["provider_execution_claim"] = "this-case-result-only"
        if source is not None:
            versions = _compact_provider_versions(source)
            if versions is not None:
                result["provider_versions"] = versions
            natural_shutdown = source.get("natural_daemon_shutdown")
            if isinstance(natural_shutdown, bool):
                result["natural_daemon_shutdown"] = natural_shutdown
    else:
        result["provider_execution_claim"] = "unresolved"
    if evidence_error is not None:
        result["evidence_error"] = evidence_error
    return result


def _case_reference(
        row: MatrixRow,
        evidence_status: str,
        compact_path: Path,
        digest: str,
        compact: dict[str, object]) -> dict[str, object]:
    proof = compact.get("source_proof")
    binaries = proof.get("binary_identities") if isinstance(proof, dict) else None
    return {
        "case_id": row.definition.case_id,
        "provider": row.definition.provider,
        "permission": row.definition.permission,
        "scenario": row.definition.scenario,
        "status": row.status,
        "exit_code": row.exit_code,
        "evidence_status": evidence_status,
        "receipt_path": str(compact_path.resolve()),
        "receipt_sha256": digest,
        "binary_identities": binaries,
    }


def _read_case_source(case_directory: Path, status: str) -> tuple[dict[str, object] | None, str | None, str | None]:
    source_name = "success.json" if status == "PASS" else "failure.json"
    source_path = case_directory / source_name
    try:
        value, data = _read_json(source_path, MAX_INPUT_BYTES)
    except EvidenceError as error:
        return None, None, error.args[0] if error.args else "source_receipt_invalid"
    if not isinstance(value, dict):
        return None, _digest_bytes(data), "source_receipt_malformed"
    if not _source_status_matches(value, status):
        return value, _digest_bytes(data), "source_status_mismatch"
    return value, _digest_bytes(data), None


def _case_cleanup_journal(compact_path: Path) -> Path:
    return compact_path.with_name(compact_path.stem + ".cleanup.json")


def _prepare_case_cleanup(
        compact_path: Path,
        prepared: dict[str, object],
        cleanup_root: Path,
        expected_identity: dict[str, object]) -> None:
    journal = _case_cleanup_journal(compact_path)
    authorization = _case_cleanup_authorization(prepared)
    prepared["raw_deletion"] = {
        "authority": "DirectoryCleanup",
        "authorization_sha256": authorization,
        "journal_path": str(journal.resolve()),
    }
    prepared["receipt_path"] = str(compact_path.resolve())
    _write_atomic(compact_path, prepared)
    try:
        prepare_directory_cleanup(
            journal,
            "native acceptance case " + str(prepared.get("case_id", "unknown")),
            expected_identity,
            cleanup_root,
            authorization,
        )
    except (OSError, RuntimeError, ValueError) as error:
        raise EvidenceError("case_cleanup_prepare_failed") from error


def _replay_prepared_case(
        compact_path: Path,
        case_directory: Path,
        cases_root: Path,
        cleanup_root: Path,
        row: MatrixRow) -> dict[str, object] | None:
    """Resume the exact identity-pinned cleanup described by a durable marker."""
    try:
        value, _ = _read_json(compact_path, MAX_RECEIPT_BYTES)
    except EvidenceError:
        return None
    if (not isinstance(value, dict)
            or value.get("case_id") != row.definition.case_id
            or value.get("status") != row.status
            or value.get("exit_code") != row.exit_code
            or value.get("evidence_status") != "RESOLVED"
            or value.get("terminal_state") != "published-before-cleanup"):
        return None
    deletion = value.get("raw_deletion")
    identity = value.get("creation_identity")
    if (not isinstance(deletion, dict)
            or deletion.get("authority") != "DirectoryCleanup"
            or not _identity_shape(identity, case_directory, cases_root)
            or deletion.get("authorization_sha256") != _case_cleanup_authorization(value)):
        return None
    authorization = deletion.get("authorization_sha256")
    journal_value = deletion.get("journal_path")
    if (not isinstance(authorization, str) or HEX_DIGEST.fullmatch(authorization) is None
            or not isinstance(journal_value, str)):
        return None
    try:
        journal = _inside(compact_path.parent, Path(journal_value))
    except EvidenceError:
        return None
    cleanup = value.get("cleanup")
    if not isinstance(cleanup, dict):
        return None
    try:
        if not journal.exists() and not journal.is_symlink():
            prepare_directory_cleanup(
                journal,
                "native acceptance case " + row.definition.case_id,
                identity,
                cleanup_root,
                authorization,
            )
        else:
            existing = read_directory_cleanup(journal, authorization)
            if existing.get("target") != identity:
                raise RuntimeError("case cleanup target identity changed")
        completed = resume_directory_cleanup(journal, authorization, identity)
        if completed.get("state") != "complete" or completed.get("target") != identity:
            raise RuntimeError("case cleanup did not reach its terminal state")
        journal_digest = cleanup_journal_digest(journal)
    except (OSError, RuntimeError, ValueError) as error:
        raise EvidenceError("case_cleanup_journal_invalid") from error

    final = dict(value)
    final_cleanup = dict(cleanup)
    final_cleanup["raw_after"] = _empty_summary()
    final_cleanup["raw_deleted"] = True
    final["cleanup"] = final_cleanup
    final["terminal_state"] = "published-after-cleanup"
    final["raw_deletion"] = {
        **deletion,
        "journal_sha256": journal_digest,
    }
    final_bytes = _write_atomic(compact_path, final)
    final_digest = _digest_bytes(final_bytes)
    reference = _case_reference(row, "RESOLVED", compact_path, final_digest, final)
    _validate_case_receipt(case_directory.parent.parent, reference)
    return reference
