"""GitHub release delivery of one promoted candidate's exact payload bytes.

The manifest and locator own the immutable payload inventory. The caller owns
the exclusive payload and request-body directories, publication authorization
and the overall delivery order. ``decide_release`` owns act, skip or refuse;
GitHub owns the release, its assets and the tag. This module performs only the
GitHub I/O those decisions call for: a typed promotion or ``PreparedRelease``
is never authorization, and nothing here is stored.

Only the exact release not-found answer, and a successful tag listing without
the exact ref, mean absence; every other failed or malformed read stops. Once
the draft's ID is known, from its creation acknowledgement or a preparation,
the release is read by that ID: a complete listing may not show it yet, but a
listing naming another release with the tag still stops. A mutation whose
command fails or whose acknowledgement is not the exact expected reply stops
the invocation: nothing is retried and no success is inferred from a later
read. A later explicit invocation observes again from the start.

The release text is presentation, never authorization. Only when it creates an
absent draft does this edge ask GitHub to generate human release notes: a
read-only request that saves nothing, sent after every guard and before any
write, for the exact merged commit since the latest release that guard just
checked. The draft, titled with its tag, holds those notes and then the exact
``verification_block``. An existing draft or release keeps its text: it is
never regenerated or rewritten, and ``decide_release`` refuses any body that
``release_body_matches`` does not accept. Publication must acknowledge the
body just read and accepted.

This edge owns GitHub's latest-release observation; ``require_latest_delivery``
owns the version order. Right after each ``decide_release`` before the first
write, and again before the tag and the publication, it reads the complete
release listing. Only when that successful listing holds no published,
non-prerelease release is there no latest; otherwise only GitHub's own
``releases/latest`` answer names it, never the listing order or the highest
version. A draft target must then be strictly newer, and a complete published
target must already be latest. Each read is a point-in-time observation, not
an atomic guard against a concurrent publication.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
import os
from pathlib import Path
import stat

from .assets import resolve_release_identity
from .candidate_artifact import CandidateArtifact
from .candidate_manifest import MANIFEST_PATH, PAYLOAD_MEMBER_COUNT, encode_manifest
from .candidate_publication_contract import (
    Absent, ReleaseSubject, ObservedRelease, ReleaseAsset, decide_release, release_text,
    require_latest_delivery,
)
from .contract import HEX_SHA256, VerificationError
from .filesystem import bounded_fingerprint
from .github import (
    MAX_RELEASE_PAGE_BYTES, GitHubClient, iter_release_assets, iter_releases, object_value, positive_id,
)
from .published_ports import ConsumerPorts


STAGE = "deliver-candidate-github"


# One upload streams at most one archive of the manifest's largest accepted size.
UPLOAD_TIMEOUT = 600


# Tags sharing the candidate tag as a prefix; a longer listing is refused, never truncated.
MAX_TAG_REFS = 100


_TAG_REFS_LIMIT = 1 << 20


_DIGEST_PREFIX = "sha256:"


@dataclass(frozen=True)
class PreparedRelease:
    """The release ID and complete observation that one preparation or publication returned."""

    release_id: int
    observed: ObservedRelease

    def __post_init__(self) -> None:
        if (type(self.release_id) is not int or self.release_id <= 0
                or not isinstance(self.observed, ObservedRelease)):
            raise VerificationError("candidate_release_prepared_invalid", STAGE)


@dataclass(frozen=True)
class _Target:
    """Where and what one promotion delivers; ``files`` maps each payload name to (size, sha256)."""

    repo: str
    version: str
    tag: str
    merged_commit: str
    files: Mapping[str, tuple[int, str]]


@dataclass(frozen=True)
class _Remote:
    """One complete read: the release (or its absence) and the tag's commit (or its absence)."""

    release_id: int | None
    release: ObservedRelease | Absent
    tag_commit: str | Absent


