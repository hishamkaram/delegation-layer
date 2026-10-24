"""Bounded reader for the two closed private native-evidence stage formats.

The release stage carries the archive and native receipts with their cleanup
journals. The candidate stage replaces them with the sanitized native
completion. The caller selects the expected kind from its trusted context; the
reader never infers it from the stage's own ``kind`` field.
"""

from __future__ import annotations

from dataclasses import dataclass
import os
from pathlib import Path, PurePosixPath
import re
import stat

from .contract import AGGREGATE_RECEIPT_NAME, EvidenceError
from .filesystem import _bounded_file_bytes
from .public_contract import (
    CASE_IDS, MAX_STAGE_FILE_BYTES, MAX_STAGE_FILES, MAX_STAGE_TOTAL_BYTES,
    PublicProofError, parse_json, require_object, require_sha256, sha256,
)

_CASE_RECEIPT = re.compile(r"^case-receipts/(case-[0-9]{2}-[A-Za-z0-9-]+)\.json$")
_CASE_CLEANUP = re.compile(r"^cleanup-journals/(case-[0-9]{2}-[A-Za-z0-9-]+)\.json$")
_RETENTION_VICTIM = re.compile(r"^retention/victim-[0-9]{2}\.json$")
LEGACY_STAGE_KIND = "native-acceptance-sanitized-evidence"
CANDIDATE_STAGE_KIND = "native-acceptance-candidate-evidence"
NATIVE_COMPLETION_NAME = "native-completion.json"
_RELEASE_CLEANUP = {
    "archive-receipt.json": ("archive", "release-cleanup/archive-receipt-cleanup.json"),
    "native-acceptance-receipt.json": ("native", "release-cleanup/native-acceptance-receipt-cleanup.json"),
}
_REQUIRED_STAGE_FILES = {
    AGGREGATE_RECEIPT_NAME, "archive-receipt.json", "native-acceptance-receipt.json",
}
_COMMON_MANIFEST_KEYS = frozenset({
    "schema_version", "kind", "aggregate_receipt", "case_count", "case_receipts",
    "cleanup_journals", "retention", "file_count", "files", "credentials_in_receipt",
})
_COMMON_DIRECTORIES = frozenset({"case-receipts", "cleanup-journals", "retention"})
_STAGE_READ_ERRORS = {
    "evidence_file_invalid": "public_stage_file_invalid",
    "evidence_file_too_large": "public_stage_file_too_large",
    "evidence_file_changed": "public_stage_file_changed",
    "evidence_file_unreadable": "public_stage_file_unreadable",
    "evidence_path_parent_component": "public_stage_path_invalid",
    "evidence_path_symlink": "public_stage_symlink_denied",
    "evidence_path_unreadable": "public_stage_file_unreadable",
}


@dataclass(frozen=True)
class _StageFormat:
    """The closed inventory of one stage kind."""

    manifest_keys: frozenset[str]
    required_files: frozenset[str]
    fixed_paths: frozenset[str]
    directories: frozenset[str]


def _stage_format(kind: str) -> _StageFormat:
    if kind == LEGACY_STAGE_KIND:
        return _StageFormat(
            manifest_keys=_COMMON_MANIFEST_KEYS | {"release_cleanup_journals"},
            required_files=frozenset(_REQUIRED_STAGE_FILES),
            fixed_paths=frozenset(_REQUIRED_STAGE_FILES | {item[1] for item in _RELEASE_CLEANUP.values()}),
            directories=_COMMON_DIRECTORIES | {"release-cleanup"})
    if kind == CANDIDATE_STAGE_KIND:
        required = frozenset({AGGREGATE_RECEIPT_NAME, NATIVE_COMPLETION_NAME})
        return _StageFormat(manifest_keys=_COMMON_MANIFEST_KEYS | {"native_completion"},
                            required_files=required, fixed_paths=required, directories=_COMMON_DIRECTORIES)
    raise PublicProofError("public_stage_kind_invalid")


