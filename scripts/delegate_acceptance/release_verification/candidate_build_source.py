"""Frozen-source preflight and the private clone one candidate build runs in.

The caller's repository is only read. The build runs in an independent clone
that holds no tags but the one intended local build tag, so GoReleaser's exact
tag lookup succeeds without touching any tag, ref or object of the caller's
repository. The clone's origin names the public repository URL and is never
fetched from or pushed to.
"""

from __future__ import annotations

from collections.abc import Mapping
import os
from pathlib import Path

from .candidate_build_plan import STAGE, BuildRequest
from .candidate_codec import revision
from .commands import command
from .contract import VerificationError


# The only caller variables a build child inherits. Tokens are never among them.
INHERITED_VARIABLES = ("PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "TZ",
                       "GOROOT", "GOPATH", "GOMODCACHE", "GOCACHE")


GIT_TIMEOUT_SECONDS = 300


def child_environment(parent: Mapping[str, str]) -> dict[str, str]:
    """Build the credential-free child environment; Go VCS stamping stays enabled.

    Git ignores ``refs/replace`` so every object ID names its own content.
    """
    environment = {key: parent[key] for key in INHERITED_VARIABLES if key in parent}
    if not environment.get("PATH"):
        raise VerificationError("build_path_unavailable", STAGE)
    environment.update({
        "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_NO_REPLACE_OBJECTS": "1",
        "GIT_TERMINAL_PROMPT": "0", "GOTOOLCHAIN": "local",
    })
    return environment


def git(receipt: dict, environment: Mapping[str, str], name: str, arguments: list[str], *,
        cwd: Path, capture: bool = True) -> bytes:
    return command(receipt, name, ["git", *arguments], dict(environment), capture=capture,
                   timeout=GIT_TIMEOUT_SECONDS, cwd=cwd)


def _object_name(output: bytes, name: str) -> str:
    try:
        return revision(output.decode("ascii").strip(), name)
    except UnicodeError:
        raise VerificationError("git_output_invalid", name) from None


def head_tree(receipt: dict, environment: Mapping[str, str], *, repository: Path) -> tuple[str, str]:
    """Return (HEAD commit, HEAD tree) of one repository."""
    head = _object_name(git(receipt, environment, "git-head", ["rev-parse", "HEAD"], cwd=repository),
                        "git-head")
    tree = _object_name(
        git(receipt, environment, "git-tree", ["rev-parse", "HEAD^{tree}"], cwd=repository),
        "git-tree")
    return head, tree


def require_clean(receipt: dict, environment: Mapping[str, str], *, repository: Path,
                  code: str) -> None:
    status = git(receipt, environment, "git-status",
                 ["status", "--porcelain=v1", "--untracked-files=all"], cwd=repository)
    if status:
        raise VerificationError(code, STAGE)


def preflight_source(receipt: dict, environment: Mapping[str, str], request: BuildRequest, *,
                     source_root: Path) -> str:
    """Prove the caller's checkout is the clean requested commit; return its tree."""
    toplevel = git(receipt, environment, "git-toplevel", ["rev-parse", "--show-toplevel"],
                   cwd=source_root).decode("utf-8", errors="replace").strip()
    if Path(toplevel).resolve() != source_root.resolve():
        raise VerificationError("source_root_not_repository_root", STAGE)
    head, tree = head_tree(receipt, environment, repository=source_root)
    if head != request.commit:
        raise VerificationError("source_head_mismatch", STAGE)
    require_clean(receipt, environment, repository=source_root, code="source_not_clean")
    git(receipt, environment, "git-base-commit", ["cat-file", "-e", request.base_commit + "^{commit}"],
        cwd=source_root, capture=False)
    git(receipt, environment, "git-base-ancestor",
        ["merge-base", "--is-ancestor", request.base_commit, request.commit],
        cwd=source_root, capture=False)
    return tree


def clone_frozen(receipt: dict, environment: Mapping[str, str], request: BuildRequest, *,
                 source_root: Path, clone: Path, tree: str) -> None:
    """Clone into the owned empty directory, check out the commit, and tag it locally."""
    git(receipt, environment, "git-clone",
        ["clone", "--quiet", "--no-local", "--no-checkout", "--no-tags", "--", str(source_root),
         str(clone)], cwd=clone.parent, capture=False)
    git(receipt, environment, "git-checkout", ["checkout", "--quiet", "--detach", request.commit],
        cwd=clone, capture=False)
    head, clone_tree = head_tree(receipt, environment, repository=clone)
    if head != request.commit or clone_tree != tree:
        raise VerificationError("clone_identity_mismatch", STAGE)
    require_clean(receipt, environment, repository=clone, code="clone_not_clean")
    git(receipt, environment, "git-origin",
        ["remote", "set-url", "origin", f"https://github.com/{request.repository}.git"],
        cwd=clone, capture=False)
    git(receipt, environment, "git-tag", ["tag", request.tag, request.commit], cwd=clone,
        capture=False)