def prepare_release_assets(promotion: ReleaseSubject, artifact: CandidateArtifact, *,
                           client: GitHubClient, ports: ConsumerPorts, work_directory: Path,
                           resume_published: bool = False) -> PreparedRelease:
    """Create the draft when needed and upload only missing payload files; never tag or publish."""
    target = _target(promotion)
    _require_artifact(promotion, artifact, target, work_directory)
    remote = _observe(client, target)
    plan = decide_release(promotion=promotion, observed=remote.release, resume_published=resume_published)
    latest = _require_latest(client, target, published=plan.complete)
    if plan.complete:
        return _prepared(remote)
    if plan.create_draft:
        body = release_text(_release_notes(client, target, latest, work_directory), promotion)
        created = _create_draft(client, target, body, work_directory)
        remote = _observe_known(client, target, created, conflict="candidate_release_draft_unconfirmed")
        plan = decide_release(promotion=promotion, observed=remote.release,
                              resume_published=resume_published)
    known = _prepared(remote)
    for name in plan.upload:
        _require_payload_file(artifact.payload, name, target.files[name])
        ports.run(client.receipt, "candidate-release-upload",
                  ["gh", "release", "upload", target.tag, name, "--repo", target.repo],
                  client.environment, timeout=UPLOAD_TIMEOUT, cwd=artifact.payload)
    final = _observe_known(client, target, known.release_id, conflict="candidate_release_upload_unconfirmed")
    plan = decide_release(promotion=promotion, observed=final.release, resume_published=resume_published)
    if plan.create_draft or plan.upload:
        raise VerificationError("candidate_release_upload_unconfirmed", STAGE)
    return _prepared(final)


def publish_prepared_release(promotion: ReleaseSubject, prepared: PreparedRelease, *,
                             client: GitHubClient, work_directory: Path,
                             resume_published: bool = False) -> PreparedRelease:
    """Create the absent tag at the merged commit, then publish the same complete draft once."""
    target = _target(promotion)
    if not isinstance(prepared, PreparedRelease):
        raise VerificationError("candidate_release_prepared_invalid", STAGE)
    _canonical_directory(work_directory)
    remote = _observe_known(client, target, prepared.release_id, conflict="candidate_release_not_prepared")
    plan = decide_release(promotion=promotion, observed=remote.release, resume_published=resume_published)
    # A fresh read right before the tag and publication: a release published meanwhile is never overtaken.
    _require_latest(client, target, published=plan.complete)
    if plan.complete:
        return _prepared(remote)
    if plan.create_draft or plan.upload:
        raise VerificationError("candidate_release_not_prepared", STAGE)
    if remote.tag_commit is Absent.ABSENT:
        _create_tag(client, target, work_directory)
    _acknowledged(client.json_body(
        f"repos/{target.repo}/releases/{prepared.release_id}", {"draft": False, "make_latest": "true"},
        method="PATCH", body_path=work_directory / "release-publish.json", limit=MAX_RELEASE_PAGE_BYTES),
        {"id": prepared.release_id, "tag_name": target.tag, "draft": False, "prerelease": False,
         "body": _prepared(remote).observed.body})
    final = _observe_known(client, target, prepared.release_id,
                           conflict="candidate_release_publish_unconfirmed")
    plan = decide_release(promotion=promotion, observed=final.release, resume_published=resume_published)
    if not plan.complete:
        raise VerificationError("candidate_release_publish_unconfirmed", STAGE)
    return _prepared(final)


def _target(promotion: ReleaseSubject) -> _Target:
    if not isinstance(promotion, ReleaseSubject):
        raise VerificationError("candidate_delivery_promotion_invalid", STAGE)
    files = {entry.path: (entry.size, entry.sha256) for entry in promotion.manifest.artifacts}
    files[MANIFEST_PATH] = (len(encode_manifest(promotion.manifest)), promotion.locator.manifest_sha256)
    return _Target(repo=promotion.locator.repository, version=promotion.manifest.version,
                   tag=promotion.manifest.build_tag, merged_commit=promotion.manifest.source.commit, files=files)


