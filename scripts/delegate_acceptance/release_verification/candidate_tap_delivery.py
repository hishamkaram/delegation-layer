"""Homebrew tap delivery of one promoted candidate's exact formula bytes.

The manifest owns the formula's size and digest; GitHub owns the tap branch and
its formula file; ``decide_tap`` owns act or skip and the compare-and-swap base.
The caller owns publication authority, delivery order, the trusted tap
credentials and the request-body directory lifetime; the GitHub release and npm
edges own their latest-version guards.
This module performs only the tap I/O the decision calls for and stores nothing.

Only a successful, complete tree listing of the branch without the exact
formula path means absence; every failed, truncated or malformed read stops. One
PUT carries the exact payload, with the observed blob as its precondition when
the file exists. A failed, unknown or malformed acknowledgement stops without a
retry or a later read; a known acknowledgement is confirmed by reading the
branch's formula blob back.
"""

from __future__ import annotations

import base64
import re
from pathlib import Path
import stat

from ..evidence.bounded_files import BoundedReadError, read_bounded_regular_file
from ..evidence.digests import sha
from .candidate_artifact import CandidateArtifact
from .candidate_manifest import REPOSITORY_RE, ArtifactEntry, ArtifactKind, entry_limit
from .candidate_publication_contract import Absent, Delivery, TapFormula, decide_tap
from .contract import VerificationError
from .filesystem import bounded_fingerprint
from .github import GitHubClient, object_value
from .published_contract import git_blob_sha


STAGE = "deliver-candidate-tap"


TAP_REPOSITORY = "hishamkaram/homebrew-tap"


TAP_BRANCH = "main"


FORMULA_PATH = "Formula/delegation-layer.rb"


# A plain branch name: no ref namespace, path, revision or query syntax.
_BRANCH_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$")


_GIT_OBJECT_RE = re.compile(r"^[0-9a-f]{40}$")


# A complete recursive listing of the small tap repository; a larger reply refuses.
_TREE_LISTING_LIMIT = 4 << 20


_REGULAR_FILE_MODE = "100644"


def deliver_tap(artifact: CandidateArtifact, *, client: GitHubClient, work_directory: Path,
                tap_repository: str = TAP_REPOSITORY, branch: str = TAP_BRANCH) -> str:
    """Make the branch's formula the exact candidate formula; return its confirmed Git blob SHA."""
    entry = _formula_entry(artifact)
    if (not isinstance(tap_repository, str) or REPOSITORY_RE.fullmatch(tap_repository) is None
            or not isinstance(branch, str) or _BRANCH_RE.fullmatch(branch) is None or ".." in branch
            or branch.endswith(".lock")):
        raise VerificationError("candidate_tap_target_invalid", STAGE)
    _require_work_directory(work_directory, artifact.payload)
    formula = _formula_bytes(artifact.payload / entry.path, entry)
    observed = _observe(client, tap_repository, branch)
    plan = decide_tap(manifest=artifact.manifest, formula=formula, observed=observed)
    expected = git_blob_sha(formula)
    if plan.action is Delivery.SKIP:
        return expected
    # The bytes sent are re-read and re-checked against the manifest immediately before the PUT.
    payload = _formula_bytes(artifact.payload / entry.path, entry)
    body: dict[str, object] = {
        "message": f"delegation-layer {artifact.manifest.version}",
        "content": base64.b64encode(payload).decode("ascii"), "branch": branch,
    }
    if plan.base_blob_sha is not None:
        body["sha"] = plan.base_blob_sha
    reply = client.json_body(f"repos/{tap_repository}/contents/{FORMULA_PATH}", body, method="PUT",
                             body_path=work_directory / "tap-formula.json")
    _require_acknowledged(reply, expected)
    confirmed = _observe(client, tap_repository, branch)
    if (not isinstance(confirmed, TapFormula) or confirmed.blob_sha != expected
            or confirmed.content != payload):
        raise VerificationError("candidate_tap_readback_mismatch", STAGE)
    return expected


