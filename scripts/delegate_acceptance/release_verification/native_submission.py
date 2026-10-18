"""Authenticate a native proof against GitHub facts before admitting its content."""

from __future__ import annotations

from dataclasses import dataclass
import hashlib
import re

from .assets import resolve_release_identity
from .contract import REVISION_RE, TAG_RE, VerificationError
from .github import GitHubClient, object_value, positive_id


@dataclass(frozen=True)
class SubmissionIdentity:
    repository: str
    repository_id: int
    release_revision: str
    harness_revision: str
    sender_id: int
    asset_id: int
    proof_sha256: str
    asset_name: str


def repository_identity(client: GitHubClient, repo: str, harness_revision: str) -> int:
    if (re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo) is None
            or REVISION_RE.fullmatch(harness_revision) is None):
        raise VerificationError("native_repository_identity_invalid", "submission-identity")
    repository = object_value(client.json(f"repos/{repo}"), "repository-identity")
    branch = object_value(client.json(f"repos/{repo}/branches/main"), "protected-main")
    commit = object_value(branch.get("commit"), "protected-main")
    if (repository.get("full_name") != repo or branch.get("protected") is not True
            or commit.get("sha") != harness_revision):
        raise VerificationError("native_harness_not_current_protected_main", "submission-identity")
    return positive_id(repository.get("id"), "repository-identity")


def authenticate_submission(
        client: GitHubClient, repo: str, tag: str, *, asset_id: int,
        proof_sha256: str, harness_revision: str, sender_id: int,
        max_proof_bytes: int) -> tuple[SubmissionIdentity, bytes]:
    if TAG_RE.fullmatch(tag) is None or re.fullmatch(r"[0-9a-f]{64}", proof_sha256) is None:
        raise VerificationError("native_submission_identity_invalid", "submission-identity")
    repository_id = repository_identity(client, repo, harness_revision)
    release = object_value(client.json(f"repos/{repo}/releases/tags/{tag}"), "release-identity")
    release_id = positive_id(release.get("id"), "release-identity")
    if release.get("tag_name") != tag or release.get("prerelease") is not False:
        raise VerificationError("native_release_identity_invalid", "submission-identity")
    asset = _find_release_asset(client, repo, release_id, asset_id)
    uploader = object_value(asset.get("uploader"), "native-uploader")
    uploader_id = positive_id(uploader.get("id"), "native-uploader")
    login = uploader.get("login")
    if (uploader_id != sender_id or not isinstance(login, str)
            or re.fullmatch(r"[A-Za-z0-9-]{1,39}(?:\[bot\])?", login) is None):
        raise VerificationError("native_uploader_sender_mismatch", "submission-authentication")
    permission = object_value(client.json(
        f"repos/{repo}/collaborators/{login}/permission",
    ), "native-uploader-permission")
    permission_user = object_value(permission.get("user"), "native-uploader-permission")
    if (permission.get("permission") not in {"admin", "maintain", "write"}
            or permission_user.get("id") != uploader_id):
        raise VerificationError("native_uploader_not_authorized", "submission-authentication")
    data = client.bytes(f"repos/{repo}/releases/assets/{asset_id}", limit=max_proof_bytes)
    if hashlib.sha256(data).hexdigest() != proof_sha256:
        raise VerificationError("native_proof_digest_mismatch", "submission-bytes")
    release_identity = resolve_release_identity(repo, tag, client.receipt, client.environment)
    identity = SubmissionIdentity(
        repository=repo, repository_id=repository_id,
        release_revision=release_identity["commit_sha"], harness_revision=harness_revision,
        sender_id=sender_id, asset_id=asset_id, proof_sha256=proof_sha256,
        asset_name=str(asset["name"]),
    )
    return identity, data


def _find_release_asset(client: GitHubClient, repo: str,
                        release_id: int, asset_id: int) -> dict[str, object]:
    for page in range(1, 11):
        rows = client.json(f"repos/{repo}/releases/{release_id}/assets?per_page=100&page={page}",
                           limit=1024 * 1024)
        if not isinstance(rows, list):
            raise VerificationError("release_assets_invalid", "submission-identity")
        matches = [row for row in rows if isinstance(row, dict) and row.get("id") == asset_id]
        if len(matches) == 1:
            result = object_value(matches[0], "proof-asset")
            name, size = result.get("name"), result.get("size")
            if (not isinstance(name, str)
                    or re.fullmatch(r"native-proof-(Linux|Darwin)-(amd64|arm64)-[0-9a-f]{32}\.zip", name)
                    is None or type(size) is not int or size <= 0):
                raise VerificationError("native_proof_asset_invalid", "submission-identity")
            return result
        if len(rows) < 100:
            break
    raise VerificationError("native_proof_asset_not_in_release", "submission-identity")


def require_public_repository(client: GitHubClient, repo: str) -> None:
    facts = object_value(client.json(f"repos/{repo}"), "public-release-precheck")
    if facts.get("private") is not False or facts.get("visibility") != "public":
        raise VerificationError("anonymous_installation_requires_public_repository",
                                "public-release-precheck")
