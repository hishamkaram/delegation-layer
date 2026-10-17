"""Aggregate for native acceptance evidence."""

from __future__ import annotations

from pathlib import Path
from ..directories.journal import acquire_exclusive_file_lock
import re
from ..directories.journal import release_exclusive_file_lock
import stat
import time
from .cases import (
    _case_reference,
    _prepare_case_cleanup,
    _read_case_source,
    _replay_prepared_case,
    _sanitize_case,
)
from .contract import (
    _CaseObservation,
    _source_proof_error,
    _unresolved_source_proof,
    AGGREGATE_KIND,
    CASE_CLEANUP_ROOT_NAME,
    CASE_OWNER_ROOT_NAME,
    CASE_ROOT_NAME,
    COMPACT_RECEIPT_ROOT_NAME,
    CaseDefinition,
    EXPECTED_CASES,
    EvidenceError,
    MATRIX_HEADER,
    MAX_AGGREGATE_BYTES,
    MAX_MATRIX_BYTES,
    MAX_RECEIPT_BYTES,
    MatrixRow,
    SCHEMA_VERSION,
    _aggregate_exit,
    _case_result_verdict,
    _digest_bytes,
    _empty_summary,
)
from .directories import (
    _case_creation_identity,
)
from .filesystem import (
    _bounded_file_bytes,
    _digest_file,
    _directory_identity,
    _ensure_directory,
    _identity_matches,
    _inside,
    _read_json,
    _reject_path_components,
    _tree_summary,
    _write_atomic,
)
from .proof import (
    _cleanup_observation,
    _source_proof,
)
from .retention import (
    _enforce_retention,
    _publish_completed_retention_receipt,
)
from .retention_records import (
    _retention_count,
)
from .validation import (
    validate_aggregate_receipt,
)


def _parse_matrix(path: Path) -> tuple[MatrixRow, ...]:
    try:
        metadata = path.lstat()
        if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode):
            raise EvidenceError("case_matrix_invalid")
        if metadata.st_size > MAX_MATRIX_BYTES:
            raise EvidenceError("case_matrix_too_large")
        raw = _bounded_file_bytes(path, MAX_MATRIX_BYTES).decode("ascii")
    except EvidenceError:
        raise
    except (OSError, UnicodeError) as error:
        raise EvidenceError("case_matrix_invalid") from error
    lines = raw.splitlines(keepends=True)
    if not lines or lines[0] != MATRIX_HEADER:
        raise EvidenceError("case_matrix_header_invalid")
    rows: list[MatrixRow] = []
    for line in lines[1:]:
        if not line.endswith("\n"):
            raise EvidenceError("case_matrix_line_invalid")
        fields = line[:-1].split("\t")
        if len(fields) != 7 or any(not field or any(ord(char) < 32 for char in field) for field in fields):
            raise EvidenceError("case_matrix_line_invalid")
        case_id, provider, permission, scenario, raw_exit, status, case_output = fields
        if status not in {"PASS", "BLOCKED", "FAIL"}:
            raise EvidenceError("case_matrix_status_invalid")
        if not re.fullmatch(r"[0-9]{1,3}", raw_exit):
            raise EvidenceError("case_matrix_exit_invalid")
        exit_code = int(raw_exit)
        if (exit_code > 255 or (status == "PASS" and exit_code != 0)
                or (status == "BLOCKED" and exit_code != 2)
                or (status == "FAIL" and exit_code in {0, 2})):
            raise EvidenceError("case_matrix_exit_mismatch")
        definition = CaseDefinition(case_id, provider, permission, scenario, case_output)
        rows.append(MatrixRow(definition, exit_code, status))
    if len(rows) != len(EXPECTED_CASES):
        raise EvidenceError("case_matrix_incomplete")
    for actual, expected in zip(rows, EXPECTED_CASES):
        if actual.definition != expected:
            raise EvidenceError("case_matrix_incomplete")
    return tuple(rows)


