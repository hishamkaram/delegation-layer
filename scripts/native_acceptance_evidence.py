#!/usr/bin/env python3
"""Build, validate, and retain bounded native acceptance evidence."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
import time

try:
    from acceptance_supervisor_common import (
        DirectoryOwner,
        OwnedDirectory,
        acquire_exclusive_file_lock,
        cleanup_journal_digest,
        directory_identity_record,
        prepare_directory_cleanup,
        read_directory_cleanup,
        release_exclusive_file_lock,
        resume_directory_cleanup,
    )
except ModuleNotFoundError:
    from scripts.acceptance_supervisor_common import (
        DirectoryOwner,
        OwnedDirectory,
        acquire_exclusive_file_lock,
        cleanup_journal_digest,
        directory_identity_record,
        prepare_directory_cleanup,
        read_directory_cleanup,
        release_exclusive_file_lock,
        resume_directory_cleanup,
    )


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


def canonical_json(value: object) -> bytes:
    try:
        return (json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n").encode()
    except (TypeError, ValueError) as error:
        raise EvidenceError("receipt_json_invalid") from error


def _fsync_directory(path: Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    except OSError as error:
        raise EvidenceError("evidence_directory_sync_failed") from error


def _write_atomic(path: Path, value: object, max_bytes: int = MAX_RECEIPT_BYTES) -> bytes:
    data = canonical_json(value)
    if len(data) > max_bytes:
        raise EvidenceError("receipt_too_large")
    _reject_path_components(path)
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        os.chmod(path, stat.S_IRUSR | stat.S_IWUSR)
        _fsync_directory(path.parent)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
    return data


def _bounded_file_bytes(path: Path, limit: int) -> bytes:
    _reject_path_components(path)
    try:
        metadata = path.lstat()
        if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode):
            raise EvidenceError("evidence_file_invalid")
        if metadata.st_size > limit:
            raise EvidenceError("evidence_file_too_large")
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
        descriptor = os.open(path, flags)
        try:
            opened = os.fstat(descriptor)
            if (not stat.S_ISREG(opened.st_mode) or opened.st_size > limit
                    or (opened.st_dev, opened.st_ino) != (metadata.st_dev, metadata.st_ino)):
                raise EvidenceError("evidence_file_changed")
            chunks: list[bytes] = []
            remaining = limit
            while remaining:
                chunk = os.read(descriptor, min(1024 * 1024, remaining))
                if not chunk:
                    break
                chunks.append(chunk)
                remaining -= len(chunk)
            if os.read(descriptor, 1):
                raise EvidenceError("evidence_file_too_large")
            data = b"".join(chunks)
        finally:
            os.close(descriptor)
    except EvidenceError:
        raise
    except (OSError, UnicodeError) as error:
        raise EvidenceError("evidence_file_unreadable") from error
    return data


def _read_json(path: Path, limit: int = MAX_INPUT_BYTES) -> tuple[object, bytes]:
    try:
        data = _bounded_file_bytes(path, limit)
        value = json.loads(data)
    except EvidenceError:
        raise
    except (UnicodeError, json.JSONDecodeError, RecursionError) as error:
        raise EvidenceError("evidence_file_invalid") from error
    return value, data


def _digest_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _digest_file(path: Path, limit: int = MAX_INPUT_BYTES) -> str:
    value = hashlib.sha256()
    try:
        data = _bounded_file_bytes(path, limit)
        value.update(data)
    except EvidenceError:
        raise
    return value.hexdigest()


def _ensure_directory(path: Path) -> None:
    try:
        metadata = path.lstat()
    except OSError as error:
        raise EvidenceError("evidence_directory_missing") from error
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISDIR(metadata.st_mode):
        raise EvidenceError("evidence_directory_invalid")


def _reject_path_components(path: Path) -> None:
    if ".." in path.parts:
        raise EvidenceError("evidence_path_parent_component")
    absolute = path.absolute()
    current = Path(absolute.anchor)
    for part in absolute.parts[1:]:
        current /= part
        try:
            metadata = current.lstat()
        except FileNotFoundError:
            break
        except OSError as error:
            raise EvidenceError("evidence_path_unreadable") from error
        if stat.S_ISLNK(metadata.st_mode):
            raise EvidenceError("evidence_path_symlink")


def _inside(root: Path, candidate: Path) -> Path:
    _reject_path_components(root)
    _reject_path_components(candidate)
    root = root.resolve()
    candidate = candidate.absolute()
    try:
        relative = candidate.relative_to(root)
    except ValueError as error:
        raise EvidenceError("evidence_path_outside_aggregate") from error
    current = root
    for part in relative.parts:
        current = current / part
        try:
            metadata = current.lstat()
        except FileNotFoundError:
            continue
        except OSError as error:
            raise EvidenceError("evidence_path_unreadable") from error
        if stat.S_ISLNK(metadata.st_mode):
            raise EvidenceError("evidence_path_symlink")
    return candidate


def _empty_summary() -> dict[str, int | bool]:
    return {"root_count": 0, "file_count": 0, "byte_count": 0, "complete": True,
            "count": 0, "bytes": 0}


def _tree_summary(path: Path) -> dict[str, int | bool]:
    _reject_path_components(path)
    if not path.exists() and not path.is_symlink():
        return _empty_summary()
    if path.is_symlink():
        raise EvidenceError("evidence_path_symlink")
    count = 0
    total = 0
    complete = True
    try:
        pending = [path]
        visited = 0
        while pending and complete:
            current = pending.pop()
            with os.scandir(current) as entries:
                for entry in entries:
                    visited += 1
                    if visited > MAX_WALK_ENTRIES:
                        complete = False
                        break
                    metadata = entry.stat(follow_symlinks=False)
                    if stat.S_ISDIR(metadata.st_mode) and not stat.S_ISLNK(metadata.st_mode):
                        pending.append(Path(entry.path))
                        continue
                    count += 1
                    total += metadata.st_size
                    if total > MAX_WALK_BYTES:
                        complete = False
                        break
    except OSError as error:
        raise EvidenceError("evidence_tree_unreadable") from error
    return {
        "root_count": 1,
        "file_count": count,
        "byte_count": total,
        "complete": complete,
        # Compatibility aliases; the named totals above are authoritative.
        "count": count,
        "bytes": total,
    }


def _bounded_children(path: Path, *, limit: int = MAX_WALK_ENTRIES) -> list[Path]:
    _reject_path_components(path)
    children: list[Path] = []
    try:
        with os.scandir(path) as entries:
            for entry in entries:
                if len(children) >= limit:
                    raise EvidenceError("evidence_directory_too_large")
                children.append(Path(entry.path))
    except EvidenceError:
        raise
    except OSError as error:
        raise EvidenceError("evidence_directory_unreadable") from error
    return children


def _directory_identity(path: Path) -> dict[str, object]:
    try:
        child = path.lstat()
        parent = path.parent.lstat()
    except OSError as error:
        raise EvidenceError("evidence_directory_identity_unavailable") from error
    if (not stat.S_ISDIR(child.st_mode) or stat.S_ISLNK(child.st_mode)
            or not stat.S_ISDIR(parent.st_mode) or stat.S_ISLNK(parent.st_mode)):
        raise EvidenceError("evidence_directory_identity_invalid")
    return {
        "path": str(path.absolute()),
        "parent": str(path.parent.absolute()),
        "device": child.st_dev,
        "inode": child.st_ino,
        "parent_device": parent.st_dev,
        "parent_inode": parent.st_ino,
    }


def _identity_matches(path: Path, identity: object) -> bool:
    if not isinstance(identity, dict):
        return False
    try:
        observed = _directory_identity(path)
    except EvidenceError:
        return False
    return observed == identity


def _identity_shape(identity: object, path: Path, parent: Path) -> bool:
    return (isinstance(identity, dict)
            and identity.get("path") == str(path.absolute())
            and identity.get("parent") == str(parent.absolute())
            and all(type(identity.get(key)) is int for key in
                    ("device", "inode", "parent_device", "parent_inode")))


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
            or not os.path.isabs(value) or any(ord(character) < 32 for character in value)):
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


def _cleanup_observation(case_directory: Path, status: str) -> tuple[bool, dict[str, object], str | None]:
    cleanup_path = case_directory / "cleanup.json"
    if cleanup_path.exists() or cleanup_path.is_symlink():
        try:
            value, data = _read_json(cleanup_path, MAX_RECEIPT_BYTES)
        except EvidenceError:
            return False, {"status": "unreadable"}, "cleanup_receipt_invalid"
        if not isinstance(value, dict):
            return False, {"status": "malformed"}, "cleanup_receipt_invalid"
        cleanup_status = value.get("status")
        shutdown = value.get("supervisor_shutdown_observed")
        valid_status = cleanup_status in {"removed", "complete"}
        valid_shutdown = shutdown is True
        if not valid_status or not valid_shutdown:
            return False, {
                "status": "unverified",
                "source_status": cleanup_status if isinstance(cleanup_status, str) else None,
                "supervisor_shutdown_observed": shutdown if isinstance(shutdown, bool) else None,
            }, "cleanup_receipt_incomplete"
        raw_captures = value.get("raw_process_captures")
        if status in {"PASS", "BLOCKED"} and raw_captures not in (None, "removed"):
            return False, {"status": "unverified", "raw_process_captures": "retained"}, "raw_case_evidence_retained"
        return True, {
            "status": "verified",
            "source_status": cleanup_status,
            "supervisor_shutdown_observed": shutdown if isinstance(shutdown, bool) else None,
            "raw_process_captures": raw_captures if isinstance(raw_captures, str) else None,
            "cleanup_receipt_sha256": _digest_bytes(data),
        }, None

    if status == "BLOCKED":
        try:
            child_names = sorted(item.name for item in _bounded_children(case_directory))
        except EvidenceError:
            return False, {"status": "unverified"}, "case_directory_unreadable"
        if "processes" in child_names:
            return False, {"status": "unverified", "unexpected": ["processes"]}, "raw_case_evidence_retained"
        proof = {
            "status": "complete", "supervisor_shutdown_observed": True,
            "raw_process_captures": "removed", "method": "blocked-before-runtime-cleanup",
        }
        try:
            proof_bytes = _write_atomic(cleanup_path, proof)
        except EvidenceError:
            return False, {"status": "unverified"}, "cleanup_receipt_publish_failed"
        return True, {
            "status": "verified", "method": "blocked-before-runtime-cleanup",
            "cleanup_receipt_sha256": _digest_bytes(proof_bytes),
        }, None
    return False, {"status": "missing"}, "cleanup_receipt_missing"


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


def _read_optional_object(path: Path) -> tuple[dict[str, object] | None, str | None]:
    if not path.exists() and not path.is_symlink():
        return None, None
    try:
        value, data = _read_json(path, MAX_RECEIPT_BYTES)
    except EvidenceError as error:
        return None, error.args[0] if error.args else "proof_file_invalid"
    if not isinstance(value, dict):
        return None, "proof_file_malformed"
    return value, _digest_bytes(data)


def _task_session_bindings(source: dict[str, object]) -> list[dict[str, str]]:
    tasks = source.get("tasks")
    sessions = source.get("provider_sessions")
    if not isinstance(tasks, dict) or not isinstance(sessions, dict) or set(tasks) != set(sessions):
        return []
    result: list[dict[str, str]] = []
    for alias in sorted(tasks):
        task_id = tasks.get(alias)
        session_id = sessions.get(alias)
        if (not isinstance(alias, str) or SAFE_TEXT.fullmatch(alias) is None
                or not isinstance(task_id, str) or _safe_proof_text(task_id) is None
                or not isinstance(session_id, str) or _safe_proof_text(session_id) is None):
            return []
        result.append({"alias": alias, "task_id": task_id, "session_id": session_id})
    return result


def _expected_task_aliases(provider: str, scenario: str) -> tuple[str, ...]:
    """Return the authoritative task turns required by one passing case."""
    if scenario == "interactive":
        return ("interactive",)
    if scenario == "timeout-resume":
        return ("timeout_source", "timeout_resume")
    if provider == "codex:review":
        return ("fresh",)
    return ("fresh", "resume")


def _binary_identities(
        row: MatrixRow,
        case_directory: Path,
        source: dict[str, object] | None) -> dict[str, object]:
    binding, binding_digest = _read_optional_object(case_directory / "binding.json")
    hashes: dict[str, str] = {}
    versions: dict[str, str] = {}
    binding_matches = (binding is not None
                       and binding.get("provider") == row.definition.provider
                       and binding.get("mode") == row.definition.permission
                       and binding.get("scenario") == row.definition.scenario)
    provider_binding: dict[str, str] | None = None
    if binding_matches:
        for key, value in binding.items():
            if key.endswith("_sha256") and isinstance(value, str) and HEX_DIGEST.fullmatch(value):
                hashes[key] = value
            elif key.endswith("_version") and isinstance(value, str):
                safe = _safe_proof_text(value)
                if safe is not None:
                    versions[key] = safe
        provider_task_id = binding.get("provider_task_id")
        provider_executable = _safe_runtime_executable(binding.get("provider_executable"))
        provider_version = _safe_proof_text(binding.get("provider_version"))
        provider_sha256 = binding.get("provider_sha256")
        if (binding.get("provider_identity_scope") == "first-admitted-task"
                and isinstance(provider_task_id, str)
                and re.fullmatch(r"[0-9a-f]{32}", provider_task_id) is not None
                and provider_executable is not None and provider_version is not None
                and isinstance(provider_sha256, str)
                and HEX_DIGEST.fullmatch(provider_sha256) is not None):
            provider_binding = {
                "task_id": provider_task_id,
                "executable": provider_executable,
                "version": provider_version,
                "sha256": provider_sha256,
            }
    provider_runtimes = _compact_provider_runtime_identities(source) if source is not None else None
    if source is not None:
        provider_versions = _compact_provider_versions(source)
        if provider_versions is not None:
            versions.update({f"provider:{key}": value for key, value in provider_versions.items()})
    source_tasks = source.get("tasks") if isinstance(source, dict) else None
    source_task_ids = (
        sorted(source_tasks.values())
        if isinstance(source_tasks, dict) and source_tasks
        and all(isinstance(value, str) and re.fullmatch(r"[0-9a-f]{32}", value)
                for value in source_tasks.values())
        else []
    )
    runtime_coverage = (provider_runtimes is not None
                        and sorted(provider_runtimes) == source_task_ids
                        and len(source_task_ids) == len(set(source_task_ids)))
    provider_binding_matches = (
        provider_binding is not None and provider_runtimes is not None
        and provider_runtimes.get(provider_binding["task_id"]) == {
            "executable": provider_binding["executable"],
            "version": provider_binding["version"],
            "sha256": provider_binding["sha256"],
        }
    )
    required_hashes = {
        "provider_sha256", "delegate_sha256", "runner_sha256", "pueue_sha256", "pueued_sha256",
    }
    state = ("complete" if binding_matches and required_hashes.issubset(hashes)
             and runtime_coverage and provider_binding_matches else "unavailable")
    result: dict[str, object] = {
        "state": state,
        "sha256": dict(sorted(hashes.items())),
        "task_ids": source_task_ids,
        "provider_binding": provider_binding,
        "provider_runtime_identities": provider_runtimes or {},
    }
    if versions:
        result["versions"] = dict(sorted(versions.items()))
    if binding_digest is not None:
        result["binding_receipt_sha256"] = binding_digest
    return result


def _source_proof(
        row: MatrixRow,
        source: dict[str, object] | None,
        cleanup: dict[str, object],
        case_directory: Path,
        cleanup_ok: bool,
        source_digest: str | None) -> dict[str, object]:
    source = source or {}
    diagnostics, diagnostics_digest = _read_optional_object(case_directory / "failure-diagnostics.json")
    source_gate = _safe_proof_text(source.get("gate"))
    source_provider = _safe_proof_text(source.get("provider"))
    source_mode = _safe_proof_text(source.get("mode")) or row.definition.permission
    source_scenario = _safe_proof_text(source.get("scenario")) or row.definition.scenario
    binding, binding_marker = _read_optional_object(case_directory / "binding.json")
    binding_error = binding_marker if binding is None else None
    requested_options = _source_requested_options(
        source, binding, require_binding=row.status == "PASS", binding_error=binding_error,
    )
    task_sessions = _task_session_bindings(source)
    task_ids = sorted({binding["task_id"] for binding in task_sessions})
    diagnostic_task_ids: list[str] = []
    if diagnostics is not None:
        diagnostic_task_ids = _task_identifier_values(
            diagnostics.get("task_ids", diagnostics.get("tasks", {})),
        )
    session_ids = sorted({binding["session_id"] for binding in task_sessions})
    sessions_by_alias = {
        binding["alias"]: binding["session_id"] for binding in task_sessions
    }
    binary = _binary_identities(row, case_directory, source)
    natural_shutdown = source.get("natural_daemon_shutdown")
    expected_aliases = set(_expected_task_aliases(
        row.definition.provider, row.definition.scenario,
    ))
    observed_aliases = {binding["alias"] for binding in task_sessions}
    session_continuity = (
        bool(task_sessions)
        and len(task_ids) == len(task_sessions)
        and len(session_ids) == 1
    )
    lifecycle: dict[str, object] = {
        "state": "complete" if cleanup_ok and natural_shutdown is True else "unresolved",
        "cleanup_status": cleanup.get("status"),
        "supervisor_shutdown_observed": cleanup.get("supervisor_shutdown_observed"),
        "natural_daemon_shutdown": natural_shutdown if isinstance(natural_shutdown, bool) else None,
    }
    if diagnostics_digest is not None and HEX_DIGEST.fullmatch(diagnostics_digest):
        lifecycle["failure_diagnostics_sha256"] = diagnostics_digest
    if row.status == "BLOCKED":
        proof_state = "blocked-before-runtime"
    elif row.status == "FAIL":
        proof_state = "unresolved"
    elif (source_gate == "acceptance-native" and source_provider == row.definition.provider
          and source_mode == row.definition.permission and source_scenario == row.definition.scenario
          and observed_aliases == expected_aliases and session_continuity
          and binary.get("state") == "complete"
          and lifecycle["state"] == "complete"
          and requested_options["requested_options_state"] == "recorded"):
        proof_state = "complete"
    else:
        proof_state = "unresolved"
    return {
        "state": proof_state,
        "source_receipt_sha256": source_digest,
        "final_status": row.status,
        "source_status": source.get("status") if isinstance(source.get("status"), str) else None,
        "gate": source_gate or "acceptance-native",
        "provider": source_provider or row.definition.provider,
        "permission": row.definition.permission,
        "mode": source_mode,
        "scenario": source_scenario,
        **requested_options,
        "task_ids": task_ids,
        "task_session_bindings": task_sessions,
        "diagnostic_task_ids": diagnostic_task_ids,
        "session_ids": {
            "state": "recorded" if task_sessions else "not-recorded",
            "values": session_ids,
            "by_alias": sessions_by_alias,
        },
        "binary_identities": binary,
        "lifecycle_proof": lifecycle,
    }


def _sanitize_case(
        row: MatrixRow,
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


def _retention_count(value: int | None) -> int:
    if value is None:
        raw = os.environ.get("DELEGATE_ACCEPTANCE_RESOLVED_RETENTION", str(DEFAULT_RETENTION_COUNT))
        if not re.fullmatch(r"[0-9]+", raw):
            raise EvidenceError("retention_count_invalid")
        value = int(raw)
    if value < 1 or value > MAX_RETENTION_COUNT:
        raise EvidenceError("retention_count_invalid")
    return value


def _resolved_receipt(path: Path) -> tuple[dict[str, object], int] | None:
    try:
        value, _ = _read_json(path, MAX_RECEIPT_BYTES)
    except EvidenceError:
        return None
    if not isinstance(value, dict) or value.get("kind") != AGGREGATE_KIND:
        return None
    try:
        validate_aggregate_receipt(path)
    except EvidenceError:
        return None
    status = value.get("status")
    cases = value.get("cases")
    aggregate_root = path.parent
    owner = value.get("aggregate_owner")
    if (not _identity_matches(aggregate_root, owner)
            or not isinstance(owner, dict)
            or owner.get("path") != str(aggregate_root.absolute())):
        return None
    case_root = aggregate_root / CASE_ROOT_NAME
    try:
        case_root_empty = not case_root.is_symlink() and not _bounded_children(case_root)
    except EvidenceError:
        return None
    if (status not in {"PASS", "BLOCKED"} or value.get("unresolved_case_count") != 0
            or value.get("matrix_complete") is not True
            or value.get("expected_case_count") != len(EXPECTED_CASES)
            or value.get("case_count") != len(EXPECTED_CASES)
            or not isinstance(cases, list) or len(cases) != len(EXPECTED_CASES)
            or any(not isinstance(case, dict) or case.get("evidence_status") != "RESOLVED"
                   or case.get("status") not in {"PASS", "BLOCKED"} for case in cases)
            or case_root.is_symlink()
            or (case_root.exists() and not case_root_empty)
            or not isinstance(value.get("completed_at_ns"), int)
            or not isinstance(value.get("terminal_evidence_published"), bool)
            or value.get("terminal_evidence_published") is not True):
        return None
    completed = value.get("completed_at_ns")
    order = completed if type(completed) is int and completed >= 0 else -1
    if order < 0:
        return None
    return value, order


def _aggregate_root_summary(entries: list[tuple[Path, dict[str, object], int]]) -> dict[str, object]:
    summaries = [_tree_summary(path) for path, _, _ in entries]
    complete = all(summary.get("complete") is True for summary in summaries)
    file_count = sum(int(summary.get("file_count", 0)) for summary in summaries)
    byte_count = sum(int(summary.get("byte_count", 0)) for summary in summaries)
    result: dict[str, object] = {
        "root_count": len(entries), "file_count": file_count, "byte_count": byte_count,
        "complete": complete,
        "count": len(entries), "bytes": byte_count,
    }
    return result


def _current_retention_entry(
        root: Path,
        receipt: dict[str, object]) -> tuple[Path, dict[str, object], int] | None:
    cases = receipt.get("cases")
    completed = receipt.get("completed_at_ns")
    if (receipt.get("case_results_verdict") not in {"PASS", "BLOCKED"}
            or receipt.get("matrix_complete") is not True
            or not isinstance(cases, list) or len(cases) != len(EXPECTED_CASES)
            or any(not isinstance(case, dict) or case.get("evidence_status") != "RESOLVED"
                   or case.get("status") not in {"PASS", "BLOCKED"} for case in cases)
            or type(completed) is not int or completed < 0
            or not _identity_matches(root, receipt.get("aggregate_owner"))):
        return None
    return root, receipt, completed


def _retention_authorization(
        current_owner: dict[str, object],
        configured_count: int,
        path: Path,
        identity: dict[str, object],
        before: dict[str, object]) -> str:
    return _digest_bytes(canonical_json({
        "kind": "native-acceptance-retention-victim",
        "current_owner": current_owner,
        "configured_count": configured_count,
        "path": str(path.absolute()),
        "identity": identity,
        "before": before,
    }))


def _retention_plan_result(plan: dict[str, object], plan_path: Path) -> dict[str, object]:
    result = plan.get("result")
    if not isinstance(result, dict):
        raise EvidenceError("retention_plan_invalid")
    return {
        **result,
        "plan_path": str(plan_path.resolve()),
        "plan_sha256": _digest_file(plan_path, MAX_RECEIPT_BYTES),
    }


def _validate_retention_plan(
        plan: object,
        root: Path,
        current_receipt: dict[str, object],
        count: int) -> dict[str, object]:
    current_owner = current_receipt.get("aggregate_owner")
    current_entry = _current_retention_entry(root, current_receipt)
    expected_counted = current_entry is not None
    expected_order = current_entry[2] if current_entry is not None else None
    if (not isinstance(plan, dict) or plan.get("schema_version") != SCHEMA_VERSION
            or plan.get("kind") != "native-acceptance-retention-plan"
            or plan.get("state") not in {"prepared", "partial", "complete"}
            or plan.get("configured_count") != count
            or plan.get("current_root") != str(root.absolute())
            or plan.get("current_owner") != current_owner
            or plan.get("current_counted") is not expected_counted
            or plan.get("current_order") != expected_order
            or not isinstance(plan.get("retained"), list)
            or not isinstance(plan.get("victims"), list)):
        raise EvidenceError("retention_plan_invalid")
    return plan


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


def _reconcile_pending_retention(parent: Path, current_root: Path) -> None:
    for candidate in sorted(_bounded_children(parent), key=lambda item: item.name):
        if (candidate == current_root or not candidate.is_dir() or candidate.is_symlink()
                or not candidate.name.startswith("native-aggregate-")):
            continue
        plan_path = candidate / RETENTION_PLAN_NAME
        if not plan_path.exists() or plan_path.is_symlink():
            continue
        candidate_lock = acquire_exclusive_file_lock(
            candidate.parent / ("." + candidate.name + ".lock"),
        )
        try:
            plan, _ = _read_json(plan_path, MAX_RECEIPT_BYTES)
            if (not isinstance(plan, dict)
                    or plan.get("state") not in {"prepared", "partial", "complete"}):
                raise EvidenceError("retention_plan_invalid")
            receipt_path = candidate / AGGREGATE_RECEIPT_NAME
            receipt, _ = _read_json(receipt_path, MAX_RECEIPT_BYTES)
            configured = plan.get("configured_count")
            if not isinstance(receipt, dict) or type(configured) is not int:
                raise EvidenceError("retention_plan_invalid")
            if receipt.get("terminal_evidence_published") is True:
                if plan.get("state") != "complete":
                    raise EvidenceError("aggregate_retention_plan_invalid")
                continue
            result = _enforce_retention_locked(candidate, receipt, configured)
            if result.get("status") != "complete":
                raise EvidenceError("retention_pending_plan_incomplete")
            _publish_completed_retention_receipt(candidate, receipt_path, receipt, result)
        finally:
            release_exclusive_file_lock(candidate_lock)


def _enforce_retention(root: Path, current_receipt: dict[str, object], count: int) -> dict[str, object]:
    """Run retention while the caller holds the parent coordinator lock."""
    parent = root.parent
    _reconcile_pending_retention(parent, root)
    return _enforce_retention_locked(root, current_receipt, count)


def _enforce_retention_locked(
        root: Path, current_receipt: dict[str, object], count: int) -> dict[str, object]:
    parent = root.parent
    current_entry = _current_retention_entry(root, current_receipt)
    current_owner = current_receipt.get("aggregate_owner")
    if (not isinstance(current_owner, dict)
            or not _identity_matches(root, current_owner)):
        raise EvidenceError("retention_current_root_invalid")
    plan_path = root / RETENTION_PLAN_NAME
    cleanup_root = root / RETENTION_CLEANUP_ROOT_NAME
    cleanup_root.mkdir(mode=0o700, exist_ok=True)
    _ensure_directory(cleanup_root)

    if plan_path.exists() or plan_path.is_symlink():
        if plan_path.is_symlink():
            raise EvidenceError("retention_plan_invalid")
        plan_value, _ = _read_json(plan_path, MAX_RECEIPT_BYTES)
        plan = _validate_retention_plan(plan_value, root, current_receipt, count)
        if plan["state"] == "complete":
            return _retention_plan_result(plan, plan_path)
    else:
        old: list[tuple[Path, dict[str, object], int]] = []
        try:
            children = sorted(_bounded_children(parent), key=lambda item: item.name)
        except EvidenceError as error:
            raise EvidenceError("retention_parent_unreadable") from error
        for child in children:
            if child == root or not child.name.startswith("native-aggregate-"):
                continue
            if not child.is_dir() or child.is_symlink():
                continue
            resolved = _resolved_receipt(child / AGGREGATE_RECEIPT_NAME)
            if resolved is not None:
                old.append((child, resolved[0], resolved[1]))
        old.sort(key=lambda item: (item[2], item[0].name), reverse=True)
        retained_slots = max(0, count - 1) if current_entry is not None else count
        retained_old = old[:retained_slots]
        victims_raw = old[retained_slots:]
        retained: list[dict[str, object]] = []
        victims: list[dict[str, object]] = []
        for path, candidate, order in retained_old:
            identity = candidate.get("aggregate_owner")
            if not isinstance(identity, dict) or not _identity_matches(path, identity):
                raise EvidenceError("retention_identity_changed")
            retained.append({"path": str(path.absolute()), "identity": identity, "order": order})
        for index, (path, candidate, order) in enumerate(victims_raw):
            identity = candidate.get("aggregate_owner")
            before = _tree_summary(path)
            if (not isinstance(identity, dict) or not _identity_matches(path, identity)
                    or before.get("complete") is not True):
                raise EvidenceError("retention_candidate_invalid")
            journal = cleanup_root / f"victim-{index:02d}.json"
            authorization = _retention_authorization(
                current_owner, count, path, identity, before,
            )
            victims.append({
                "path": str(path.absolute()),
                "identity": identity,
                "order": order,
                "before": before,
                "journal_path": str(journal.absolute()),
                "authorization_sha256": authorization,
                "journal_sha256": None,
            })
        plan = {
            "schema_version": SCHEMA_VERSION,
            "kind": "native-acceptance-retention-plan",
            "state": "prepared",
            "configured_count": count,
            "current_root": str(root.absolute()),
            "current_owner": current_owner,
            "current_counted": current_entry is not None,
            "current_order": current_entry[2] if current_entry is not None else None,
            "retained": retained,
            "victims": victims,
            "result": None,
        }
        _write_atomic(plan_path, plan)

    errors: list[str] = []
    for victim in plan["victims"]:
        if not isinstance(victim, dict):
            raise EvidenceError("retention_plan_invalid")
        path = Path(str(victim.get("path", "")))
        identity = victim.get("identity")
        before = victim.get("before")
        journal = Path(str(victim.get("journal_path", "")))
        authorization = victim.get("authorization_sha256")
        if (not isinstance(identity, dict) or not isinstance(authorization, str)
                or HEX_DIGEST.fullmatch(authorization) is None
                or not isinstance(before, dict)
                or authorization != _retention_authorization(
                    current_owner, count, path, identity, before,
                )):
            raise EvidenceError("retention_plan_invalid")
        victim_lock = -1
        try:
            victim_lock = acquire_exclusive_file_lock(
                path.parent / ("." + path.name + ".lock"),
            )
            if not journal.exists() and not journal.is_symlink():
                prepare_directory_cleanup(
                    journal,
                    "resolved native aggregate retention",
                    identity,
                    parent,
                    authorization,
                )
            else:
                existing = read_directory_cleanup(journal, authorization)
                if existing.get("target") != identity:
                    raise RuntimeError("retention cleanup target identity changed")
            completed = resume_directory_cleanup(journal, authorization, identity)
            if completed.get("state") != "complete" or completed.get("target") != identity:
                raise RuntimeError("retention cleanup did not complete")
            victim["journal_sha256"] = cleanup_journal_digest(journal)
            _write_atomic(plan_path, plan)
            if path.exists() or path.is_symlink():
                raise RuntimeError("retention target pathname was reoccupied")
        except (OSError, RuntimeError, ValueError) as error:
            errors.append(f"{path.name}:{str(error)[:96]}")
            break
        finally:
            if victim_lock >= 0:
                release_exclusive_file_lock(victim_lock)

    retained_entries: list[tuple[Path, dict[str, object], int]] = []
    if current_entry is not None:
        retained_entries.append(current_entry)
    for retained in plan["retained"]:
        if not isinstance(retained, dict):
            raise EvidenceError("retention_plan_invalid")
        path = Path(str(retained.get("path", "")))
        identity = retained.get("identity")
        order = retained.get("order")
        resolved = _resolved_receipt(path / AGGREGATE_RECEIPT_NAME)
        if (not isinstance(identity, dict) or type(order) is not int
                or not _identity_matches(path, identity) or resolved is None):
            errors.append(f"{path.name}:retention_retained_root_changed")
            continue
        retained_entries.append((path, resolved[0], order))
    after_summary = _aggregate_root_summary(retained_entries)
    before_summary = _summary_with_removed(after_summary, plan["victims"])
    pruned = [
        {
            "path": victim["path"],
            "before": victim["before"],
            "journal_path": victim["journal_path"],
            "journal_sha256": victim["journal_sha256"],
        }
        for victim in plan["victims"]
        if isinstance(victim, dict) and isinstance(victim.get("journal_sha256"), str)
    ]
    result = {
        "configured_count": count,
        "before": before_summary,
        "after": after_summary,
        "pruned": pruned,
        "errors": errors,
        "status": "complete" if (not errors and len(retained_entries) <= count
                                   and len(pruned) == len(plan["victims"])
                                   and before_summary.get("complete") is True
                                   and after_summary.get("complete") is True) else "partial",
    }
    plan["state"] = "complete" if result["status"] == "complete" else "partial"
    plan["result"] = result
    _write_atomic(plan_path, plan)
    return _retention_plan_result(plan, plan_path)


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


def _publish_completed_retention_receipt(
        root: Path,
        receipt_path: Path,
        receipt: dict[str, object],
        retention_result: dict[str, object]) -> dict[str, object]:
    """Publish one terminal aggregate after its retention plan is durable."""
    if retention_result.get("status") != "complete":
        raise EvidenceError("aggregate_retention_incomplete")
    if receipt.get("terminal_evidence_published") is not False:
        raise EvidenceError("aggregate_terminal_receipt_immutable")
    verdict = receipt.get("case_results_verdict")
    cases = receipt.get("cases")
    policy = receipt.get("policy")
    if (verdict not in {"PASS", "BLOCKED", "FAIL"}
            or not isinstance(cases, list)
            or not isinstance(policy, dict)
            or type(policy.get("require_at_least_one_pass")) is not bool):
        raise EvidenceError("aggregate_receipt_invalid")
    unresolved = [
        case["case_id"] for case in cases
        if isinstance(case, dict) and case.get("evidence_status") == "UNRESOLVED"
        and isinstance(case.get("case_id"), str)
    ]
    receipt["retention"] = retention_result
    receipt["terminal_evidence_published"] = True
    receipt["status"] = verdict
    receipt["unresolved_case_count"] = len(unresolved)
    receipt["unresolved_cases"] = unresolved
    receipt["exit_code"] = _aggregate_exit(
        verdict, verdict, bool(policy["require_at_least_one_pass"]),
    )
    evidence = receipt.get("evidence")
    if not isinstance(evidence, dict):
        raise EvidenceError("aggregate_evidence_invalid")
    evidence["case_raw"] = {
        "before": evidence.get("case_raw", {}).get("before")
        if isinstance(evidence.get("case_raw"), dict) else _empty_summary(),
        "after": _tree_summary(root / CASE_ROOT_NAME),
    }
    evidence["compact_receipts"] = _tree_summary(root / COMPACT_RECEIPT_ROOT_NAME)
    _write_atomic(receipt_path, receipt)
    validate_aggregate_receipt(
        receipt_path,
        expected_tag=receipt.get("tag") if isinstance(receipt.get("tag"), str) else None,
        expected_source_revision=(
            receipt.get("source_revision")
            if isinstance(receipt.get("source_revision"), str) else None
        ),
    )
    return receipt


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

    existing_nonterminal: dict[str, object] | None = None
    if receipt_path.exists() or receipt_path.is_symlink():
        if receipt_path.is_symlink():
            raise EvidenceError("aggregate_receipt_invalid")
        existing_value, _ = _read_json(receipt_path, MAX_RECEIPT_BYTES)
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
            return existing, int(existing["exit_code"])
        if existing_value.get("terminal_evidence_published") is not False:
            raise EvidenceError("aggregate_existing_receipt_invalid")
        existing_nonterminal = existing_value

    rows = _parse_matrix(matrix_path)
    raw_before = _tree_summary(cases_root)
    compact_cases: list[dict[str, object]] = []
    unresolved: list[str] = []
    for row in rows:
        candidate_case_directory = aggregate_root / row.definition.case_output
        compact_path = receipts_root / f"{row.definition.case_id}.json"

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
                compact_cases.append(_case_reference(
                    row, "RESOLVED", compact_path, replayed_digest, replayed,
                ))
                continue
            if isinstance(replayed, dict) and replayed.get("terminal_state") == "published-before-cleanup":
                replayed_case = _replay_prepared_case(
                    compact_path, candidate_case_directory, cases_root, cleanup_root, row,
                )
                if replayed_case is not None:
                    compact_cases.append(replayed_case)
                    continue
                raise EvidenceError("case_cleanup_replay_invalid")
            if (replayed.get("terminal_state") == "published-unresolved"
                    and replayed.get("evidence_status") == "UNRESOLVED"):
                compact_cases.append(_case_reference(
                    row, "UNRESOLVED", compact_path, replayed_digest, replayed,
                ))
                unresolved.append(row.definition.case_id)
                continue
            raise EvidenceError("case_compact_receipt_state_invalid")

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
        except EvidenceError as error:
            cleanup_ok = False
            cleanup = {"status": "unverified"}
            evidence_error = error.args[0] if error.args else "case_evidence_invalid"
            proof = _source_proof(row, source, cleanup, case_directory, cleanup_ok, source_digest) \
                if case_directory.exists() and not case_directory.is_symlink() else {
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
                    row, source, source_digest, cleanup, before, before, "RESOLVED", None,
                    proof, creation_identity, "published-before-cleanup", False,
                )
                _prepare_case_cleanup(
                    compact_path, prepared, cleanup_root, creation_identity,
                )
                resolved = _replay_prepared_case(
                    compact_path, case_directory, cases_root, cleanup_root, row,
                )
                if resolved is None:
                    raise EvidenceError("directory_owner_cleanup_failed")
                compact_cases.append(resolved)
                continue

        after = (_tree_summary(case_directory)
                 if case_directory.exists() and not case_directory.is_symlink()
                 else _empty_summary())
        evidence_status = "UNRESOLVED"
        if evidence_status == "UNRESOLVED":
            unresolved.append(row.definition.case_id)
        compact = _sanitize_case(
            row, source, source_digest, cleanup, before, after, evidence_status, evidence_error,
            proof, creation_identity,
            "published-unresolved",
            False,
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
        compact_cases.append(compact_case)

    rows_tuple = tuple(rows)
    result_verdict = _case_result_verdict(rows_tuple, bool(unresolved))
    aggregate_status = result_verdict
    if result_verdict == "FAIL":
        aggregate_status = "FAIL"
    retention = _retention_count(retention_count)
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
    # The provisional receipt makes this aggregate visible to the retention scan.
    _write_atomic(receipt_path, receipt)
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
        _write_atomic(receipt_path, receipt)
        raise EvidenceError("aggregate_retention_incomplete")
    receipt = _publish_completed_retention_receipt(
        aggregate_root, receipt_path, receipt, retention_result,
    )
    return receipt, int(receipt["exit_code"])


def _validate_case_receipt(root: Path, case: dict[str, object]) -> None:
    required = {"case_id", "provider", "permission", "scenario", "status", "exit_code",
                "evidence_status", "receipt_path", "receipt_sha256", "binary_identities"}
    if set(case) < required:
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
    if case["evidence_status"] not in {"RESOLVED", "UNRESOLVED"}:
        raise EvidenceError("aggregate_case_evidence_status_invalid")
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
            or diagnostic_task_ids != sorted(set(diagnostic_task_ids))
            or any(_safe_proof_text(item) is None for item in diagnostic_task_ids)):
        raise EvidenceError("aggregate_case_diagnostic_task_ids_invalid")
    sessions = proof.get("session_ids")
    if (not isinstance(sessions, dict) or sessions.get("state") not in {"recorded", "not-recorded"}
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
    binaries = proof.get("binary_identities")
    if (not isinstance(binaries, dict) or binaries.get("state") not in {"complete", "unavailable"}
            or not isinstance(binaries.get("sha256"), dict)
            or any(not isinstance(key, str) or not isinstance(value, str)
                   or HEX_DIGEST.fullmatch(value) is None for key, value in binaries["sha256"].items())):
        raise EvidenceError("aggregate_case_binary_identity_invalid")
    binary_task_ids = binaries.get("task_ids")
    if (not isinstance(binary_task_ids, list) or len(binary_task_ids) > 64
            or binary_task_ids != sorted(set(binary_task_ids))
            or any(not isinstance(task_id, str)
                   or re.fullmatch(r"[0-9a-f]{32}", task_id) is None
                   for task_id in binary_task_ids)):
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
    lifecycle = proof.get("lifecycle_proof")
    if not isinstance(lifecycle, dict) or lifecycle.get("state") not in {"complete", "unresolved"}:
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


def validate_aggregate_receipt(
        path: Path,
        *,
        expected_tag: str | None = None,
        expected_source_revision: str | None = None) -> tuple[dict[str, object], str]:
    value, data = _read_json(path, MAX_RECEIPT_BYTES)
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
    if status not in {"PASS", "BLOCKED", "FAIL"}:
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
    _validate_retention(value.get("retention"), aggregate_root, value)
    return value, _digest_bytes(data)


def _write_bytes_atomic(path: Path, data: bytes, limit: int = MAX_RECEIPT_BYTES) -> None:
    if len(data) > limit:
        raise EvidenceError("staged_evidence_too_large")
    _reject_path_components(path)
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        os.chmod(path, stat.S_IRUSR | stat.S_IWUSR)
        _fsync_directory(path.parent)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def stage_evidence(
        aggregate_path: Path,
        output: Path,
        *,
        archive_receipt: Path | None = None,
        native_receipt: Path | None = None,
        expected_tag: str | None = None,
        expected_source_revision: str | None = None) -> dict[str, object]:
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
        )
    finally:
        release_exclusive_file_lock(lock_fd)


def _stage_evidence_locked(
        aggregate_path: Path,
        output: Path,
        *,
        archive_receipt: Path | None = None,
        native_receipt: Path | None = None,
        expected_tag: str | None = None,
        expected_source_revision: str | None = None) -> dict[str, object]:
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
    if output.exists() or output.is_symlink():
        if output.is_symlink() or not output.is_dir():
            raise EvidenceError("stage_output_invalid")
        try:
            if _bounded_children(output):
                raise EvidenceError("stage_output_not_empty")
        except EvidenceError:
            raise
        except OSError as error:
            raise EvidenceError("stage_output_unreadable") from error
    else:
        output.mkdir(mode=0o700, parents=True)
    case_output = output / "case-receipts"
    case_output.mkdir(mode=0o700)
    cleanup_output = output / "cleanup-journals"
    retention_output = output / "retention"
    retention_output.mkdir(mode=0o700)
    _write_bytes_atomic(output / AGGREGATE_RECEIPT_NAME, _bounded_file_bytes(
        aggregate_path, MAX_RECEIPT_BYTES,
    ))
    files: list[dict[str, str]] = [{"path": AGGREGATE_RECEIPT_NAME, "sha256": aggregate_digest}]
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
        _write_bytes_atomic(case_output / name, data)
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
            cleanup_output.mkdir(mode=0o700, exist_ok=True)
            _write_bytes_atomic(cleanup_output / journal_name, journal_data)
            journal_entry = {
                "case_id": str(case["case_id"]),
                "path": f"cleanup-journals/{journal_name}",
                "sha256": journal_digest,
                "authorization_sha256": deletion["authorization_sha256"],
            }
            cleanup_journals.append(journal_entry)
            files.append({"path": journal_entry["path"], "sha256": journal_digest})

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
    _write_bytes_atomic(output / retention_plan_path, retention_plan_data)
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
        _write_bytes_atomic(output / journal_path, journal_data)
        journal_entry = {
            "path": journal_path,
            "sha256": journal_digest,
            "authorization_sha256": victim["authorization_sha256"],
        }
        retention_journals.append(journal_entry)
        files.append({"path": journal_path, "sha256": journal_digest})
    release_cleanup_journals: list[dict[str, str]] = []
    for source, name in ((archive_receipt, "archive-receipt.json"),
                         (native_receipt, "native-acceptance-receipt.json")):
        if source is None:
            continue
        source = source.absolute()
        data = _bounded_file_bytes(source, MAX_RECEIPT_BYTES)
        _write_bytes_atomic(output / name, data)
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
            release_cleanup_output = output / "release-cleanup"
            release_cleanup_output.mkdir(mode=0o700, exist_ok=True)
            journal_name = name.removesuffix(".json") + "-cleanup.json"
            journal_path = f"release-cleanup/{journal_name}"
            _write_bytes_atomic(output / journal_path, journal_data)
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
    manifest: dict[str, object] = {
        "schema_version": SCHEMA_VERSION,
        "kind": "native-acceptance-sanitized-evidence",
        "aggregate_receipt": {"path": AGGREGATE_RECEIPT_NAME, "sha256": aggregate_digest},
        "case_count": len(EXPECTED_CASES),
        "case_receipts": case_receipts,
        "cleanup_journals": cleanup_journals,
        "retention": {
            "plan": {"path": retention_plan_path, "sha256": retention_plan_digest},
            "cleanup_journals": retention_journals,
        },
        "release_cleanup_journals": release_cleanup_journals,
        "file_count": len(files),
        "files": files,
        "credentials_in_receipt": False,
    }
    _write_atomic(output / "manifest.json", manifest)
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
        if _digest_file(output / item["path"], MAX_RECEIPT_BYTES) != item["sha256"]:
            raise EvidenceError("stage_manifest_digest_mismatch")
    return manifest


def _parse_optional_revision(value: str | None) -> str | None:
    if value is None or value == "":
        return None
    if not re.fullmatch(r"[0-9a-f]{40,64}", value):
        raise EvidenceError("source_revision_invalid")
    return value


def _command(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("list-cases")
    prepare = commands.add_parser("prepare-case")
    prepare.add_argument("--aggregate-root", required=True)
    prepare.add_argument("--case-id", required=True)
    aggregate = commands.add_parser("aggregate")
    aggregate.add_argument("--aggregate-root", required=True)
    aggregate.add_argument("--matrix")
    aggregate.add_argument("--receipt")
    aggregate.add_argument("--tag")
    aggregate.add_argument("--source-revision")
    aggregate.add_argument("--retention-count", type=int)
    aggregate.add_argument("--require-pass", action="store_true")
    validate = commands.add_parser("validate")
    validate.add_argument("--receipt", required=True)
    validate.add_argument("--tag")
    validate.add_argument("--source-revision")
    stage = commands.add_parser("stage")
    stage.add_argument("--aggregate-receipt", required=True)
    stage.add_argument("--output", required=True)
    stage.add_argument("--archive-receipt")
    stage.add_argument("--native-receipt")
    stage.add_argument("--tag")
    stage.add_argument("--source-revision")
    args = parser.parse_args(argv)
    if args.command == "list-cases":
        for case in EXPECTED_CASES:
            print("\t".join((case.case_id, case.provider, case.permission, case.scenario)))
        return 0
    if args.command == "prepare-case":
        directory = prepare_case_directory(Path(args.aggregate_root), args.case_id)
        print(json.dumps({"case_id": args.case_id, "case_directory": str(directory)}, sort_keys=True))
        return 0
    if args.command == "validate":
        source_revision = _parse_optional_revision(
            args.source_revision if args.source_revision is not None
            else os.environ.get("DELEGATE_ACCEPTANCE_SOURCE_REVISION"),
        )
        value, digest = validate_aggregate_receipt(
            Path(args.receipt).absolute(),
            expected_tag=args.tag or os.environ.get("DELEGATE_ACCEPTANCE_TAG") or None,
            expected_source_revision=source_revision,
        )
        print(json.dumps({
            "aggregate_receipt": str(Path(args.receipt).absolute()),
            "status": value["status"], "case_count": value["case_count"],
            "sha256": digest,
        }, sort_keys=True))
        return 0
    if args.command == "stage":
        source_revision = _parse_optional_revision(
            args.source_revision if args.source_revision is not None
            else os.environ.get("DELEGATE_ACCEPTANCE_SOURCE_REVISION"),
        )
        manifest = stage_evidence(
            Path(args.aggregate_receipt), Path(args.output),
            archive_receipt=Path(args.archive_receipt) if args.archive_receipt else None,
            native_receipt=Path(args.native_receipt) if args.native_receipt else None,
            expected_tag=args.tag or os.environ.get("DELEGATE_ACCEPTANCE_TAG") or None,
            expected_source_revision=source_revision,
        )
        print(json.dumps({"output": str(Path(args.output).absolute()), **manifest}, sort_keys=True))
        return 0
    root = Path(args.aggregate_root).absolute()
    matrix = Path(args.matrix).absolute() if args.matrix else root / CASE_MATRIX_NAME
    receipt = Path(args.receipt).absolute() if args.receipt else root / AGGREGATE_RECEIPT_NAME
    source_revision = _parse_optional_revision(
        args.source_revision if args.source_revision is not None
        else os.environ.get("DELEGATE_ACCEPTANCE_SOURCE_REVISION"),
    )
    value, exit_code = build_aggregate(
        root, matrix, receipt,
        tag=args.tag or os.environ.get("DELEGATE_ACCEPTANCE_TAG") or None,
        source_revision=source_revision,
        require_pass=args.require_pass, retention_count=args.retention_count,
    )
    print(json.dumps({
        "aggregate_receipt": str(receipt), "status": value["status"], "exit_code": exit_code,
        "case_count": value["case_count"], "unresolved_case_count": value["unresolved_case_count"],
    }, sort_keys=True))
    return exit_code


def main(argv: list[str] | None = None) -> int:
    try:
        return _command(argv if argv is not None else sys.argv[1:])
    except (EvidenceError, OSError, ValueError) as error:
        print(f"native acceptance evidence failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
