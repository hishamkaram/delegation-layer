"""Validate owned acceptance paths and finite prompt transport."""

from __future__ import annotations

import os
from scripts.delegate_acceptance.evidence.validation import require
from pathlib import Path


def reject_tmp(path: Path, label: str) -> None:
    resolved = path.resolve(strict=False)
    excluded = [Path(item).resolve(strict=False)
                for item in ("/tmp", "/var/tmp", "/var/folders", "/dev")]
    require(not any(resolved == root or root in resolved.parents for root in excluded),
            f"{label} must be outside provider temporary roots: {resolved}")


def path_is_within(path: Path, parent: Path) -> bool:
    child = path.resolve(strict=False)
    ancestor = parent.resolve(strict=False)
    return child == ancestor or ancestor in child.parents


def ensure_private_directory(path: Path, label: str, create: bool = False) -> Path:
    reject_tmp(path, label)
    if create:
        path.mkdir(mode=0o700, parents=True, exist_ok=False)
    require(path.is_dir() and not path.is_symlink(), f"{label} is not a private directory: {path}")
    os.chmod(path, 0o700)
    require(path.stat().st_mode & 0o077 == 0, f"{label} is group/world accessible: {path}")
    return path.resolve()


def write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with path.open("xb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    os.chmod(path, 0o600)


def no_prompt_argv(argv: list[object], briefs: list[Path], forbidden: tuple[str, ...] = ()) -> None:
    """Prove finite prompts travel through files/stdin, never child argv."""
    strings = [str(value) for value in argv]
    joined = "\x00".join(strings)
    for brief in briefs:
        content = brief.read_bytes().decode("utf-8", errors="replace")
        require(content not in joined, "brief content leaked into child argv")
    for value in forbidden:
        require(value not in strings, "forbidden provider argument leaked into child argv: " + value)
