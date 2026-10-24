"""Canonical source-change facts a reviewer classifies, and their one byte encoding each.

The values here are what the Git source observer read from exact immutable
objects. They hold no scope decision: a reviewer supplies the change scope and
rationale, and ``reviewed_change_scope`` only binds those classifications to
the observed facts. Nothing here proves a review, authorizes paid work, or
decides readiness or publication.

Changed-path inventory encoding: every path that differs between the base tree
and the head tree (additions, deletions and modifications; renames reported as
a deletion plus an addition), as its exact UTF-8 bytes followed by one NUL,
sorted by those bytes, each path once. An empty diff encodes as empty bytes.
The count is the number of paths and the digest is the SHA-256 of exactly
those bytes.

Registry fingerprint encoding: the master registry file and every module
fragment it references at the head tree, sorted by UTF-8 path bytes, each
encoded as ``<path> NUL <sha256 hex of the exact blob bytes> NUL``. The
fingerprint is the SHA-256 of the concatenation.
"""

from __future__ import annotations

from dataclasses import dataclass
import posixpath
import re

from ..evidence.digests import sha
from ..native_evidence.public_contract import PublicProofError, parse_json
from .candidate_attempts import ChangeScope, ReviewedChangeScope
from .candidate_codec import revision, sha256_hex
from .candidate_manifest import SourceIdentity
from .contract import VerificationError


STAGE = "validate-candidate-source"


REGISTRY_PATH = "agents/_data/subsystems.json"


# The registry manifest version whose ``module_files`` this reader understands.
REGISTRY_VERSION = 1


# Bounds the fragment inventory one registry may name.
MAX_REGISTRY_FILES = 1024


def path_bytes(path: str) -> bytes:
    return path.encode("utf-8")


def _canonical_path(value: object) -> bool:
    """A repository-relative POSIX path as Git stores it: no empty, ``.`` or ``..`` component."""
    if not isinstance(value, str) or not value or "\0" in value:
        return False
    try:
        path_bytes(value)
    except UnicodeError:
        return False
    return (not value.startswith("/") and posixpath.normpath(value) == value
            and all(part not in ("", ".", "..") for part in value.split("/")))


def _sorted_unique_paths(paths: tuple[str, ...]) -> bool:
    encoded = [path_bytes(path) for path in paths]
    return encoded == sorted(set(encoded))


@dataclass(frozen=True)
class ChangedPathInventory:
    """The complete base-to-head changed-path inventory; the paths own its encoding."""

    paths: tuple[str, ...]

    def __post_init__(self) -> None:
        if (not isinstance(self.paths, tuple) or not all(_canonical_path(path) for path in self.paths)
                or not _sorted_unique_paths(self.paths)):
            raise VerificationError("source_changed_paths_invalid", STAGE)

    @property
    def encoded(self) -> bytes:
        return b"".join(path_bytes(path) + b"\0" for path in self.paths)

    @property
    def count(self) -> int:
        return len(self.paths)

    @property
    def sha256(self) -> str:
        return sha(self.encoded)


def changed_path_inventory(output: bytes) -> ChangedPathInventory:
    """Decode NUL-terminated Git name-only output into the canonical inventory.

    A name that is not UTF-8 has no source-path view, so it is refused rather
    than replaced. Every name is kept byte for byte: whitespace and line breaks
    are part of the name.
    """
    if output and not output.endswith(b"\0"):
        raise VerificationError("git_output_invalid", STAGE)
    names = output.split(b"\0")[:-1] if output else []
    if len(set(names)) != len(names):
        raise VerificationError("git_output_invalid", STAGE)
    try:
        paths = tuple(name.decode("utf-8") for name in sorted(names))
    except UnicodeError:
        raise VerificationError("source_path_not_utf8", STAGE) from None
    if not all(_canonical_path(path) for path in paths):
        raise VerificationError("git_output_invalid", STAGE)
    return ChangedPathInventory(paths=paths)


def registry_module_files(master: bytes) -> tuple[str, ...]:
    """Read only the registry version and its fragment paths; the Go registry owner validates the rest."""
    try:
        value = parse_json(master)
    except PublicProofError:
        raise VerificationError("registry_master_invalid", STAGE) from None
    if not isinstance(value, dict):
        raise VerificationError("registry_master_invalid", STAGE)
    version = value.get("version")
    if isinstance(version, bool) or not isinstance(version, int) or version != REGISTRY_VERSION:
        raise VerificationError("registry_version_unsupported", STAGE)
    files = value.get("module_files")
    if not isinstance(files, list) or len(files) > MAX_REGISTRY_FILES:
        raise VerificationError("registry_module_files_invalid", STAGE)
    if not all(_canonical_path(path) for path in files):
        raise VerificationError("registry_fragment_path_invalid", STAGE)
    if len(set(files)) != len(files) or REGISTRY_PATH in files:
        raise VerificationError("registry_fragment_duplicate", STAGE)
    return tuple(files)


