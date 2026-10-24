"""Read one candidate's source-change facts from exact immutable Git objects.

Every read names a commit, tree or blob by its object ID through Git plumbing,
so the result never depends on the caller's checkout, index or ``HEAD``, and
no command writes any of them. The caller passes the credential-free
``candidate_build_source.child_environment`` so user and system Git
configuration stay out of the reads. The observation makes no scope decision
and grants no review, launch, readiness or publication authority.
"""

from __future__ import annotations

from collections.abc import Mapping
from pathlib import Path

from .candidate_build_source import GIT_TIMEOUT_SECONDS, git
from .candidate_codec import revision
from .candidate_manifest import SourceIdentity
from .candidate_source_inventory import (
    REGISTRY_PATH, ObservedSourceChange, RegistryFingerprint, changed_path_inventory,
    registry_fingerprint, registry_module_files, tree_entries,
)
from .commands import command
from .contract import MAX_SOURCE_FILE, VerificationError


STAGE = "observe-candidate-source"


# Generous finite bounds for complete listings; overflow refuses, never truncates.
MAX_CHANGED_PATHS_OUTPUT = 32 * 1024 * 1024


MAX_TREE_LISTING_OUTPUT = 4 * 1024 * 1024


def _read(receipt: dict, environment: Mapping[str, str], name: str, arguments: list[str], *,
          repository: Path, max_output: int) -> bytes:
    """Run one recorded read-only Git command whose complete output must fit ``max_output``."""
    return command(receipt, name, ["git", *arguments], dict(environment), capture=True,
                   timeout=GIT_TIMEOUT_SECONDS, cwd=repository, max_output=max_output)


def _peel(receipt: dict, environment: Mapping[str, str], name: str, object_id: str, kind: str, *,
          repository: Path) -> str:
    """Return the ID ``object_id`` peels to; a missing object fails the command."""
    output = git(receipt, environment, name, ["rev-parse", "--verify", f"{object_id}^{{{kind}}}"],
                 cwd=repository)
    try:
        return revision(output.decode("ascii").strip(), STAGE)
    except UnicodeError:
        raise VerificationError("git_output_invalid", name) from None


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


def _registry(receipt: dict, environment: Mapping[str, str], *, repository: Path,
              tree: str) -> RegistryFingerprint:
    master = read_regular_blobs(receipt, environment, repository=repository, tree=tree,
                                paths=(REGISTRY_PATH,))[REGISTRY_PATH]
    fragments = registry_module_files(master)
    blobs = {REGISTRY_PATH: master}
    if fragments:
        blobs.update(read_regular_blobs(receipt, environment, repository=repository, tree=tree, paths=fragments))
    return registry_fingerprint(blobs)


def observe_source_change(receipt: dict, environment: Mapping[str, str], *, repository: Path,
                          source: SourceIdentity) -> ObservedSourceChange:
    """Observe the base-to-head change of one manifest source identity in ``repository``.

    The head commit must have exactly the manifest tree and descend from the
    manifest base commit. Any mismatch, failed command or exceeded bound
    refuses before a caller can write a review or start provider work.
    """
    if not isinstance(source, SourceIdentity):
        raise VerificationError("source_observation_invalid", STAGE)
    if _peel(receipt, environment, "git-source-commit", source.commit, "commit",
             repository=repository) != source.commit:
        raise VerificationError("source_commit_mismatch", STAGE)
    if _peel(receipt, environment, "git-source-base", source.base_commit, "commit",
             repository=repository) != source.base_commit:
        raise VerificationError("source_base_mismatch", STAGE)
    if _peel(receipt, environment, "git-source-tree", source.commit, "tree",
             repository=repository) != source.tree:
        raise VerificationError("source_tree_mismatch", STAGE)
    base_tree = _peel(receipt, environment, "git-source-base-tree", source.base_commit, "tree",
                      repository=repository)
    git(receipt, environment, "git-source-base-ancestor",
        ["merge-base", "--is-ancestor", source.base_commit, source.commit], cwd=repository, capture=False)
    changed = _read(receipt, environment, "git-source-changed-paths",
                    ["diff-tree", "-r", "-z", "--name-only", "--no-renames", "--no-relative", "--no-ext-diff",
                     "--no-textconv", base_tree, source.tree],
                    repository=repository, max_output=MAX_CHANGED_PATHS_OUTPUT)
    return ObservedSourceChange(
        source=source, base_tree=base_tree, changed_paths=changed_path_inventory(changed),
        registry=_registry(receipt, environment, repository=repository, tree=source.tree),
    )
