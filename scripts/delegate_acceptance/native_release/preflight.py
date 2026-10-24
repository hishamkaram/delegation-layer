"""Read-only repository, source, and release prerequisites before native work."""

from __future__ import annotations

from collections.abc import Mapping
from pathlib import Path
import sys

from scripts.delegate_acceptance.release_verification.assets import resolve_release_identity
from scripts.delegate_acceptance.release_verification.candidate_build_source import child_environment, git
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from scripts.delegate_acceptance.release_verification.source import observe_harness, source_seeds
from .contract import (
    Invocation, ReleaseError, integer, object_value, release_request, revision_value, text_value,
)


# The source owner's refusals as the native release reports them; any other error keeps its identity.
SOURCE_REFUSALS = {
    "source_fingerprint_invalid": "native_source_fingerprint_invalid",
    "source_path_invalid": "native_source_path_invalid",
    "source_changed_during_observation": "native_source_changed_during_preflight",
}


def require_python() -> None:
    if sys.version_info < (3, 12):
        raise ReleaseError("python_3_12_required")


def _harness(receipt: dict, environment: Mapping[str, str], checkout: Path) -> tuple[str, str, bool]:
    """The native-acceptance harness of ``checkout``: HEAD commit, fingerprint and committed match."""
    try:
        return observe_harness(receipt, environment, checkout=checkout, seeds=source_seeds("native-acceptance"))
    except VerificationError as error:
        code = SOURCE_REFUSALS.get(error.code)
        if code is None:
            raise
        raise ReleaseError(code) from error


def observe_executing_harness(receipt: dict, environment: Mapping[str, str], *,
                              checkout: Path) -> dict[str, str]:
    """The revision and fingerprint of the native harness this checkout executes.

    This is the harness that runs the native cases, never the build harness a
    candidate manifest records. Every fingerprinted file must equal its bytes at
    the checkout's HEAD, so the revision alone names what runs.
    """
    head, source_sha, committed = _harness(receipt, environment, checkout)
    if not committed:
        raise ReleaseError("candidate_native_harness_not_committed")
    return {"harness_revision": head, "harness_sha256": source_sha}


def preflight(invocation: Invocation, client: GitHubClient) -> dict[str, object]:
    require_python()
    request, checkout = release_request(invocation), invocation.checkout
    env = client.environment
    head, source_sha, committed_source = _harness(client.receipt, env, checkout)
    repository = object_value(client.json(f"repos/{request.repo}"))
    branch = object_value(client.json(f"repos/{request.repo}/branches/main"))
    commit = object_value(branch.get("commit"))
    main = revision_value(commit.get("sha"))
    if repository.get("full_name") != request.repo or branch.get("protected") is not True:
        raise ReleaseError("current_protected_main_required")
    release = client.release(request.repo, request.tag)
    if (release.get("tag_name") != request.tag or type(release.get("draft")) is not bool
            or not release["draft"] and not request.no_submit
            or release.get("prerelease") is not False):
        raise ReleaseError("exact_draft_release_required")
    identity = object_value(resolve_release_identity(request.repo, request.tag,
                                                   {"commands": []}, env))
    release_revision = revision_value(identity.get("commit_sha"))
    dirty = bool(git(client.receipt, child_environment(env), "native-release-git-status",
                     ["status", "--porcelain", "--untracked-files=all"], cwd=checkout))
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
    request = release_request(invocation)
    repo, tag = request.repo, request.tag
    branch = object_value(client.json(f"repos/{repo}/branches/main"))
    release = client.release(repo, tag)
    identity = object_value(resolve_release_identity(repo, tag, {"commands": []}, client.environment))
    actor = object_value(client.json("user"))
    if (facts.get("candidate_harness") is not False or branch.get("protected") is not True
            or object_value(branch.get("commit")).get("sha") != facts.get("harness_revision")
            or release.get("id") != facts.get("release_id") or release.get("draft") is not True
            or release.get("tag_name") != tag or release.get("prerelease") is not False
            or identity.get("commit_sha") != facts.get("release_revision")
            or actor.get("id") != facts.get("actor_id") or actor.get("type") != "User"):
        raise ReleaseError("submission_prerequisite_changed")