# Git tree modes of a regular file; a symlink (120000) or gitlink never qualifies.
REGULAR_BLOB_MODES = frozenset({"100644", "100755"})


_TREE_ENTRY_RE = re.compile(rb"([0-7]{6}) (blob|tree|commit) ([0-9a-f]{40,64})\t(.+)", re.DOTALL)


@dataclass(frozen=True)
class TreeEntry:
    mode: str
    kind: str
    object_id: str

    @property
    def regular_blob(self) -> bool:
        return self.kind == "blob" and self.mode in REGULAR_BLOB_MODES


def tree_entries(output: bytes) -> dict[str, TreeEntry]:
    """Decode NUL-terminated ``git ls-tree -z`` output into entries keyed by exact path."""
    if output and not output.endswith(b"\0"):
        raise VerificationError("git_output_invalid", STAGE)
    entries: dict[str, TreeEntry] = {}
    lines = output.split(b"\0")[:-1] if output else []
    for line in lines:
        match = _TREE_ENTRY_RE.fullmatch(line)
        if match is None:
            raise VerificationError("git_output_invalid", STAGE)
        mode, kind, object_id, name = match.groups()
        try:
            path = name.decode("utf-8")
        except UnicodeError:
            raise VerificationError("source_path_not_utf8", STAGE) from None
        if path in entries:
            raise VerificationError("git_output_invalid", STAGE)
        entries[path] = TreeEntry(mode=mode.decode("ascii"), kind=kind.decode("ascii"),
                                  object_id=object_id.decode("ascii"))
    return entries


@dataclass(frozen=True)
class RegistryFile:
    """One registry source file at the head tree, bound by the SHA-256 of its exact blob bytes."""

    path: str
    sha256: str

    def __post_init__(self) -> None:
        if not _canonical_path(self.path):
            raise VerificationError("registry_fragment_path_invalid", STAGE)
        sha256_hex(self.sha256, STAGE)


@dataclass(frozen=True)
class RegistryFingerprint:
    """The master registry and every fragment it references, sorted by path."""

    files: tuple[RegistryFile, ...]

    def __post_init__(self) -> None:
        if (not isinstance(self.files, tuple) or not all(isinstance(item, RegistryFile) for item in self.files)
                or not _sorted_unique_paths(tuple(item.path for item in self.files))
                or REGISTRY_PATH not in {item.path for item in self.files}):
            raise VerificationError("registry_fingerprint_invalid", STAGE)

    @property
    def encoded(self) -> bytes:
        return b"".join(path_bytes(item.path) + b"\0" + item.sha256.encode("ascii") + b"\0"
                        for item in self.files)

    @property
    def sha256(self) -> str:
        return sha(self.encoded)


def registry_fingerprint(blobs: dict[str, bytes]) -> RegistryFingerprint:
    """Fingerprint the exact registry blobs read at one tree, keyed by repository path."""
    files = tuple(RegistryFile(path=path, sha256=sha(blobs[path]))
                  for path in sorted(blobs, key=path_bytes))
    return RegistryFingerprint(files=files)


@dataclass(frozen=True)
class ObservedSourceChange:
    """Facts read from exact Git objects for one manifest source identity.

    ``source`` is the manifest identity the observation matched, not a second
    owner of the head or tree.
    """

    source: SourceIdentity
    base_tree: str
    changed_paths: ChangedPathInventory
    registry: RegistryFingerprint

    def __post_init__(self) -> None:
        revision(self.base_tree, STAGE)
        if (not isinstance(self.source, SourceIdentity)
                or not isinstance(self.changed_paths, ChangedPathInventory)
                or not isinstance(self.registry, RegistryFingerprint)):
            raise VerificationError("source_observation_invalid", STAGE)


def reviewed_change_scope(observed: ObservedSourceChange, *, scope: ChangeScope,
                          rationale: str) -> ReviewedChangeScope:
    """Bind a reviewer's scope classification and rationale to the observed source facts.

    This makes no claim that the classification is correct and grants no
    review authority; the recorded source review owns that.
    """
    if not isinstance(observed, ObservedSourceChange):
        raise VerificationError("source_observation_invalid", STAGE)
    return ReviewedChangeScope(
        scope=scope, base_commit=observed.source.base_commit, base_tree=observed.base_tree,
        changed_path_count=observed.changed_paths.count,
        changed_paths_sha256=observed.changed_paths.sha256,
        subsystem_registry_sha256=observed.registry.sha256, rationale=rationale,
    )
