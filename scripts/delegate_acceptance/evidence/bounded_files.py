"""One identity-checked, bounded regular-file reader for acceptance evidence."""

from enum import Enum
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


def read_bounded_regular_file(path: Path, limit: int) -> bytes:
    """Pin the opened file to its observed identity and reject growth past limit."""
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
            chunks: list[bytes] = []
            remaining = limit
            while remaining:
                chunk = os.read(descriptor, min(1024 * 1024, remaining))
                if not chunk:
                    break
                chunks.append(chunk)
                remaining -= len(chunk)
            if os.read(descriptor, 1):
                raise BoundedReadError(ReadFailure.TOO_LARGE)
            return b"".join(chunks)
        finally:
            os.close(descriptor)
    except BoundedReadError:
        raise
    except (OSError, UnicodeError) as error:
        raise BoundedReadError(ReadFailure.UNREADABLE) from error