def _canonical_directory(path: Path) -> None:
    """An absolute directory path with no symlink or relative component anywhere."""
    try:
        canonical = isinstance(path, Path) and path.is_absolute() and path.resolve(strict=True) == path
        directory = canonical and stat.S_ISDIR(path.lstat().st_mode)
    except OSError:
        raise VerificationError("candidate_delivery_path_invalid", STAGE) from None
    if not directory:
        raise VerificationError("candidate_delivery_path_invalid", STAGE)


def _require_artifact(promotion: ReleaseSubject, artifact: CandidateArtifact, target: _Target,
                      work_directory: Path) -> None:
    """Bind the loaded payload to the promotion and check every exact byte before any request."""
    if (not isinstance(artifact, CandidateArtifact) or artifact.locator != promotion.locator
            or artifact.manifest != promotion.manifest):
        raise VerificationError("candidate_delivery_artifact_mismatch", STAGE)
    payload = artifact.payload
    _canonical_directory(payload)
    _canonical_directory(work_directory)
    if payload.is_relative_to(work_directory) or work_directory.is_relative_to(payload):
        raise VerificationError("candidate_delivery_directories_overlap", STAGE)
    names: list[str] = []
    try:
        with os.scandir(payload) as entries:
            for entry in entries:
                names.append(entry.name)
                if len(names) > PAYLOAD_MEMBER_COUNT:
                    break
    except OSError:
        raise VerificationError("candidate_delivery_path_invalid", STAGE) from None
    if sorted(names) != sorted(target.files):
        raise VerificationError("candidate_delivery_payload_inventory_mismatch", STAGE)
    for name, expected in target.files.items():
        _require_payload_file(payload, name, expected)


def _require_payload_file(payload: Path, name: str, expected: tuple[int, str]) -> None:
    """The named regular, non-symlink file still holds exactly the manifest's bytes."""
    size, digest = expected
    observed = bounded_fingerprint(payload / name, size, "candidate_delivery_payload_mismatch")
    if (observed.size, observed.sha256) != (size, digest):
        raise VerificationError("candidate_delivery_payload_mismatch", STAGE)


def _observe(client: GitHubClient, target: _Target) -> _Remote:
    """Read the tag, then find the release by its tag in the complete listing."""
    tag_commit = _observe_tag(client, target)
    try:
        row = client.release(target.repo, target.tag)
    except VerificationError as error:
        if not _release_not_found(error):
            raise
        return _Remote(release_id=None, release=Absent.ABSENT, tag_commit=tag_commit)
    return _observed(client, target, row, tag_commit)


def _observe_known(client: GitHubClient, target: _Target, release_id: int, *, conflict: str) -> _Remote:
    """Read the tag, then the known release by its ID; ``conflict`` names a listed other release.

    The complete listing may omit a release created moments ago, so only its exact
    not-found answer is tolerated; it is never a substitute for the read by ID.
    """
    positive_id(release_id, STAGE)
    tag_commit = _observe_tag(client, target)
    try:
        listed = client.release(target.repo, target.tag)
    except VerificationError as error:
        if not _release_not_found(error):
            raise
    else:
        if listed.get("id") != release_id:
            raise VerificationError(conflict, STAGE)
    row = object_value(client.json(f"repos/{target.repo}/releases/{release_id}",
                                   limit=MAX_RELEASE_PAGE_BYTES), STAGE)
    if positive_id(row.get("id"), STAGE) != release_id:
        raise VerificationError("candidate_release_identity_conflict", STAGE)
    return _observed(client, target, row, tag_commit)