def _formula_entry(artifact: CandidateArtifact) -> ArtifactEntry:
    if not isinstance(artifact, CandidateArtifact):
        raise VerificationError("candidate_tap_artifact_invalid", STAGE)
    (entry,) = (item for item in artifact.manifest.artifacts if item.kind is ArtifactKind.HOMEBREW_FORMULA)
    return entry


def _require_work_directory(path: Path, payload: Path) -> None:
    """An absolute, canonical request-body directory outside the payload."""
    try:
        canonical = isinstance(path, Path) and path.is_absolute() and path.resolve(strict=True) == path
        directory = canonical and stat.S_ISDIR(path.lstat().st_mode)
    except OSError:
        raise VerificationError("candidate_tap_path_invalid", STAGE) from None
    if not directory or path.is_relative_to(payload) or payload.is_relative_to(path):
        raise VerificationError("candidate_tap_path_invalid", STAGE)


def _formula_bytes(path: Path, entry: ArtifactEntry) -> bytes:
    """The payload formula, only while it still holds exactly the manifest's bytes.

    The fingerprint owner refuses a linked path; the bytes returned are the ones hashed here.
    """
    bounded_fingerprint(path, entry.size, "candidate_tap_formula_mismatch")
    try:
        data = read_bounded_regular_file(path, entry.size)
    except BoundedReadError:
        raise VerificationError("candidate_tap_formula_mismatch", STAGE) from None
    if len(data) != entry.size or sha(data) != entry.sha256:
        raise VerificationError("candidate_tap_formula_mismatch", STAGE)
    return data


def _observe(client: GitHubClient, repository: str, branch: str) -> TapFormula | Absent:
    """The branch's formula file, or ABSENT only from a complete listing without its path."""
    listing = object_value(client.json(f"repos/{repository}/git/trees/{branch}?recursive=1",
                                       limit=_TREE_LISTING_LIMIT), STAGE)
    if listing.get("truncated") is not False:
        raise VerificationError("candidate_tap_tree_truncated", STAGE)
    tree = listing.get("tree")
    if not isinstance(tree, list):
        raise VerificationError("candidate_tap_tree_invalid", STAGE)
    paths: set[str] = set()
    selected: dict[str, object] | None = None
    for item in tree:
        record = object_value(item, STAGE)
        path = record.get("path")
        if not isinstance(path, str) or path in paths:
            raise VerificationError("candidate_tap_tree_invalid", STAGE)
        paths.add(path)
        if path == FORMULA_PATH:
            selected = record
    if selected is None:
        return Absent.ABSENT
    blob_sha = selected.get("sha")
    if (selected.get("mode") != _REGULAR_FILE_MODE or selected.get("type") != "blob"
            or not isinstance(blob_sha, str) or _GIT_OBJECT_RE.fullmatch(blob_sha) is None):
        raise VerificationError("candidate_tap_formula_not_regular", STAGE)
    content = client.raw_blob(repository, blob_sha, limit=entry_limit(ArtifactKind.HOMEBREW_FORMULA))
    return TapFormula(content=content, blob_sha=blob_sha)


def _require_acknowledged(reply: object, expected_blob: str) -> None:
    """Accept only the exact file content and a well-formed commit identity."""
    content = reply.get("content") if isinstance(reply, dict) else None
    commit = reply.get("commit") if isinstance(reply, dict) else None
    commit_sha = commit.get("sha") if isinstance(commit, dict) else None
    if (not isinstance(content, dict) or content.get("path") != FORMULA_PATH or content.get("type") != "file"
            or content.get("sha") != expected_blob or not isinstance(commit_sha, str)
            or _GIT_OBJECT_RE.fullmatch(commit_sha) is None):
        raise VerificationError("candidate_tap_ack_invalid", STAGE)
