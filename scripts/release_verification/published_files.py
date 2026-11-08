"""Bounded file observations for published-consumer verification."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import hashlib
import io
import json
import os
import stat
import tarfile
from scripts.release_verification.contract import BINARIES
from scripts.acceptance.evidence.source import MAX_OUTPUT
from scripts.acceptance.evidence.source import MAX_SOURCE_FILE
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.filesystem import _bounded_children
from scripts.acceptance.evidence.source import _bounded_file_bytes
from scripts.release_verification.published_contract import MAX_PACKAGE
from scripts.release_verification.published_contract import MAX_SKILL_FILES
from scripts.release_verification.published_contract import PACKAGE_SKILL_PREFIX
from scripts.release_verification.published_contract import safe_skill_path


_MAX_PACKAGE_MEMBERS = 2048


_MAX_PACKAGE_EXPANDED = 64 * 1024 * 1024


_MAX_TREE_DEPTH = 8


@dataclass(frozen=True)
class PackageContents:
    manifest: dict
    skill_digests: dict[str, str]


def create_private_file(path: Path) -> None:
    """Create one empty owner-only file; an existing path is never reused."""
    try:
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    except OSError:
        raise VerificationError("published_workspace_invalid", "prepare-workspace") from None
    os.close(descriptor)


def read_package_bytes(tarball: Path) -> bytes:
    return _bounded_file_bytes(tarball, MAX_PACKAGE, "npm_pack_too_large")


def read_package(data: bytes) -> PackageContents:
    """Read package.json and every packaged skill file digest without extracting to disk."""
    manifest: dict | None = None
    digests: dict[str, str] = {}
    expanded = 0
    try:
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as bundle:
            for count, member in enumerate(bundle, 1):
                expanded += max(member.size, 0)
                if (count > _MAX_PACKAGE_MEMBERS or expanded > _MAX_PACKAGE_EXPANDED
                        or member.name.startswith("/") or "\\" in member.name
                        or ".." in member.name.split("/")):
                    raise VerificationError("npm_pack_member_invalid", "npm-pack")
                if member.name == "package/package.json":
                    manifest = _package_manifest(bundle, member)
                elif member.name.startswith(PACKAGE_SKILL_PREFIX) and not member.isdir():
                    relative = safe_skill_path(member.name[len(PACKAGE_SKILL_PREFIX):])
                    digests[relative] = _member_digest(bundle, member)
                    if len(digests) > MAX_SKILL_FILES:
                        raise VerificationError("npm_pack_member_invalid", "npm-pack")
    except (OSError, tarfile.TarError, EOFError):
        raise VerificationError("npm_pack_archive_invalid", "npm-pack") from None
    if manifest is None:
        raise VerificationError("npm_pack_manifest_missing", "npm-pack")
    return PackageContents(manifest, digests)


def _member_digest(bundle: tarfile.TarFile, member: tarfile.TarInfo) -> str:
    source = bundle.extractfile(member) if member.isfile() else None
    if source is None or member.size > MAX_SOURCE_FILE:
        raise VerificationError("npm_pack_member_invalid", "npm-pack")
    with source:
        return hashlib.sha256(source.read(MAX_SOURCE_FILE + 1)).hexdigest()


def _package_manifest(bundle: tarfile.TarFile, member: tarfile.TarInfo) -> dict:
    source = bundle.extractfile(member) if member.isfile() else None
    if source is None or member.size > MAX_OUTPUT:
        raise VerificationError("npm_pack_member_invalid", "npm-pack")
    with source:
        try:
            value = json.loads(source.read(MAX_OUTPUT + 1))
        except (UnicodeError, json.JSONDecodeError, RecursionError):
            raise VerificationError("npm_pack_manifest_invalid", "npm-pack") from None
    if not isinstance(value, dict):
        raise VerificationError("npm_pack_manifest_invalid", "npm-pack")
    return value


def installed_skill_digests(root: Path) -> dict[str, str]:
    """Digest every regular file under an installed skill directory; links fail."""
    digests: dict[str, str] = {}
    pending = [(root, "", 0)]
    while pending:
        directory, prefix, depth = pending.pop()
        if depth > _MAX_TREE_DEPTH:
            raise VerificationError("installed_skill_invalid", "verify-skill")
        for child in _bounded_children(directory):
            relative = f"{prefix}{child.name}"
            try:
                mode = child.lstat().st_mode
            except OSError:
                raise VerificationError("installed_skill_invalid", "verify-skill") from None
            if stat.S_ISDIR(mode):
                pending.append((child, relative + "/", depth + 1))
            elif stat.S_ISREG(mode):
                digests[safe_skill_path(relative)] = hashlib.sha256(
                    _bounded_file_bytes(child, MAX_SOURCE_FILE, "installed_skill_invalid")).hexdigest()
            else:
                raise VerificationError("installed_skill_invalid", "verify-skill")
            if len(digests) > MAX_SKILL_FILES:
                raise VerificationError("installed_skill_invalid", "verify-skill")
    return digests


def require_exact_binaries(directory: Path) -> None:
    """The installer must publish exactly the four release binaries and nothing else."""
    if directory.is_symlink() or not directory.is_dir():
        raise VerificationError("installed_binaries_invalid", "inspect-binaries")
    if {child.name for child in _bounded_children(directory)} != set(BINARIES):
        raise VerificationError("installed_binaries_unexpected_entries", "inspect-binaries")
