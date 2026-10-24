"""Staging for native acceptance evidence.

Both stage kinds share the source lease, aggregate validation, the case,
cleanup and retention plan, the publication transaction with its manifest-last
commit and recovery, and the published-tree verification. Only their finishers
differ: the release stage adds the archive and native receipts, the candidate
stage adds the sanitized native completion.
"""

from __future__ import annotations

from dataclasses import dataclass
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
from .candidate_proof import completion_record, require_completion_binding, require_harness_revision
from .public_contract import (
    CandidateProofExpectation, NativeCompletion, PublicProofError,
    canonical_json as public_canonical_json,
)
from .public_stage import CANDIDATE_STAGE_KIND, LEGACY_STAGE_KIND, NATIVE_COMPLETION_NAME
from .stage_publication import StagePlan, publish_stage
from .validation import (
    validate_aggregate_receipt,
)


_LEGACY_SOURCE_NAMES = frozenset({AGGREGATE_RECEIPT_NAME, "archive-receipt.json", "native-acceptance-receipt.json"})
_CANDIDATE_SOURCE_NAMES = frozenset({AGGREGATE_RECEIPT_NAME, NATIVE_COMPLETION_NAME})


def _with_source_lease(aggregate_path: Path, stage: Callable[[], dict[str, object]]) -> dict[str, object]:
    """Run ``stage`` while holding the source aggregate's ownership lease."""
    source_root = aggregate_path.absolute().parent
    lock_fd = acquire_exclusive_file_lock(
        source_root.parent / ("." + source_root.name + ".lock"),
    )
    try:
        return stage()
    finally:
        release_exclusive_file_lock(lock_fd)


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
    """Stage release evidence while retaining the source aggregate's ownership lease."""
    return _with_source_lease(aggregate_path, lambda: _stage_evidence_locked(
        aggregate_path,
        output,
        archive_receipt=archive_receipt,
        native_receipt=native_receipt,
        expected_tag=expected_tag,
        expected_source_revision=expected_source_revision,
        directory_identity=directory_identity,
        expected_source_digests=expected_source_digests,
    ))


def stage_candidate_evidence(
        aggregate_path: Path,
        output: Path,
        *,
        completion: NativeCompletion,
        expectation: CandidateProofExpectation,
        directory_identity: dict[str, object] | None = None,
        expected_source_digests: dict[str, str] | None = None) -> dict[str, object]:
    """Stage candidate evidence for one observed native completion of the expected invocation.

    The completion must be the natural Wait of the expected invocation, and the
    aggregate must carry its exit code and the expected native harness revision.
    A natural Wait alone proves no case or invocation closure.
    """
    if not isinstance(completion, NativeCompletion) or not isinstance(expectation, CandidateProofExpectation):
        raise EvidenceError("stage_candidate_input_invalid")
    return _with_source_lease(aggregate_path, lambda: _stage_candidate_locked(
        aggregate_path, output, completion=completion, expectation=expectation,
        directory_identity=directory_identity, expected_source_digests=expected_source_digests,
    ))


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


def _validated_source(
        aggregate_path: Path, output: Path, extra_sources: tuple[Path | None, ...], *,
        expected_tag: str | None, expected_source_revision: str | None,
) -> tuple[Path, Path, dict[str, object], str]:
    aggregate_path = aggregate_path.absolute()
    output = output.absolute()
    _reject_path_components(aggregate_path)
    _reject_path_components(output)
    for source in extra_sources:
        if source is not None:
            _reject_path_components(source)
    if output.name.startswith("."):
        raise EvidenceError("stage_output_hidden_or_parent")
    aggregate, aggregate_digest = validate_aggregate_receipt(
        aggregate_path,
        expected_tag=expected_tag,
        expected_source_revision=expected_source_revision,
    )
    return aggregate_path, output, aggregate, aggregate_digest


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
    aggregate_path, output, aggregate, aggregate_digest = _validated_source(
        aggregate_path, output, (archive_receipt, native_receipt),
        expected_tag=expected_tag, expected_source_revision=expected_source_revision)
    plan = StagePlan(output)
    common = _plan_common(aggregate_path, aggregate, aggregate_digest, plan)
    release_cleanup_journals = _stage_release_receipts(
        output, archive_receipt, native_receipt, common.files, plan.put,
    )
    manifest = _finish_manifest(plan, common, LEGACY_STAGE_KIND,
                                {"release_cleanup_journals": release_cleanup_journals})
    return _publish(plan, common.files, manifest, directory_identity,
                    expected_source_digests, _LEGACY_SOURCE_NAMES)