@dataclass(frozen=True)
class StageSnapshot:
    """A loaded stage of the trusted ``kind`` its caller selected."""

    root: Path
    manifest_bytes: bytes
    manifest: dict[str, object]
    data: dict[str, bytes]
    digests: dict[str, str]
    cases: dict[str, dict[str, object]]
    cleanup: dict[str, dict[str, str]]
    release_cleanup: dict[str, dict[str, str]]
    kind: str


def _reject_link_components(path: Path) -> Path:
    absolute = path.absolute()
    current = Path(absolute.anchor)
    for part in absolute.parts[1:]:
        current = current / part
        try:
            mode = current.lstat().st_mode
        except OSError as error:
            raise PublicProofError("public_stage_path_unavailable") from error
        if stat.S_ISLNK(mode):
            raise PublicProofError("public_stage_symlink_denied")
    if not stat.S_ISDIR(current.lstat().st_mode):
        raise PublicProofError("public_stage_not_directory")
    return current.resolve(strict=True)


def _stage_inventory(root: Path, allowed_directories: frozenset[str]) -> set[str]:
    files: set[str] = set()
    pending = [root]
    max_entries = MAX_STAGE_FILES + 1 + len(allowed_directories)
    entries_seen = 0
    while pending:
        parent = pending.pop()
        try:
            with os.scandir(parent) as entries:
                for entry in entries:
                    if entries_seen >= max_entries:
                        raise PublicProofError("public_stage_entry_limit")
                    entries_seen += 1
                    child = Path(entry.path)
                    relative = child.relative_to(root).as_posix()
                    if entry.is_symlink():
                        raise PublicProofError("public_stage_symlink_denied")
                    if entry.is_dir(follow_symlinks=False):
                        if relative not in allowed_directories:
                            raise PublicProofError("public_stage_directory_unexpected")
                        pending.append(child)
                    elif entry.is_file(follow_symlinks=False):
                        files.add(relative)
                    else:
                        raise PublicProofError("public_stage_entry_invalid")
        except PublicProofError:
            raise
        except OSError as error:
            raise PublicProofError("public_stage_unreadable") from error
    return files


def _read_stage_file(root: Path, relative: str, limit: int) -> bytes:
    path = root
    for part in PurePosixPath(relative).parts:
        if part in {"", ".", ".."}:
            raise PublicProofError("public_stage_path_invalid")
        path = path / part
        try:
            mode = path.lstat().st_mode
        except OSError as error:
            raise PublicProofError("public_stage_file_missing") from error
        if stat.S_ISLNK(mode):
            raise PublicProofError("public_stage_symlink_denied")
    if not stat.S_ISREG(mode):
        raise PublicProofError("public_stage_file_invalid")
    try:
        return _bounded_file_bytes(path, limit)
    except EvidenceError as error:
        code = _STAGE_READ_ERRORS.get(str(error), "public_stage_file_unreadable")
        raise PublicProofError(code) from error


def _allow_stage_path(value: object, fixed_paths: frozenset[str]) -> str:
    if not isinstance(value, str) or "\\" in value or value.startswith("/"):
        raise PublicProofError("public_stage_path_invalid")
    if (value in fixed_paths
            or _CASE_RECEIPT.fullmatch(value) is not None
            or _CASE_CLEANUP.fullmatch(value) is not None
            or value == "retention/plan.json" or _RETENTION_VICTIM.fullmatch(value) is not None):
        return value
    raise PublicProofError("public_stage_path_unexpected")


def _manifest_files(value: object, stage_format: _StageFormat) -> dict[str, str]:
    if not isinstance(value, list) or len(value) > MAX_STAGE_FILES:
        raise PublicProofError("public_stage_manifest_files_invalid")
    result: dict[str, str] = {}
    for row in value:
        item = require_object(row, {"path", "sha256"}, "public_stage_manifest_entry_invalid")
        path = _allow_stage_path(item.get("path"), stage_format.fixed_paths)
        digest = require_sha256(item.get("sha256"))
        if path in result:
            raise PublicProofError("public_stage_manifest_duplicate")
        result[path] = digest
    if not stage_format.required_files.issubset(result):
        raise PublicProofError("public_stage_required_file_missing")
    return result


