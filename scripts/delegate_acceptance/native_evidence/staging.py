"""Staging for native acceptance evidence."""

from __future__ import annotations

from pathlib import Path
from collections.abc import Callable
from ..directories.journal import acquire_exclusive_file_lock
import json
from ..directories.journal import release_exclusive_file_lock
from .contract import (
    AGGREGATE_RECEIPT_NAME,
    COMPACT_RECEIPT_ROOT_NAME,
    EXPECTED_CASES,
    EvidenceError,
    HEX_DIGEST,
    MAX_AGGREGATE_BYTES,
    MAX_RECEIPT_BYTES,
    RETENTION_CLEANUP_ROOT_NAME,
    SCHEMA_VERSION,
    canonical_json,
    _digest_bytes,
)
from .filesystem import (
    _bounded_children,
    _bounded_file_bytes,
    _digest_file,
    _inside,
    _read_json,
    _reject_path_components,
    _write_bytes_atomic,
)
from .stage_publication import StagePlan, publish_stage
from .validation import (
    validate_aggregate_receipt,
)


def stage_evidence(
        aggregate_path: Path,
        output: Path,
        *,
        archive_receipt: Path | None = None,
        native_receipt: Path | None = None,
        expected_tag: str | None = None,
        expected_source_revision: str | None = None,
        directory_identity: dict[str, object] | None = None,
        expected_source_digests: dict[str, str] | None = None) -> dict[str, object]:
    """Stage evidence while retaining the source aggregate's ownership lease."""
    source_root = aggregate_path.absolute().parent
    lock_fd = acquire_exclusive_file_lock(
        source_root.parent / ("." + source_root.name + ".lock"),
    )
    try:
        return _stage_evidence_locked(
            aggregate_path,
            output,
            archive_receipt=archive_receipt,
            native_receipt=native_receipt,
            expected_tag=expected_tag,
            expected_source_revision=expected_source_revision,
            directory_identity=directory_identity,
            expected_source_digests=expected_source_digests,
        )
    finally:
        release_exclusive_file_lock(lock_fd)


def _stage_cases(
        aggregate_path: Path, output: Path, aggregate: dict[str, object],
        files: list[dict[str, str]],
        put: Callable[[Path, bytes, int], None] = _write_bytes_atomic,
) -> tuple[list[dict[str, str]], list[dict[str, str]]]:
    case_output = output / "case-receipts"
    cleanup_output = output / "cleanup-journals"
    case_receipts: list[dict[str, str]] = []
    cleanup_journals: list[dict[str, str]] = []
    case_rows = aggregate.get("cases")
    if not isinstance(case_rows, list) or len(case_rows) != len(EXPECTED_CASES):
        raise EvidenceError("stage_case_count_invalid")
    for case in case_rows:
        if not isinstance(case, dict):
            raise EvidenceError("stage_case_invalid")
        source = Path(str(case["receipt_path"]))
        data = _bounded_file_bytes(source, MAX_RECEIPT_BYTES)
        digest = _digest_bytes(data)
        if digest != case.get("receipt_sha256"):
            raise EvidenceError("stage_case_digest_mismatch")
        name = f"{case['case_id']}.json"
        put(case_output / name, data, MAX_RECEIPT_BYTES)
        case_entry = {"path": f"case-receipts/{name}", "sha256": digest}
        case_receipts.append(case_entry)
        files.append(case_entry)
        compact = json.loads(data)
        deletion = compact.get("raw_deletion") if isinstance(compact, dict) else None
        if case.get("evidence_status") == "RESOLVED":
            if (not isinstance(deletion, dict)
                    or not isinstance(deletion.get("journal_path"), str)
                    or not isinstance(deletion.get("journal_sha256"), str)
                    or not isinstance(deletion.get("authorization_sha256"), str)):
                raise EvidenceError("stage_cleanup_journal_missing")
            source_journal = _inside(
                aggregate_path.parent / COMPACT_RECEIPT_ROOT_NAME,
                Path(deletion["journal_path"]),
            )
            journal_data = _bounded_file_bytes(source_journal, MAX_RECEIPT_BYTES)
            journal_digest = _digest_bytes(journal_data)
            if journal_digest != deletion["journal_sha256"]:
                raise EvidenceError("stage_cleanup_journal_digest_mismatch")
            journal_name = f"{case['case_id']}.json"
            put(cleanup_output / journal_name, journal_data, MAX_RECEIPT_BYTES)
            journal_entry = {
                "case_id": str(case["case_id"]),
                "path": f"cleanup-journals/{journal_name}",
                "sha256": journal_digest,
                "authorization_sha256": deletion["authorization_sha256"],
            }
            cleanup_journals.append(journal_entry)
            files.append({"path": journal_entry["path"], "sha256": journal_digest})

    return case_receipts, cleanup_journals