def _require_latest(client: GitHubClient, target: _Target, *, published: bool) -> str | Absent:
    """Refuse unless the target advances GitHub's latest release, or, ``published``, already is it.

    ``published`` is ``decide_release``'s complete matching published release. The
    whole listing is read before any decision, so a late page fault still refuses.
    Return the checked latest tag, or ``Absent.ABSENT`` when no release is published.
    """
    stable = False
    for value in iter_releases(client, target.repo):
        row = object_value(value, STAGE)
        draft, prerelease = row.get("draft"), row.get("prerelease")
        if type(draft) is not bool or type(prerelease) is not bool:
            raise VerificationError("candidate_github_latest_invalid", STAGE)
        stable = stable or not (draft or prerelease)
    latest: str | Absent = Absent.ABSENT
    latest_tag: str | Absent = Absent.ABSENT
    if stable:
        row = object_value(client.json(f"repos/{target.repo}/releases/latest", limit=MAX_RELEASE_PAGE_BYTES),
                           STAGE)
        tag = row.get("tag_name")
        if (row.get("draft") is not False or row.get("prerelease") is not False or type(tag) is not str
                or not tag.startswith("v")):
            raise VerificationError("candidate_github_latest_invalid", STAGE)
        latest_tag, latest = tag, tag[1:]
    require_latest_delivery(version=target.version, latest_version=latest, resume_published=published)
    return latest_tag


def _release_not_found(error: VerificationError) -> bool:
    return (error.code, error.stage) == ("github_release_not_found", "github-release")


def _observed(client: GitHubClient, target: _Target, row: dict[str, object],
              tag_commit: str | Absent) -> _Remote:
    """Validate one release row, then read its complete asset listing."""
    release_id = positive_id(row.get("id"), STAGE)
    draft, prerelease = row.get("draft"), row.get("prerelease")
    commitish, body = row.get("target_commitish"), row.get("body")
    if (row.get("tag_name") != target.tag or type(draft) is not bool or type(prerelease) is not bool
            or type(commitish) is not str or type(body) is not str):
        raise VerificationError("candidate_release_row_invalid", STAGE)
    if tag_commit is Absent.ABSENT:
        if not draft:
            raise VerificationError("candidate_release_tag_missing", STAGE)
        named = commitish
    else:
        # A tag fixes the release commit; a published target_commitish may be a branch name.
        named = tag_commit
    observed = ObservedRelease(tag=target.tag, target_commit=named, draft=draft, prerelease=prerelease,
                               assets=_assets(client, target, release_id), body=body)
    return _Remote(release_id=release_id, release=observed, tag_commit=tag_commit)


def _observe_tag(client: GitHubClient, target: _Target) -> str | Absent:
    """Only a successful listing without the exact ref is absence; a present tag must name the merge."""
    ref = f"refs/tags/{target.tag}"
    rows = client.json(f"repos/{target.repo}/git/matching-refs/tags/{target.tag}", limit=_TAG_REFS_LIMIT)
    if not isinstance(rows, list) or len(rows) > MAX_TAG_REFS:
        raise VerificationError("candidate_tag_refs_invalid", STAGE)
    exact = 0
    for value in rows:
        name = object_value(value, STAGE).get("ref")
        if type(name) is not str or not name.startswith(ref):
            raise VerificationError("candidate_tag_refs_invalid", STAGE)
        exact += name == ref
    if exact > 1:
        raise VerificationError("candidate_tag_refs_invalid", STAGE)
    if exact == 0:
        return Absent.ABSENT
    commit = resolve_release_identity(target.repo, target.tag, client.receipt, client.environment)["commit_sha"]
    if commit != target.merged_commit:
        raise VerificationError("candidate_release_tag_mismatch", STAGE)
    return commit


def _assets(client: GitHubClient, target: _Target, release_id: int) -> tuple[ReleaseAsset, ...]:
    """Every listed asset, fully uploaded, uniquely identified and of its digest's exact size."""
    ids: set[int] = set()
    assets: list[ReleaseAsset] = []
    for value in iter_release_assets(client, target.repo, release_id):
        row = object_value(value, STAGE)
        asset_id = positive_id(row.get("id"), STAGE)
        name, size, digest = row.get("name"), row.get("size"), row.get("digest")
        if asset_id in ids:
            raise VerificationError("candidate_release_asset_duplicate", STAGE)
        ids.add(asset_id)
        if (type(name) is not str or type(size) is not int or size <= 0 or type(digest) is not str
                or not digest.startswith(_DIGEST_PREFIX)
                or HEX_SHA256.fullmatch(digest[len(_DIGEST_PREFIX):]) is None):
            raise VerificationError("candidate_release_asset_invalid", STAGE)
        if row.get("state") != "uploaded":
            raise VerificationError("candidate_release_asset_not_uploaded", STAGE)
        if name in target.files and target.files[name][0] != size:
            raise VerificationError("candidate_release_asset_conflict", STAGE)
        assets.append(ReleaseAsset(name=name, sha256=digest[len(_DIGEST_PREFIX):]))
    if len({asset.name for asset in assets}) != len(assets):
        raise VerificationError("candidate_release_asset_duplicate", STAGE)
    return tuple(assets)


