"""Filesystem for exact release verification."""

from __future__ import annotations

from pathlib import Path
import hashlib
import json
import os
import re
import stat
import tempfile
from ..evidence.bounded_files import BoundedReadError
from ..directories.journal import acquire_exclusive_file_lock
from ..evidence.bounded_files import read_bounded_regular_file
from .contract import (MAX_RECEIPT, MAX_TREE_ENTRIES, VerificationError)


def _fsync_directory(path: Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    except OSError as error:
        raise VerificationError("receipt_directory_sync_failed", "write-receipt") from error


def _bounded_file_bytes(path: Path, limit: int, code: str) -> bytes:
    _reject_parent_components(path, code)
    _reject_symlink_components(path, code)
    try:
        return read_bounded_regular_file(path, limit)
    except BoundedReadError:
        raise VerificationError(code, "bounded-read") from None


def bounded_digest(path: Path, limit: int, code: str = "file_too_large") -> str:
    return hashlib.sha256(_bounded_file_bytes(path, limit, code)).hexdigest()


def _reject_parent_components(path: Path, code: str) -> None:
    if ".." in path.parts:
        raise VerificationError(code, "validate-paths")


def _reject_symlink_components(path: Path, code: str) -> None:
    absolute = path.absolute()
    current = Path(absolute.anchor)
    for part in absolute.parts[1:]:
        current /= part
        try:
            metadata = current.lstat()
        except FileNotFoundError:
            break
        except OSError as error:
            raise VerificationError(code, "validate-paths") from error
        if stat.S_ISLNK(metadata.st_mode):
            raise VerificationError(code, "validate-paths")


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


def write_receipt(path: Path, receipt: dict) -> None:
    data = (json.dumps(receipt, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n").encode()
    if len(data) > MAX_RECEIPT:
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


def read_receipt(path: Path) -> tuple[dict, str]:
    try:
        data = _bounded_file_bytes(path, MAX_RECEIPT, "archive_receipt_invalid")
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
