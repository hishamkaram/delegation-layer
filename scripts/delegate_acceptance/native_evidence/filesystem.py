"""Filesystem for native acceptance evidence."""

from __future__ import annotations

from pathlib import Path
import hashlib
import json
import os
import stat
import tempfile
from ..evidence.bounded_files import BoundedReadError, ReadFailure, read_bounded_regular_file
from .contract import (
    EvidenceError,
    MAX_INPUT_BYTES,
    MAX_RECEIPT_BYTES,
    MAX_WALK_BYTES,
    MAX_WALK_ENTRIES,
    _empty_summary,
    canonical_json,
)


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
    _publish_bytes(path, data)
    return data


def _bounded_file_bytes(path: Path, limit: int) -> bytes:
    _reject_path_components(path)
    try:
        return read_bounded_regular_file(path, limit)
    except BoundedReadError as error:
        reasons = {
            ReadFailure.INVALID: "evidence_file_invalid",
            ReadFailure.TOO_LARGE: "evidence_file_too_large",
            ReadFailure.CHANGED: "evidence_file_changed",
            ReadFailure.UNREADABLE: "evidence_file_unreadable",
        }
        raise EvidenceError(reasons[error.reason]) from error


def _read_json(path: Path, limit: int = MAX_INPUT_BYTES) -> tuple[object, bytes]:
    try:
        data = _bounded_file_bytes(path, limit)
        value = json.loads(data)
    except EvidenceError:
        raise
    except (UnicodeError, json.JSONDecodeError, RecursionError) as error:
        raise EvidenceError("evidence_file_invalid") from error
    return value, data


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


def _write_bytes_atomic(path: Path, data: bytes, limit: int = MAX_RECEIPT_BYTES) -> None:
    if len(data) > limit:
        raise EvidenceError("staged_evidence_too_large")
    _publish_bytes(path, data)


def _publish_bytes(path: Path, data: bytes) -> None:
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