def _stage_retention(
        aggregate_path: Path, output: Path, aggregate: dict[str, object],
        files: list[dict[str, str]],
        put: Callable[[Path, bytes, int], None] = _write_bytes_atomic) -> dict[str, object]:
    retention = aggregate.get("retention")
    if (not isinstance(retention, dict) or not isinstance(retention.get("plan_path"), str)
            or not isinstance(retention.get("plan_sha256"), str)):
        raise EvidenceError("stage_retention_plan_missing")
    retention_plan_source = _inside(aggregate_path.parent, Path(retention["plan_path"]))
    retention_plan_data = _bounded_file_bytes(retention_plan_source, MAX_RECEIPT_BYTES)
    retention_plan_digest = _digest_bytes(retention_plan_data)
    if retention_plan_digest != retention["plan_sha256"]:
        raise EvidenceError("stage_retention_plan_digest_mismatch")
    retention_plan_path = "retention/plan.json"
    put(output / retention_plan_path, retention_plan_data, MAX_RECEIPT_BYTES)
    files.append({"path": retention_plan_path, "sha256": retention_plan_digest})
    retention_plan = json.loads(retention_plan_data)
    retention_journals: list[dict[str, str]] = []
    victims = retention_plan.get("victims") if isinstance(retention_plan, dict) else None
    if not isinstance(victims, list):
        raise EvidenceError("stage_retention_plan_invalid")
    for index, victim in enumerate(victims):
        if (not isinstance(victim, dict) or not isinstance(victim.get("journal_path"), str)
                or not isinstance(victim.get("journal_sha256"), str)
                or not isinstance(victim.get("authorization_sha256"), str)):
            raise EvidenceError("stage_retention_journal_missing")
        source_journal = _inside(
            aggregate_path.parent / RETENTION_CLEANUP_ROOT_NAME,
            Path(victim["journal_path"]),
        )
        journal_data = _bounded_file_bytes(source_journal, MAX_RECEIPT_BYTES)
        journal_digest = _digest_bytes(journal_data)
        if journal_digest != victim["journal_sha256"]:
            raise EvidenceError("stage_retention_journal_digest_mismatch")
        journal_path = f"retention/victim-{index:02d}.json"
        put(output / journal_path, journal_data, MAX_RECEIPT_BYTES)
        journal_entry = {
            "path": journal_path,
            "sha256": journal_digest,
            "authorization_sha256": victim["authorization_sha256"],
        }
        retention_journals.append(journal_entry)
        files.append({"path": journal_path, "sha256": journal_digest})
    return {
        "plan": {"path": retention_plan_path, "sha256": retention_plan_digest},
        "cleanup_journals": retention_journals,
    }


def _stage_release_receipts(
        output: Path, archive_receipt: Path | None, native_receipt: Path | None,
        files: list[dict[str, str]],
        put: Callable[[Path, bytes, int], None] = _write_bytes_atomic) -> list[dict[str, str]]:
    release_cleanup_journals: list[dict[str, str]] = []
    for source, name in ((archive_receipt, "archive-receipt.json"),
                         (native_receipt, "native-acceptance-receipt.json")):
        if source is None:
            continue
        source = source.absolute()
        data = _bounded_file_bytes(source, MAX_AGGREGATE_BYTES)
        put(output / name, data, MAX_AGGREGATE_BYTES)
        files.append({"path": name, "sha256": _digest_bytes(data)})
        try:
            release_receipt = json.loads(data)
        except (UnicodeError, json.JSONDecodeError, RecursionError) as error:
            raise EvidenceError("stage_release_receipt_invalid") from error
        cleanup = release_receipt.get("cleanup") if isinstance(release_receipt, dict) else None
        transaction = cleanup.get("transaction") if isinstance(cleanup, dict) else None
        completion = release_receipt.get("completion") if isinstance(release_receipt, dict) else None
        if isinstance(transaction, dict):
            journal_value = transaction.get("journal_path")
            expected_digest = transaction.get("journal_sha256")
            authorization = transaction.get("authorization_sha256")
            expected_journal = source.with_name(source.stem + ".cleanup.json")
            if (not isinstance(journal_value, str)
                    or Path(journal_value).absolute() != expected_journal
                    or not isinstance(expected_digest, str)
                    or HEX_DIGEST.fullmatch(expected_digest) is None
                    or not isinstance(authorization, str)
                    or HEX_DIGEST.fullmatch(authorization) is None):
                raise EvidenceError("stage_release_cleanup_journal_invalid")
            journal_data = _bounded_file_bytes(expected_journal, MAX_RECEIPT_BYTES)
            journal_digest = _digest_bytes(journal_data)
            if journal_digest != expected_digest:
                raise EvidenceError("stage_release_cleanup_journal_digest_mismatch")
            journal_name = name.removesuffix(".json") + "-cleanup.json"
            journal_path = f"release-cleanup/{journal_name}"
            put(output / journal_path, journal_data, MAX_RECEIPT_BYTES)
            journal_entry = {
                "receipt": name,
                "path": journal_path,
                "sha256": journal_digest,
                "authorization_sha256": authorization,
            }
            release_cleanup_journals.append(journal_entry)
            files.append({"path": journal_path, "sha256": journal_digest})
        elif isinstance(completion, dict) and completion.get("state") == "complete":
            raise EvidenceError("stage_release_cleanup_journal_missing")
    return release_cleanup_journals