def _stage_candidate_locked(
        aggregate_path: Path, output: Path, *, completion: NativeCompletion,
        expectation: CandidateProofExpectation, directory_identity: dict[str, object] | None,
        expected_source_digests: dict[str, str] | None) -> dict[str, object]:
    aggregate_path, output, aggregate, aggregate_digest = _validated_source(
        aggregate_path, output, (), expected_tag=None,
        expected_source_revision=expectation.harness_revision)
    try:
        require_completion_binding(completion, expectation, exit_code=aggregate.get("exit_code"))
        require_harness_revision(aggregate, expectation)
    except PublicProofError as error:
        raise EvidenceError(str(error)) from error
    plan = StagePlan(output)
    common = _plan_common(aggregate_path, aggregate, aggregate_digest, plan)
    completion_bytes = public_canonical_json(completion_record(completion))
    plan.put(output / NATIVE_COMPLETION_NAME, completion_bytes, MAX_RECEIPT_BYTES)
    record = {"path": NATIVE_COMPLETION_NAME, "sha256": _digest_bytes(completion_bytes)}
    common.files.append(record)
    manifest = _finish_manifest(plan, common, CANDIDATE_STAGE_KIND, {"native_completion": record})
    return _publish(plan, common.files, manifest, directory_identity,
                    expected_source_digests, _CANDIDATE_SOURCE_NAMES)


@dataclass(frozen=True)
class _CommonPlan:
    """The planned aggregate, case, cleanup and retention entries both stage kinds share.

    ``files`` records every planned file in plan order; a finisher appends its own.
    """

    aggregate_digest: str
    files: list[dict[str, str]]
    case_receipts: list[dict[str, str]]
    cleanup_journals: list[dict[str, str]]
    retention: dict[str, object]


def _plan_common(aggregate_path: Path, aggregate: dict[str, object], aggregate_digest: str,
                 plan: StagePlan) -> _CommonPlan:
    output = plan.output
    plan.put(output / AGGREGATE_RECEIPT_NAME, _bounded_file_bytes(
        aggregate_path, MAX_AGGREGATE_BYTES,
    ), MAX_AGGREGATE_BYTES)
    files: list[dict[str, str]] = [{"path": AGGREGATE_RECEIPT_NAME, "sha256": aggregate_digest}]
    case_receipts, cleanup_journals = _stage_cases(aggregate_path, output, aggregate, files, plan.put)
    retention = _stage_retention(aggregate_path, output, aggregate, files, plan.put)
    return _CommonPlan(aggregate_digest, files, case_receipts, cleanup_journals, retention)


def _finish_manifest(plan: StagePlan, common: _CommonPlan, kind: str,
                     kind_fields: dict[str, object]) -> dict[str, object]:
    """Seal the plan with the manifest of ``kind``; the manifest is written last."""
    planned_files = [{"path": item.name, "sha256": _digest_bytes(item.data)}
                     for item in plan.files()]
    if common.files != planned_files:
        raise EvidenceError("stage_manifest_plan_mismatch")
    manifest: dict[str, object] = {
        "schema_version": SCHEMA_VERSION,
        "kind": kind,
        "aggregate_receipt": {"path": AGGREGATE_RECEIPT_NAME, "sha256": common.aggregate_digest},
        "case_count": len(EXPECTED_CASES),
        "case_receipts": common.case_receipts,
        "cleanup_journals": common.cleanup_journals,
        "retention": common.retention,
        **kind_fields,
        "file_count": len(common.files),
        "files": planned_files,
        "credentials_in_receipt": False,
    }
    plan.put(plan.output / "manifest.json", canonical_json(manifest), MAX_RECEIPT_BYTES)
    return manifest


def _publish(plan: StagePlan, files: list[dict[str, str]], manifest: dict[str, object],
             directory_identity: dict[str, object] | None, expected_source_digests: dict[str, str] | None,
             source_names: frozenset[str]) -> dict[str, object]:
    _check_source_digests(plan, expected_source_digests, names=source_names)
    publish_stage(plan, directory_identity)
    _verify_staged_manifest(plan.output, files)
    return manifest


def _check_source_digests(plan: StagePlan, expected: dict[str, str] | None, *,
                          names: frozenset[str] = _LEGACY_SOURCE_NAMES) -> None:
    if expected is None:
        return
    if set(expected) != names:
        raise EvidenceError("stage_source_digest_set_invalid")
    observed = {item.name: _digest_bytes(item.data) for item in plan.files() if item.name in names}
    if observed != expected:
        raise EvidenceError("stage_source_digest_changed")
