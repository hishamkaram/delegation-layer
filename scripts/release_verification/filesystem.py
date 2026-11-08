"""Filesystem for exact release verification."""

from __future__ import annotations
from scripts.acceptance.evidence.source import _bounded_file_bytes, _reject_parent_components, _reject_symlink_components

from pathlib import Path
import errno
import hashlib
import json
import os
import re
import tempfile
from scripts.acceptance.evidence.bounded_files import BoundedReadError
from scripts.acceptance.directories.journal import acquire_exclusive_file_lock
from scripts.acceptance.evidence.bounded_files import FileFingerprint
from scripts.acceptance.evidence.bounded_files import fingerprint_bounded_regular_file
from scripts.release_verification.contract import MAX_RECEIPT as MAX_RECEIPT
from scripts.release_verification.contract import MAX_RELEASE_RECEIPT_BYTES
from scripts.release_verification.contract import MAX_TREE_ENTRIES
from scripts.acceptance.evidence.source import VerificationError


def _fsync_directory(path: Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    except OSError as error:
        raise VerificationError("receipt_directory_sync_failed", "write-receipt") from error




def bounded_fingerprint(path: Path, limit: int, code: str) -> FileFingerprint:
    """Return the size and SHA-256 of one bounded regular file without holding its bytes.

    The path guards and the pinned read are those of ``_bounded_file_bytes``; only
    the read's sink differs, so memory is bounded by the read chunk size, O(chunk), never the
    file's size, and every refusal keeps its identity.
    """
    _reject_parent_components(path, code)
    _reject_symlink_components(path, code)
    try:
        return fingerprint_bounded_regular_file(path, limit)
    except BoundedReadError:
        raise VerificationError(code, "bounded-read") from None


def bounded_digest(path: Path, limit: int, code: str = "file_too_large") -> str:
    return bounded_fingerprint(path, limit, code).sha256






def create_new_file(path: Path, code: str, *, stage: str) -> int:
    """Create one owner-only regular file and return its open descriptor; the caller closes it.

    Links and existing paths are never reused. A path-guard refusal reports the
    ``validate-paths`` stage; a failure to create the leaf reports the caller's ``stage``.
    """
    _reject_parent_components(path, code)
    _reject_symlink_components(path, code)
    try:
        return os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0),
                       0o600)
    except OSError:
        raise VerificationError(code, stage) from None


def write_all(descriptor: int, data: bytes) -> None:
    """Write every byte of ``data`` to ``descriptor``; the caller maps the raw ``OSError``.

    A partial write continues with the rest. A write that reports no progress
    raises ``OSError(EIO)`` instead of looping forever.
    """
    view = memoryview(data)
    while view:
        written = os.write(descriptor, view)
        if written <= 0:
            raise OSError(errno.EIO, "write made no progress")
        view = view[written:]


def write_new_file(path: Path, data: bytes, *, path_code: str, write_code: str, stage: str) -> None:
    """Create ``path`` owner-only, write and sync exactly ``data``, and close it exactly once.

    A path refusal is ``create_new_file``'s. A write or sync ``OSError`` becomes
    ``write_code``. Any primary exception, including an interrupt, closes the
    descriptor once and is re-raised as the same object; a close failure on that
    path only adds the errno note ``file close failed: <ERRNO>``. A close-only
    failure becomes ``write_code``. Nothing is retried or removed: the file stays
    with the caller on every outcome.
    """
    descriptor = create_new_file(path, path_code, stage=stage)
    try:
        try:
            write_all(descriptor, data)
            os.fsync(descriptor)
        except OSError:
            raise VerificationError(write_code, stage) from None
    except BaseException as primary:
        try:
            os.close(descriptor)
        except OSError as close_error:
            primary.add_note("file close failed: " + errno.errorcode.get(close_error.errno or 0, "unknown"))
        raise
    try:
        os.close(descriptor)
    except OSError:
        raise VerificationError(write_code, stage) from None


def _bounded_children(path: Path, limit: int = MAX_TREE_ENTRIES) -> list[Path]:
    children: list[Path] = []
    try:
        with os.scandir(path) as entries:
            for entry in entries:
                if len(children) >= limit:
                    raise VerificationError("directory_too_large", "bounded-walk")
                children.append(Path(entry.path))
    except VerificationError:
        raise
    except OSError:
        raise VerificationError("directory_unreadable", "bounded-walk") from None
    return children


def write_receipt(path: Path, receipt: dict, *, max_bytes: int = MAX_RELEASE_RECEIPT_BYTES) -> None:
    data = (json.dumps(receipt, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n").encode()
    if len(data) > max_bytes:
        raise VerificationError("receipt_too_large", "write-receipt")
    _reject_parent_components(path, "unsafe_receipt_path")
    _reject_symlink_components(path, "unsafe_receipt_path")
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.replace(name, path)
        _fsync_directory(path.parent)
    finally:
        try:
            os.unlink(name)
        except FileNotFoundError:
            pass


def _acquire_receipt_lock(path: Path) -> int:
    """Serialize one release receipt from replay through terminal publication."""
    _reject_parent_components(path, "unsafe_receipt_path")
    _reject_symlink_components(path, "unsafe_receipt_path")
    path.parent.mkdir(parents=True, exist_ok=True)
    _reject_symlink_components(path.parent, "unsafe_receipt_path")
    return acquire_exclusive_file_lock(path.with_name("." + path.name + ".lock"))


def read_receipt(path: Path, *, max_bytes: int = MAX_RELEASE_RECEIPT_BYTES) -> tuple[dict, str]:
    try:
        data = _bounded_file_bytes(path, max_bytes, "archive_receipt_invalid")
        value = json.loads(data)
    except VerificationError as error:
        code = "archive_receipt_missing_or_invalid" if not path.exists() else "archive_receipt_invalid"
        raise VerificationError(code, "read-archive-receipt") from error
    except (UnicodeError, json.JSONDecodeError, RecursionError):
        raise VerificationError("archive_receipt_missing_or_invalid", "read-archive-receipt") from None
    if not isinstance(value, dict) or value.get("schema_version") != 1:
        raise VerificationError("archive_receipt_invalid", "read-archive-receipt")
    return value, hashlib.sha256(data).hexdigest()


def exit_code(path: str | None) -> int | None:
    if not path:
        return None
    try:
        raw = _bounded_file_bytes(Path(path), 16, "exit_code_file_invalid").decode("ascii")
    except (VerificationError, UnicodeError):
        return None
    return int(raw.strip()) if re.fullmatch(r"-?[0-9]{1,3}\n?", raw) else None