def build_aggregate(
        aggregate_root: Path,
        matrix_path: Path,
        receipt_path: Path,
        *,
        tag: str | None = None,
        source_revision: str | None = None,
        require_pass: bool = False,
        retention_count: int | None = None) -> tuple[dict[str, object], int]:
    """Build one aggregate while holding its single-writer ownership lock."""
    root = aggregate_root.absolute()
    lock_path = root.parent / ("." + root.name + ".lock")
    retention_lock_fd = acquire_exclusive_file_lock(
        root.parent / ".native-acceptance-retention.lock",
    )
    lock_fd = -1
    try:
        lock_fd = acquire_exclusive_file_lock(lock_path)
        return _build_aggregate_locked(
            aggregate_root,
            matrix_path,
            receipt_path,
            tag=tag,
            source_revision=source_revision,
            require_pass=require_pass,
            retention_count=retention_count,
        )
    finally:
        if lock_fd >= 0:
            release_exclusive_file_lock(lock_fd)
        release_exclusive_file_lock(retention_lock_fd)


def _existing_aggregate(
        aggregate_root: Path, receipt_path: Path, tag: str | None,
        source_revision: str | None, require_pass: bool) -> dict[str, object] | None:
    existing_nonterminal: dict[str, object] | None = None
    if receipt_path.exists() or receipt_path.is_symlink():
        if receipt_path.is_symlink():
            raise EvidenceError("aggregate_receipt_invalid")
        existing_value, _ = _read_json(receipt_path, MAX_AGGREGATE_BYTES)
        existing_policy = (
            existing_value.get("policy") if isinstance(existing_value, dict) else None
        )
        if (not isinstance(existing_value, dict)
                or existing_value.get("kind") != AGGREGATE_KIND
                or existing_value.get("claim") != "native-acceptance-case-results"
                or existing_value.get("aggregate_root") != str(aggregate_root)
                or not _identity_matches(aggregate_root, existing_value.get("aggregate_owner"))
                or existing_value.get("tag") != tag
                or existing_value.get("source_revision") != source_revision
                or existing_value.get("harness_revision") != source_revision
                or type(existing_value.get("completed_at_ns")) is not int
                or existing_value["completed_at_ns"] < 0
                or not isinstance(existing_policy, dict)
                or type(existing_policy.get("require_at_least_one_pass")) is not bool):
            raise EvidenceError("aggregate_existing_receipt_mismatch")
        if existing_policy["require_at_least_one_pass"] != require_pass:
            raise EvidenceError("aggregate_policy_mismatch")
        if existing_value.get("terminal_evidence_published") is True:
            existing, _ = validate_aggregate_receipt(
                receipt_path, expected_tag=tag, expected_source_revision=source_revision,
            )
            return existing
        if existing_value.get("terminal_evidence_published") is not False:
            raise EvidenceError("aggregate_existing_receipt_invalid")
        existing_nonterminal = existing_value

    return existing_nonterminal


def _existing_case(
        row: MatrixRow, compact_path: Path, candidate_case_directory: Path,
        cases_root: Path, cleanup_root: Path) -> dict[str, object] | None:
    # Every existing compact receipt is authoritative.  Consult its cleanup
    # journal before inspecting a pathname that may already have moved.
    if compact_path.exists() or compact_path.is_symlink():
        if compact_path.is_symlink():
            raise EvidenceError("case_compact_receipt_invalid")
        try:
            replayed, _ = _read_json(compact_path, MAX_RECEIPT_BYTES)
            replayed_digest = _digest_file(compact_path, MAX_RECEIPT_BYTES)
        except EvidenceError as error:
            raise EvidenceError("case_compact_receipt_invalid") from error
        if (not isinstance(replayed, dict)
                or replayed.get("case_id") != row.definition.case_id
                or replayed.get("status") != row.status
                or replayed.get("exit_code") != row.exit_code):
            raise EvidenceError("case_compact_receipt_mismatch")
        if (isinstance(replayed, dict) and replayed.get("case_id") == row.definition.case_id
                and replayed.get("evidence_status") == "RESOLVED"
                and replayed.get("terminal_state") == "published-after-cleanup"
                and replayed.get("cleanup", {}).get("raw_deleted") is True):
            return _case_reference(
                row, "RESOLVED", compact_path, replayed_digest, replayed,
            )
        if isinstance(replayed, dict) and replayed.get("terminal_state") == "published-before-cleanup":
            replayed_case = _replay_prepared_case(
                compact_path, candidate_case_directory, cases_root, cleanup_root, row,
            )
            if replayed_case is not None:
                return replayed_case
            raise EvidenceError("case_cleanup_replay_invalid")
        if (replayed.get("terminal_state") == "published-unresolved"
                and replayed.get("evidence_status") == "UNRESOLVED"):
            return _case_reference(
                row, "UNRESOLVED", compact_path, replayed_digest, replayed,
            )
        raise EvidenceError("case_compact_receipt_state_invalid")

    return None


