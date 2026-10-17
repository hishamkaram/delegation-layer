"""Read-only snapshots of acceptance workspaces and task evidence."""

from __future__ import annotations

import hashlib
from pathlib import Path
import stat

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.tasks.publication import snapshot


def task_digest_snapshot(directory: Path) -> dict[str, dict[str, object]]:
    return snapshot(directory, ignored_prefixes=(".ack",))


def has_git_ancestor(directory: Path) -> bool:
    resolved = directory.resolve()
    for parent in (resolved, *resolved.parents):
        try:
            (parent / ".git").lstat()
        except FileNotFoundError:
            continue
        return True
    return False


def workspace_snapshot(directory: Path) -> dict[str, dict[str, object]]:
    """Record every workspace entry without opening special files."""
    directory = Path(directory)
    require(directory.is_dir() and not directory.is_symlink(),
            "workspace directory is absent: " + str(directory))
    result: dict[str, dict[str, object]] = {}
    for path in sorted(directory.rglob("*")):
        metadata = path.lstat()
        if stat.S_ISLNK(metadata.st_mode):
            raise AcceptanceFailure("workspace contains symlink: " + str(path))
        entry: dict[str, object] = {
            "type": stat.S_IFMT(metadata.st_mode),
            "mode": stat.S_IMODE(metadata.st_mode),
        }
        if stat.S_ISREG(metadata.st_mode):
            data = path.read_bytes()
            entry.update(bytes=len(data), sha256=hashlib.sha256(data).hexdigest())
        elif stat.S_ISCHR(metadata.st_mode) or stat.S_ISBLK(metadata.st_mode):
            entry["device"] = metadata.st_rdev
        result[str(path.relative_to(directory))] = entry
    return result


