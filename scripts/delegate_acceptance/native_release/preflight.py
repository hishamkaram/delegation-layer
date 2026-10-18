"""Read-only repository, source, and release prerequisites before native work."""

from __future__ import annotations

import hashlib
from pathlib import Path
import sys

from scripts.delegate_acceptance.evidence.bounded_files import read_bounded_regular_file
from scripts.delegate_acceptance.release_verification.assets import resolve_release_identity
from scripts.delegate_acceptance.release_verification.commands import command
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from scripts.delegate_acceptance.release_verification.source import source_fingerprint
from .contract import Invocation, ReleaseError, digest_value, integer, object_value, revision_value, text_value


def require_python() -> None:
    if sys.version_info < (3, 12):
        raise ReleaseError("python_3_12_required")


def git_bytes(checkout: Path, env: dict[str, str], argv: list[str]) -> bytes:
    return command({"commands": []}, "native-release-git", ["git", *argv], env,
                   cwd=checkout, capture=True, timeout=30)


def _source(checkout: Path, env: dict[str, str], revision: str) -> tuple[str, bool]:
    fingerprint = object_value(source_fingerprint("native-acceptance"))
    files = fingerprint.get("files")
    if not isinstance(files, list) or not files:
        raise ReleaseError("native_source_fingerprint_invalid")
    names = [text_value(object_value(row).get("path")) for row in files]
    tracked = set(git_bytes(checkout, env, ["ls-tree", "-r", "--name-only", revision, "--", *names])
                  .decode("utf-8").splitlines())
    matches = True
    for row in files:
        item = object_value(row)
        path = item.get("path")
        if not isinstance(path, str) or path.startswith("/") or ".." in Path(path).parts:
            raise ReleaseError("native_source_path_invalid")
        data = read_bounded_regular_file(checkout / path, 8 << 20)
        if hashlib.sha256(data).hexdigest() != digest_value(item.get("sha256")):
            raise ReleaseError("native_source_changed_during_preflight")
        if path not in tracked:
            matches = False
        else:
            committed = git_bytes(checkout, env, ["show", f"{revision}:{path}"])
            matches = matches and hashlib.sha256(committed).hexdigest() == item["sha256"]
    return digest_value(fingerprint.get("sha256")), matches


def preflight(invocation: Invocation, client: GitHubClient) -> dict[str, object]:
    require_python()
    request, checkout = invocation.request, invocation.checkout
    env = client.environment
    head = revision_value(git_bytes(checkout, env, ["rev-parse", "HEAD"]).decode("ascii").strip())
    repository = object_value(client.json(f"repos/{request.repo}"))
    branch = object_value(client.json(f"repos/{request.repo}/branches/main"))
    commit = object_value(branch.get("commit"))
    main = revision_value(commit.get("sha"))
    if repository.get("full_name") != request.repo or branch.get("protected") is not True:
        raise ReleaseError("current_protected_main_required")
    release = object_value(client.json(f"repos/{request.repo}/releases/tags/{request.tag}"))
    if (release.get("tag_name") != request.tag or type(release.get("draft")) is not bool
            or not release["draft"] and not request.no_submit
            or release.get("prerelease") is not False):
        raise ReleaseError("exact_draft_release_required")
    identity = object_value(resolve_release_identity(request.repo, request.tag,
                                                   {"commands": []}, env))
    release_revision = revision_value(identity.get("commit_sha"))
    source_sha, committed_source = _source(checkout, env, head)
    dirty = bool(git_bytes(checkout, env, ["status", "--porcelain", "--untracked-files=all"]))
    candidate = head != main or not committed_source or dirty
    if candidate and not request.no_submit:
        raise ReleaseError("submission_requires_clean_current_main_harness")
    actor = object_value(client.json("user"))
    actor_id = integer(actor.get("id"), minimum=1)
    actor_login = text_value(actor.get("login"), r"[A-Za-z0-9-]{1,39}")
    if not request.no_submit:
        permissions = object_value(repository.get("permissions"))
        if not any(permissions.get(name) is True for name in ("push", "maintain", "admin")):
            raise ReleaseError("submission_write_permission_required")
        if actor.get("type") != "User":
            raise ReleaseError("submission_requires_maintainer_user_identity")
    return {
        "repository_id": integer(repository.get("id"), minimum=1),
        "release_id": integer(release.get("id"), minimum=1),
        "release_revision": release_revision, "harness_revision": head,
        "protected_main_revision": main, "source_harness_sha256": source_sha,
        "candidate_harness": candidate, "actor_id": actor_id, "actor_login": actor_login,
    }


def recheck_submission(invocation: Invocation, client: GitHubClient,
                       facts: dict[str, object]) -> None:
    repo, tag = invocation.request.repo, invocation.request.tag
    branch = object_value(client.json(f"repos/{repo}/branches/main"))
    release = object_value(client.json(f"repos/{repo}/releases/tags/{tag}"))
    identity = object_value(resolve_release_identity(repo, tag, {"commands": []}, client.environment))
    actor = object_value(client.json("user"))
    if (facts.get("candidate_harness") is not False or branch.get("protected") is not True
            or object_value(branch.get("commit")).get("sha") != facts.get("harness_revision")
            or release.get("id") != facts.get("release_id") or release.get("draft") is not True
            or release.get("tag_name") != tag or release.get("prerelease") is not False
            or identity.get("commit_sha") != facts.get("release_revision")
            or actor.get("id") != facts.get("actor_id") or actor.get("type") != "User"):
        raise ReleaseError("submission_prerequisite_changed")