def _observe_case(row: MatrixRow, aggregate_root: Path) -> _CaseObservation:
    candidate_case_directory = aggregate_root / row.definition.case_output
    path_error: str | None = None
    try:
        case_directory = _inside(aggregate_root, candidate_case_directory)
    except EvidenceError as error:
        case_directory = candidate_case_directory
        path_error = error.args[0] if error.args else "case_path_invalid"
    source: dict[str, object] | None = None
    source_digest: str | None = None
    evidence_error: str | None = path_error
    creation_identity: dict[str, object] | None = None
    try:
        if path_error is not None:
            raise EvidenceError(path_error)
        creation_identity = _case_creation_identity(aggregate_root, row)
        _ensure_directory(case_directory)
        if not _identity_matches(case_directory, creation_identity):
            raise EvidenceError("case_directory_identity_changed")
        source, source_digest, evidence_error = _read_case_source(case_directory, row.status)
        cleanup_ok, cleanup, cleanup_error = _cleanup_observation(case_directory, row.status)
        if evidence_error is None and cleanup_error is not None:
            evidence_error = cleanup_error
        if not cleanup_ok and evidence_error is None:
            evidence_error = "cleanup_unverified"
        proof = _source_proof(row, source, cleanup, case_directory, cleanup_ok, source_digest)
        evidence_error = _source_proof_error(row, source, proof, evidence_error)
    except EvidenceError as error:
        cleanup_ok = False
        cleanup = {"status": "unverified"}
        evidence_error = error.args[0] if error.args else "case_evidence_invalid"
        proof = _source_proof(row, source, cleanup, case_directory, cleanup_ok, source_digest) \
            if case_directory.exists() and not case_directory.is_symlink() else _unresolved_source_proof(row, source_digest)
    return _CaseObservation(
        case_directory, source, source_digest, cleanup, proof, creation_identity, evidence_error,
    )