def _entry_records(value: object, files: dict[str, str], key: str,
                   expected_paths: set[str], fixed_paths: frozenset[str]) -> dict[str, str]:
    rows = value.get(key) if isinstance(value, dict) else None
    if not isinstance(rows, list):
        raise PublicProofError("public_stage_manifest_entries_invalid")
    observed: dict[str, str] = {}
    for row in rows:
        item = require_object(row, {"path", "sha256"}, "public_stage_manifest_entry_invalid")
        path = _allow_stage_path(item.get("path"), fixed_paths)
        digest = require_sha256(item.get("sha256"))
        if path in observed or files.get(path) != digest:
            raise PublicProofError("public_stage_manifest_digest_mismatch")
        observed[path] = digest
    if set(observed) != expected_paths:
        raise PublicProofError("public_stage_manifest_entries_invalid")
    return observed


def _case_records(value: dict[str, object], files: dict[str, str],
                  fixed_paths: frozenset[str]) -> dict[str, dict[str, str]]:
    rows = value.get("case_receipts")
    expected_paths = {f"case-receipts/{case_id}.json" for case_id in CASE_IDS}
    if not isinstance(rows, list) or len(rows) != len(CASE_IDS):
        raise PublicProofError("public_stage_case_receipts_invalid")
    by_path: dict[str, dict[str, str]] = {}
    for row in rows:
        item = require_object(row, {"path", "sha256"}, "public_stage_case_receipt_invalid")
        path = _allow_stage_path(item.get("path"), fixed_paths)
        digest = require_sha256(item.get("sha256"))
        if path in by_path or files.get(path) != digest:
            raise PublicProofError("public_stage_case_receipt_digest_mismatch")
        by_path[path] = {"path": path, "sha256": digest}
    if set(by_path) != expected_paths:
        raise PublicProofError("public_stage_case_receipts_invalid")
    return {path.removeprefix("case-receipts/").removesuffix(".json"): item
            for path, item in by_path.items()}


def _cleanup_records(value: dict[str, object], files: dict[str, str],
                     fixed_paths: frozenset[str]) -> dict[str, dict[str, str]]:
    rows = value.get("cleanup_journals")
    if not isinstance(rows, list):
        raise PublicProofError("public_stage_cleanup_records_invalid")
    result: dict[str, dict[str, str]] = {}
    for row in rows:
        item = require_object(row, {"case_id", "path", "sha256", "authorization_sha256"},
                              "public_stage_cleanup_record_invalid")
        case_id = item.get("case_id")
        path = _allow_stage_path(item.get("path"), fixed_paths)
        digest = require_sha256(item.get("sha256"))
        authorization = require_sha256(item.get("authorization_sha256"))
        match = _CASE_CLEANUP.fullmatch(path)
        if (not isinstance(case_id, str) or case_id not in CASE_IDS or match is None
                or match.group(1) != case_id or case_id in result or files.get(path) != digest):
            raise PublicProofError("public_stage_cleanup_record_invalid")
        result[case_id] = {"path": path, "sha256": digest, "authorization_sha256": authorization}
    return result


def _release_cleanup_records(value: dict[str, object], files: dict[str, str],
                             fixed_paths: frozenset[str]) -> dict[str, dict[str, str]]:
    rows = value.get("release_cleanup_journals")
    if not isinstance(rows, list):
        raise PublicProofError("public_stage_release_cleanup_invalid")
    result: dict[str, dict[str, str]] = {}
    for row in rows:
        item = require_object(row, {"receipt", "path", "sha256", "authorization_sha256"},
                              "public_stage_release_cleanup_invalid")
        receipt = item.get("receipt")
        path = _allow_stage_path(item.get("path"), fixed_paths)
        digest = require_sha256(item.get("sha256"))
        authorization = require_sha256(item.get("authorization_sha256"))
        facts = _RELEASE_CLEANUP.get(receipt) if isinstance(receipt, str) else None
        if (facts is None or path != facts[1] or files.get(path) != digest
                or facts[0] in result):
            raise PublicProofError("public_stage_release_cleanup_invalid")
        result[facts[0]] = {"path": path, "sha256": digest, "authorization_sha256": authorization}
    if not set(result).issubset({facts[0] for facts in _RELEASE_CLEANUP.values()}):
        raise PublicProofError("public_stage_release_cleanup_invalid")
    return result


