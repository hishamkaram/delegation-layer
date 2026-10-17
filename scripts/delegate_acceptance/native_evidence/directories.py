"""Directories for native acceptance evidence."""

from __future__ import annotations

from ..directories.operations import DirectoryOwner
from pathlib import Path
from ..directories.journal import directory_identity_record
from .contract import (
    CASE_OWNER_ROOT_NAME,
    CASE_ROOT_NAME,
    CaseDefinition,
    EXPECTED_CASES,
    EvidenceError,
    MAX_RECEIPT_BYTES,
    MatrixRow,
    SCHEMA_VERSION,
)
from .filesystem import (
    _bounded_children,
    _ensure_directory,
    _identity_matches,
    _identity_shape,
    _read_json,
    _reject_path_components,
    _write_atomic,
)


def prepare_case_directory(aggregate_root: Path, case_id: str) -> Path:
    """Create one case and persist its ownership identity before provider work."""
    aggregate_root = aggregate_root.absolute()
    _reject_path_components(aggregate_root)
    _ensure_directory(aggregate_root)
    aggregate_root = aggregate_root.resolve()
    cases_root = aggregate_root / CASE_ROOT_NAME
    _ensure_directory(cases_root)
    definition = next((case for case in EXPECTED_CASES if case.case_id == case_id), None)
    if definition is None:
        raise EvidenceError("case_owner_case_unknown")
    owners_root = aggregate_root / CASE_OWNER_ROOT_NAME
    owners_root.mkdir(mode=0o700, exist_ok=True)
    _ensure_directory(owners_root)
    case_directory = aggregate_root / definition.case_output
    owner_receipt = owners_root / f"{case_id}.json"
    if (case_directory.exists() or case_directory.is_symlink()
            or owner_receipt.exists() or owner_receipt.is_symlink()):
        raise EvidenceError("case_owner_already_exists")
    owner = DirectoryOwner()
    token = owner.create("native acceptance case", case_directory, cases_root)
    identity = directory_identity_record(token)
    try:
        _write_atomic(owner_receipt, {
            "schema_version": SCHEMA_VERSION,
            "kind": "native-acceptance-case-owner",
            "case_id": case_id,
            "provider": definition.provider,
            "permission": definition.permission,
            "scenario": definition.scenario,
            "directory_identity": identity,
        })
    except BaseException:
        owner.rollback()
        raise
    owner.release()
    return case_directory


def _case_owner_identity(
        aggregate_root: Path, definition: CaseDefinition) -> dict[str, object]:
    receipt_path = aggregate_root / CASE_OWNER_ROOT_NAME / f"{definition.case_id}.json"
    value, _ = _read_json(receipt_path, MAX_RECEIPT_BYTES)
    case_directory = aggregate_root / definition.case_output
    cases_root = aggregate_root / CASE_ROOT_NAME
    identity = value.get("directory_identity") if isinstance(value, dict) else None
    if (not isinstance(value, dict)
            or value.get("schema_version") != SCHEMA_VERSION
            or value.get("kind") != "native-acceptance-case-owner"
            or value.get("case_id") != definition.case_id
            or value.get("provider") != definition.provider
            or value.get("permission") != definition.permission
            or value.get("scenario") != definition.scenario
            or not _identity_shape(identity, case_directory, cases_root)):
        raise EvidenceError("case_owner_receipt_invalid")
    return identity


def validate_prepared_case_directory(
        case_directory: Path, provider: str, permission: str, scenario: str) -> Path:
    """Validate an empty aggregate-owned case directory before its one case run."""
    case_directory = case_directory.absolute()
    _reject_path_components(case_directory)
    aggregate_root = case_directory.parent.parent
    cases_root = aggregate_root / CASE_ROOT_NAME
    if case_directory.parent != cases_root:
        raise EvidenceError("prepared_case_path_invalid")
    definition = next(
        (case for case in EXPECTED_CASES if case.case_id == case_directory.name), None)
    if (definition is None or definition.provider != provider
            or definition.permission != permission or definition.scenario != scenario
            or aggregate_root / definition.case_output != case_directory):
        raise EvidenceError("prepared_case_definition_mismatch")
    _ensure_directory(aggregate_root)
    _ensure_directory(cases_root)
    _ensure_directory(case_directory)
    identity = _case_owner_identity(aggregate_root, definition)
    if not _identity_matches(case_directory, identity):
        raise EvidenceError("prepared_case_identity_changed")
    if _bounded_children(case_directory):
        raise EvidenceError("prepared_case_directory_not_empty")
    return case_directory.resolve(strict=True)


def _case_creation_identity(aggregate_root: Path, row: MatrixRow) -> dict[str, object]:
    return _case_owner_identity(aggregate_root, row.definition)