def _prepared(remote: _Remote) -> PreparedRelease:
    if remote.release_id is None or isinstance(remote.release, Absent):
        raise VerificationError("candidate_release_not_prepared", STAGE)
    return PreparedRelease(release_id=remote.release_id, observed=remote.release)


def _acknowledged(reply: object, expected: Mapping[str, object]) -> int:
    """Accept only a reply object whose named fields equal the request; return its release ID."""
    if not isinstance(reply, dict) or any(type(reply.get(key)) is not type(value) or reply.get(key) != value
                                          for key, value in expected.items()):
        raise VerificationError("candidate_release_ack_invalid", STAGE)
    identifier = reply.get("id")
    if type(identifier) is not int or identifier <= 0:
        raise VerificationError("candidate_release_ack_invalid", STAGE)
    return identifier


def release_notes_endpoint(repo: str) -> str:
    """GitHub's generate-notes endpoint: a POST that only returns generated text and saves nothing."""
    return f"repos/{repo}/releases/generate-notes"


def _release_notes(client: GitHubClient, target: _Target, previous: str | Absent, work_directory: Path) -> object:
    """Generate notes for the merged commit since ``previous``, the latest tag the guard just checked.

    The range is pinned explicitly; it is left to GitHub only when no release is
    published. The reply is untrusted; ``release_text`` validates its body.
    """
    request: dict[str, object] = {"tag_name": target.tag, "target_commitish": target.merged_commit}
    if previous is not Absent.ABSENT:
        request["previous_tag_name"] = previous
    reply = client.json_body(release_notes_endpoint(target.repo), request, method="POST",
                             body_path=work_directory / "release-notes.json", limit=MAX_RELEASE_PAGE_BYTES)
    if not isinstance(reply, dict) or type(reply.get("name")) is not str:
        raise VerificationError("candidate_release_notes_invalid", STAGE)
    return reply.get("body")


def _create_draft(client: GitHubClient, target: _Target, body: str, work_directory: Path) -> int:
    request: dict[str, object] = {"tag_name": target.tag, "target_commitish": target.merged_commit,
                                  "name": target.tag, "draft": True, "prerelease": False, "body": body}
    reply = client.json_body(f"repos/{target.repo}/releases", request, method="POST",
                             body_path=work_directory / "release-draft.json", limit=MAX_RELEASE_PAGE_BYTES)
    return _acknowledged(reply, request)


def _create_tag(client: GitHubClient, target: _Target, work_directory: Path) -> None:
    """Create the lightweight tag once at the merged commit; never update or force a ref."""
    ref = f"refs/tags/{target.tag}"
    reply = client.json_body(f"repos/{target.repo}/git/refs", {"ref": ref, "sha": target.merged_commit},
                             method="POST", body_path=work_directory / "tag-ref.json")
    created = reply.get("object") if isinstance(reply, dict) else None
    if (not isinstance(reply, dict) or reply.get("ref") != ref or not isinstance(created, dict)
            or created.get("type") != "commit" or created.get("sha") != target.merged_commit):
        raise VerificationError("candidate_tag_ack_invalid", STAGE)
    identity = resolve_release_identity(target.repo, target.tag, client.receipt, client.environment)
    if identity["commit_sha"] != target.merged_commit:
        raise VerificationError("candidate_release_tag_mismatch", STAGE)