def _verify_staged_manifest(output: Path, files: list[dict[str, str]]) -> None:
    case_output = output / "case-receipts"
    staged_entries = sorted(item.name for item in _bounded_children(output) if not item.is_symlink())
    expected_entries = sorted({item["path"].split("/", 1)[0] for item in files} | {"manifest.json"})
    if staged_entries != expected_entries:
        raise EvidenceError("stage_manifest_files_invalid")
    staged_cases = sorted(item.name for item in _bounded_children(case_output))
    expected_cases = sorted(item["path"].split("/", 1)[1] for item in files
                             if item["path"].startswith("case-receipts/"))
    if staged_cases != expected_cases:
        raise EvidenceError("stage_manifest_case_files_invalid")
    manifest_value, _ = _read_json(output / "manifest.json", MAX_RECEIPT_BYTES)
    if (not isinstance(manifest_value, dict)
            or manifest_value.get("case_count") != len(EXPECTED_CASES)
            or manifest_value.get("file_count") != len(files)
            or manifest_value.get("files") != files):
        raise EvidenceError("stage_manifest_invalid")
    for item in files:
        aggregate_names = {AGGREGATE_RECEIPT_NAME, "archive-receipt.json", "native-acceptance-receipt.json"}
        limit = MAX_AGGREGATE_BYTES if item["path"] in aggregate_names else MAX_RECEIPT_BYTES
        if _digest_file(output / item["path"], limit) != item["sha256"]:
            raise EvidenceError("stage_manifest_digest_mismatch")


def _stage_evidence_locked(
        aggregate_path: Path,
        output: Path,
        *,
        archive_receipt: Path | None = None,
        native_receipt: Path | None = None,
        expected_tag: str | None = None,
        expected_source_revision: str | None = None,
        directory_identity: dict[str, object] | None = None,
        expected_source_digests: dict[str, str] | None = None) -> dict[str, object]:
    aggregate_path = aggregate_path.absolute()
    output = output.absolute()
    _reject_path_components(aggregate_path)
    _reject_path_components(output)
    for source in (archive_receipt, native_receipt):
        if source is not None:
            _reject_path_components(source)
    if output.name.startswith("."):
        raise EvidenceError("stage_output_hidden_or_parent")
    aggregate, aggregate_digest = validate_aggregate_receipt(
        aggregate_path,
        expected_tag=expected_tag,
        expected_source_revision=expected_source_revision,
    )
    plan = StagePlan(output)
    manifest, files = _build_stage_plan(aggregate_path, aggregate, aggregate_digest,
                                       archive_receipt, native_receipt, plan)
    _check_source_digests(plan, expected_source_digests)
    publish_stage(plan, directory_identity)
    _verify_staged_manifest(output, files)
    return manifest


def _build_stage_plan(
        aggregate_path: Path, aggregate: dict[str, object], aggregate_digest: str,
        archive_receipt: Path | None, native_receipt: Path | None,
        plan: StagePlan) -> tuple[dict[str, object], list[dict[str, str]]]:
    output = plan.output
    plan.put(output / AGGREGATE_RECEIPT_NAME, _bounded_file_bytes(
        aggregate_path, MAX_AGGREGATE_BYTES,
    ), MAX_AGGREGATE_BYTES)
    files: list[dict[str, str]] = [{"path": AGGREGATE_RECEIPT_NAME, "sha256": aggregate_digest}]
    case_receipts, cleanup_journals = _stage_cases(aggregate_path, output, aggregate, files, plan.put)
    retention = _stage_retention(aggregate_path, output, aggregate, files, plan.put)
    release_cleanup_journals = _stage_release_receipts(
        output, archive_receipt, native_receipt, files, plan.put,
    )
    planned_files = [{"path": item.name, "sha256": _digest_bytes(item.data)}
                     for item in plan.files()]
    if files != planned_files:
        raise EvidenceError("stage_manifest_plan_mismatch")
    manifest: dict[str, object] = {
        "schema_version": SCHEMA_VERSION,
        "kind": "native-acceptance-sanitized-evidence",
        "aggregate_receipt": {"path": AGGREGATE_RECEIPT_NAME, "sha256": aggregate_digest},
        "case_count": len(EXPECTED_CASES),
        "case_receipts": case_receipts,
        "cleanup_journals": cleanup_journals,
        "retention": retention,
        "release_cleanup_journals": release_cleanup_journals,
        "file_count": len(files),
        "files": planned_files,
        "credentials_in_receipt": False,
    }
    plan.put(output / "manifest.json", canonical_json(manifest), MAX_RECEIPT_BYTES)
    return manifest, files


def _check_source_digests(plan: StagePlan, expected: dict[str, str] | None) -> None:
    if expected is None:
        return
    names = {AGGREGATE_RECEIPT_NAME, "archive-receipt.json", "native-acceptance-receipt.json"}
    if set(expected) != names:
        raise EvidenceError("stage_source_digest_set_invalid")
    observed = {item.name: _digest_bytes(item.data) for item in plan.files() if item.name in names}
    if observed != expected:
        raise EvidenceError("stage_source_digest_changed")