def _build_case(row: MatrixRow, aggregate_root: Path) -> dict[str, object]:
    cases_root = aggregate_root / CASE_ROOT_NAME
    cleanup_root = aggregate_root / CASE_CLEANUP_ROOT_NAME
    compact_path = aggregate_root / COMPACT_RECEIPT_ROOT_NAME / f"{row.definition.case_id}.json"
    existing = _existing_case(
        row, compact_path, aggregate_root / row.definition.case_output, cases_root, cleanup_root,
    )
    if existing is not None:
        return existing
    observation = _observe_case(row, aggregate_root)
    case_directory = observation.directory
    source, source_digest = observation.source, observation.source_digest
    cleanup, proof = observation.cleanup, observation.proof
    creation_identity, evidence_error = observation.creation_identity, observation.error
    before = (_tree_summary(case_directory)
              if case_directory.exists() and not case_directory.is_symlink()
              else _empty_summary())
    if before.get("complete") is not True:
        evidence_error = evidence_error or "case_evidence_too_large"
    should_remove = row.status in {"PASS", "BLOCKED"} and evidence_error is None
    if should_remove:
        if creation_identity is None:
            evidence_error = "case_directory_identity_unavailable"
        else:
            prepared = _sanitize_case(
                row=row, source=source, source_digest=source_digest, cleanup=cleanup, before=before,
                after=before, evidence_status="RESOLVED", evidence_error=None,
                source_proof=proof, creation_identity=creation_identity, terminal_state="published-before-cleanup",
                raw_deleted=False,
            )
            _prepare_case_cleanup(
                compact_path, prepared, cleanup_root, creation_identity,
            )
            resolved = _replay_prepared_case(
                compact_path, case_directory, cases_root, cleanup_root, row,
            )
            if resolved is None:
                raise EvidenceError("directory_owner_cleanup_failed")
            return resolved

    after = (_tree_summary(case_directory)
             if case_directory.exists() and not case_directory.is_symlink()
             else _empty_summary())
    evidence_status = "UNRESOLVED"
    compact = _sanitize_case(
        row=row, source=source, source_digest=source_digest, cleanup=cleanup, before=before, after=after,
        evidence_status=evidence_status, evidence_error=evidence_error,
        source_proof=proof, creation_identity=creation_identity,
        terminal_state="published-unresolved",
        raw_deleted=False,
    )
    compact["raw_deletion"] = {
        "state": "retained",
        "authority": "DirectoryOwner",
        "directory_identity": creation_identity,
    }
    compact["receipt_path"] = str(compact_path.resolve())
    compact_bytes = _write_atomic(compact_path, compact)
    compact_case = _case_reference(
        row, evidence_status, compact_path, _digest_bytes(compact_bytes), compact,
    )
    return compact_case


def _new_aggregate_receipt(
        *, aggregate_root: Path, rows_tuple: tuple[MatrixRow, ...],
        compact_cases: list[dict[str, object]], unresolved: list[str],
        raw_before: dict[str, int | bool], existing_nonterminal: dict[str, object] | None,
        tag: str | None, source_revision: str | None,
        require_pass: bool, retention: int) -> dict[str, object]:
    cases_root = aggregate_root / CASE_ROOT_NAME
    receipts_root = aggregate_root / COMPACT_RECEIPT_ROOT_NAME
    result_verdict = _case_result_verdict(rows_tuple, bool(unresolved))
    aggregate_status = result_verdict
    if result_verdict == "FAIL":
        aggregate_status = "FAIL"
    receipt: dict[str, object] = {
        "schema_version": SCHEMA_VERSION,
        "kind": AGGREGATE_KIND,
        "claim": "native-acceptance-case-results",
        "claims_scope": "provider-permission-scenario-case-results-only",
        "aggregate_root": str(aggregate_root.resolve()),
        "aggregate_owner": _directory_identity(aggregate_root),
        "tag": tag,
        "source_revision": source_revision,
        "harness_revision": source_revision,
        "completed_at_ns": (
            existing_nonterminal["completed_at_ns"]
            if existing_nonterminal is not None else time.time_ns()
        ),
        "terminal_evidence_published": False,
        "matrix_complete": True,
        "expected_case_count": len(EXPECTED_CASES),
        "case_count": len(compact_cases),
        "cases": compact_cases,
        "case_results_verdict": result_verdict,
        "status": aggregate_status,
        "pass_count": sum(row.status == "PASS" for row in rows_tuple),
        "blocked_count": sum(row.status == "BLOCKED" for row in rows_tuple),
        "failed_count": sum(row.status == "FAIL" for row in rows_tuple),
        "unresolved_case_count": len(unresolved),
        "unresolved_cases": unresolved,
        "blocked_scope": "authentication-or-prerequisite-only",
        "policy": {"require_at_least_one_pass": require_pass},
        "exit_code": _aggregate_exit(aggregate_status, result_verdict, require_pass),
        "evidence": {
            "case_raw": {"before": raw_before, "after": _tree_summary(cases_root)},
            "compact_receipts": _tree_summary(receipts_root),
        },
        "retention": {"configured_count": retention, "status": "pending"},
        "credentials_in_receipt": False,
    }
    return receipt


