"""Probe the existing generation lock without provisioning or replacing it."""

import errno
import fcntl
import os
from pathlib import Path
import stat

from scripts.delegate_acceptance.private_cli.contention_policy import require_lease_mode


def probe_generation_lease(path: Path, mode: str) -> dict[str, object]:
    descriptor = os.open(path, os.O_RDWR | os.O_NONBLOCK | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        opened = os.fstat(descriptor)
        named = path.lstat()
        identity = (opened.st_dev, opened.st_ino)
        if not stat.S_ISREG(opened.st_mode) or opened.st_uid != os.getuid() or identity != (named.st_dev, named.st_ino):
            raise RuntimeError("modern contention bootstrap lock identity is unsafe")
        acquired = []
        for operation in (fcntl.LOCK_SH, fcntl.LOCK_EX):
            try:
                fcntl.flock(descriptor, operation | fcntl.LOCK_NB)
            except BlockingIOError as error:
                if error.errno not in (errno.EACCES, errno.EAGAIN):
                    raise
                acquired.append(False)
            else:
                acquired.append(True)
                fcntl.flock(descriptor, fcntl.LOCK_UN)
        after = path.lstat()
        if identity != (after.st_dev, after.st_ino):
            raise RuntimeError("modern contention bootstrap lock was replaced during probing")
        require_lease_mode(mode, *acquired)
        return {
            "path": str(path),
            "device": opened.st_dev,
            "inode": opened.st_ino,
            "file_mode": oct(stat.S_IMODE(opened.st_mode)),
            "owner_mode": mode,
            "probe_shared_acquired": acquired[0],
            "probe_exclusive_acquired": acquired[1],
        }
    finally:
        os.close(descriptor)
