"""Read exact regular-file bytes from immutable Git objects."""

from __future__ import annotations

import re
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path

from .candidate_build_source import GIT_TIMEOUT_SECONDS
from .candidate_codec import revision
from .commands import command
from .contract import MAX_SOURCE_FILE, VerificationError


STAGE = "observe-candidate-source"
MAX_TREE_LISTING_OUTPUT = 4 * 1024 * 1024


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


def _read(receipt: dict, environment: Mapping[str, str], name: str, arguments: list[str], *,
          repository: Path, max_output: int) -> bytes:
    """Run one recorded read-only Git command whose complete output must fit ``max_output``."""
    return command(receipt, name, ["git", *arguments], dict(environment), capture=True,
                   timeout=GIT_TIMEOUT_SECONDS, cwd=repository, max_output=max_output)


def read_regular_blobs(receipt: dict, environment: Mapping[str, str], *, repository: Path, tree: str,
                       paths: tuple[str, ...]) -> dict[str, bytes]:
    """Read the exact bytes of each named regular file of ``tree``; no checkout fallback.

    ``tree`` is the full ID of a commit or tree object, never a revision
    expression such as ``HEAD`` or a branch name, so no ref can choose the bytes.
    """
    revision(tree, STAGE)
    listing = _read(receipt, environment, "git-registry-entries",
                    ["--literal-pathspecs", "ls-tree", "-z", "--full-tree", tree, "--", *paths],
                    repository=repository, max_output=MAX_TREE_LISTING_OUTPUT)
    entries = tree_entries(listing)
    if not set(entries) <= set(paths):
        raise VerificationError("git_output_invalid", "git-registry-entries")
    blobs = {}
    for path in paths:
        entry = entries.get(path)
        if entry is None:
            raise VerificationError("registry_file_missing", STAGE)
        if not entry.regular_blob:
            raise VerificationError("registry_file_not_regular", STAGE)
        blobs[path] = _read(receipt, environment, "git-registry-blob", ["cat-file", "blob", entry.object_id],
                            repository=repository, max_output=MAX_SOURCE_FILE)
    return blobs
