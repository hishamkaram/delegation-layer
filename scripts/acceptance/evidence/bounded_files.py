"""One identity-checked, bounded regular-file reader for acceptance evidence."""

from collections.abc import Callable
from dataclasses import dataclass
from enum import Enum
import hashlib
import os
from pathlib import Path
import stat


class ReadFailure(Enum):
    INVALID = "invalid"
    TOO_LARGE = "too_large"
    CHANGED = "changed"
    UNREADABLE = "unreadable"


class BoundedReadError(Exception):
    def __init__(self, reason: ReadFailure):
        super().__init__(reason.value)
        self.reason = reason


@dataclass(frozen=True)
class FileFingerprint:
    size: int
    sha256: str


def _read_pinned(path: Path, limit: int, sink: Callable[[bytes], None]) -> int:
    """Pin the opened file to its observed identity, feed sink chunks and return the byte count.

    The pin covers the opened inode only, not the ancestors and not later writes to it.
    """
    try:
        metadata = path.lstat()
        if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode):
            raise BoundedReadError(ReadFailure.INVALID)
        if metadata.st_size > limit:
            raise BoundedReadError(ReadFailure.TOO_LARGE)
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        try:
            opened = os.fstat(descriptor)
            if (not stat.S_ISREG(opened.st_mode) or opened.st_size > limit
                    or (opened.st_dev, opened.st_ino) != (metadata.st_dev, metadata.st_ino)):
                raise BoundedReadError(ReadFailure.CHANGED)
            total = 0
            remaining = limit
            while remaining:
                chunk = os.read(descriptor, min(1024 * 1024, remaining))
                if not chunk:
                    break
                sink(chunk)
                total += len(chunk)
                remaining -= len(chunk)
            if os.read(descriptor, 1):
                raise BoundedReadError(ReadFailure.TOO_LARGE)
            return total
        finally:
            os.close(descriptor)
    except BoundedReadError:
        raise
    except (OSError, UnicodeError) as error:
        raise BoundedReadError(ReadFailure.UNREADABLE) from error


def read_bounded_regular_file(path: Path, limit: int) -> bytes:
    """Pin the opened file to its observed identity and reject growth past limit."""
    chunks: list[bytes] = []
    _read_pinned(path, limit, chunks.append)
    return b"".join(chunks)


def fingerprint_bounded_regular_file(path: Path, limit: int) -> FileFingerprint:
    """Stream the same pinned read into a SHA-256 digest; memory stays O(chunk size)."""
    digest = hashlib.sha256()
    size = _read_pinned(path, limit, digest.update)
    return FileFingerprint(size, digest.hexdigest())