def _publish_aggregate(
        *, aggregate_root: Path, receipt_path: Path, receipt: dict[str, object],
        retention: int, unresolved: list[str], raw_before: dict[str, int | bool],
        cases_root: Path, receipts_root: Path) -> tuple[dict[str, object], int]:
    # The provisional receipt makes this aggregate visible to the retention scan.
    _write_atomic(receipt_path, receipt, MAX_AGGREGATE_BYTES)
    retention_result = _enforce_retention(aggregate_root, receipt, retention)
    if retention_result["status"] != "complete":
        receipt["retention"] = retention_result
        receipt["terminal_evidence_published"] = False
        receipt["status"] = "FAIL"
        receipt["unresolved_case_count"] = len(unresolved) + 1
        receipt["unresolved_cases"] = [*unresolved, "aggregate-retention"]
        receipt["exit_code"] = 1
        evidence = receipt.get("evidence")
        if not isinstance(evidence, dict):
            raise EvidenceError("aggregate_evidence_invalid")
        evidence["case_raw"] = {"before": raw_before, "after": _tree_summary(cases_root)}
        evidence["compact_receipts"] = _tree_summary(receipts_root)
        _write_atomic(receipt_path, receipt, MAX_AGGREGATE_BYTES)
        raise EvidenceError("aggregate_retention_incomplete")
    receipt = _publish_completed_retention_receipt(
        aggregate_root, receipt_path, receipt, retention_result,
    )
    return receipt, int(receipt["exit_code"])


def _build_aggregate_locked(
        aggregate_root: Path,
        matrix_path: Path,
        receipt_path: Path,
        *,
        tag: str | None = None,
        source_revision: str | None = None,
        require_pass: bool = False,
        retention_count: int | None = None) -> tuple[dict[str, object], int]:
    aggregate_root = aggregate_root.absolute()
    _reject_path_components(aggregate_root)
    _ensure_directory(aggregate_root)
    aggregate_root = aggregate_root.resolve()
    cases_root = aggregate_root / CASE_ROOT_NAME
    receipts_root = aggregate_root / COMPACT_RECEIPT_ROOT_NAME
    owners_root = aggregate_root / CASE_OWNER_ROOT_NAME
    cleanup_root = aggregate_root / CASE_CLEANUP_ROOT_NAME
    _ensure_directory(cases_root)
    _ensure_directory(owners_root)
    receipts_root.mkdir(mode=0o700, exist_ok=True)
    _ensure_directory(receipts_root)
    cleanup_root.mkdir(mode=0o700, exist_ok=True)
    _ensure_directory(cleanup_root)
    matrix_path = _inside(aggregate_root, matrix_path)
    receipt_path = _inside(aggregate_root, receipt_path)

    existing = _existing_aggregate(
        aggregate_root, receipt_path, tag, source_revision, require_pass,
    )
    if existing is not None and existing.get("terminal_evidence_published") is True:
        return existing, int(existing["exit_code"])
    rows = _parse_matrix(matrix_path)
    raw_before = _tree_summary(cases_root)
    compact_cases = [_build_case(row, aggregate_root) for row in rows]
    unresolved = [case["case_id"] for case in compact_cases if case["evidence_status"] == "UNRESOLVED"]
    retention = _retention_count(retention_count)
    receipt = _new_aggregate_receipt(
        aggregate_root=aggregate_root, rows_tuple=tuple(rows), compact_cases=compact_cases, unresolved=unresolved,
        raw_before=raw_before, existing_nonterminal=existing,
        tag=tag, source_revision=source_revision, require_pass=require_pass, retention=retention,
    )
    return _publish_aggregate(
        aggregate_root=aggregate_root, receipt_path=receipt_path, receipt=receipt, retention=retention,
        unresolved=unresolved, raw_before=raw_before,
        cases_root=cases_root, receipts_root=receipts_root,
    )