def _native_completion_record(manifest: dict[str, object], files: dict[str, str]) -> None:
    record = require_object(manifest.get("native_completion"), {"path", "sha256"},
                            "public_stage_native_completion_invalid")
    if (record["path"] != NATIVE_COMPLETION_NAME
            or files.get(NATIVE_COMPLETION_NAME) != require_sha256(record["sha256"])):
        raise PublicProofError("public_stage_native_completion_invalid")


def load_stage(stage_dir: Path, *, expected_kind: str = LEGACY_STAGE_KIND) -> StageSnapshot:
    """Load a stage of the kind the caller expects; any other stage is refused."""
    stage_format = _stage_format(expected_kind)
    root = _reject_link_components(stage_dir)
    manifest_bytes = _read_stage_file(root, "manifest.json", MAX_STAGE_FILE_BYTES)
    manifest = parse_json(manifest_bytes, "public_stage_manifest_invalid")
    if not isinstance(manifest, dict) or set(manifest) != stage_format.manifest_keys:
        raise PublicProofError("public_stage_manifest_invalid")
    if (type(manifest.get("schema_version")) is not int or manifest["schema_version"] != 1
            or manifest.get("kind") != expected_kind
            or manifest.get("case_count") != len(CASE_IDS)
            or manifest.get("credentials_in_receipt") is not False):
        raise PublicProofError("public_stage_manifest_invalid")
    files = _manifest_files(manifest.get("files"), stage_format)
    actual = _stage_inventory(root, stage_format.directories)
    if actual != set(files) | {"manifest.json"}:
        raise PublicProofError("public_stage_unexpected_file")
    if type(manifest.get("file_count")) is not int or manifest["file_count"] != len(files):
        raise PublicProofError("public_stage_manifest_count_invalid")
    aggregate = manifest.get("aggregate_receipt")
    aggregate_record = require_object(aggregate, {"path", "sha256"},
                                      "public_stage_aggregate_invalid")
    if (aggregate_record["path"] != AGGREGATE_RECEIPT_NAME
            or files.get(AGGREGATE_RECEIPT_NAME) != aggregate_record["sha256"]):
        raise PublicProofError("public_stage_aggregate_invalid")
    fixed = stage_format.fixed_paths
    case_rows = _case_records(manifest, files, fixed)
    cleanup_rows = _cleanup_records(manifest, files, fixed)
    if expected_kind == LEGACY_STAGE_KIND:
        release_rows = _release_cleanup_records(manifest, files, fixed)
    else:
        _native_completion_record(manifest, files)
        release_rows = {}
    _entry_records(manifest, files, "case_receipts", {row["path"] for row in case_rows.values()}, fixed)
    data: dict[str, bytes] = {}
    total = len(manifest_bytes)
    for path, digest in files.items():
        limit = MAX_STAGE_TOTAL_BYTES if path == AGGREGATE_RECEIPT_NAME else MAX_STAGE_FILE_BYTES
        content = _read_stage_file(root, path, limit)
        total += len(content)
        if total > MAX_STAGE_TOTAL_BYTES or sha256(content) != digest:
            raise PublicProofError("public_stage_file_digest_mismatch")
        data[path] = content
    if len(data) > MAX_STAGE_FILES:
        raise PublicProofError("public_stage_entry_limit")
    return StageSnapshot(
        root=root, manifest_bytes=manifest_bytes, manifest=manifest, data=data,
        digests=files, cases=case_rows, cleanup=cleanup_rows, release_cleanup=release_rows,
        kind=expected_kind,
    )
