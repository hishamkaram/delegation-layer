"""Pure exact-byte delivery decisions for a CI distribution.

The subject is the manifest and GitHub-derived artifact identity. It carries
no approval or mutable state. Destination readers supply positive observations;
these functions decide whether the existing GitHub/npm/tap edge acts or skips.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum

from scripts.acceptance.evidence.digests import sha
from scripts.release_verification.candidate_codec import VERSION_RE
from scripts.acceptance.evidence.source import text
from scripts.release_verification.candidate_manifest import MANIFEST_PATH
from scripts.release_verification.candidate_manifest import ArtifactEntry
from scripts.release_verification.candidate_manifest import ArtifactKind
from scripts.release_verification.candidate_manifest import CandidateLocator
from scripts.release_verification.candidate_manifest import CandidateManifest
from scripts.release_verification.candidate_manifest import encode_manifest
from scripts.release_verification.candidate_manifest import payload_paths
from scripts.acceptance.evidence.source import HEX_SHA256
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.published_contract import NPM_PACKAGE
from scripts.release_verification.published_contract import PackageIdentity
from scripts.release_verification.published_contract import git_blob_sha
from scripts.release_verification.published_contract import npm_integrity
from scripts.release_verification.published_contract import npm_shasum


STAGE = "decide-candidate-delivery"


@dataclass(frozen=True)
class ReleaseFile:
    name: str
    sha256: str


@dataclass(frozen=True)
class ReleaseSubject:
    """The exact distribution and its observed CI artifact identity."""
    locator: CandidateLocator
    manifest: CandidateManifest

    def __post_init__(self) -> None:
        if (not isinstance(self.locator, CandidateLocator) or not isinstance(self.manifest, CandidateManifest)
                or sha(encode_manifest(self.manifest)) != self.locator.manifest_sha256):
            raise VerificationError("distribution_subject_mismatch", STAGE)

    @property
    def subjects(self) -> tuple[ReleaseFile, ...]:
        digests = {entry.path: entry.sha256 for entry in self.manifest.artifacts}
        digests[MANIFEST_PATH] = self.locator.manifest_sha256
        return tuple(ReleaseFile(name, digests[name]) for name in payload_paths(self.manifest.version))


def release_body(promotion: ReleaseSubject) -> str:
    """Human-readable CI provenance and exact delivered file digests."""
    locator = promotion.locator
    return "\n".join((
        "Built distribution",
        f"Source: {promotion.manifest.source.commit}",
        f"CI: https://github.com/{locator.repository}/actions/runs/{locator.run_id}",
        f"Artifact: {locator.artifact_id} (producer attempt {locator.run_attempt})",
        "",
        *(f"{item.sha256}  {item.name}" for item in promotion.subjects),
    ))


# The verification block's reserved summary; it appears exactly once in an accepted composed body.
VERIFICATION_SUMMARY = "<summary>Build verification details</summary>"


_VERIFICATION_OPENER = f"<details>\n{VERIFICATION_SUMMARY}\n\n```text\n"


_VERIFICATION_CLOSER = "\n```\n\n</details>\n"


def verification_block(canonical: str) -> str:
    """Return the ``release_body`` text ``canonical`` byte for byte inside the block that ends a composed body."""
    return _VERIFICATION_OPENER + canonical + _VERIFICATION_CLOSER


def release_text(notes: object, promotion: ReleaseSubject) -> str:
    """Compose untrusted human ``notes`` first and the exact verification block last.

    The notes are presentation only. They must be non-blank text that does not
    contain the reserved summary, so the composed body has exactly one block.
    """
    if type(notes) is not str or not notes.strip() or VERIFICATION_SUMMARY in notes:
        raise VerificationError("candidate_release_notes_invalid", STAGE)
    return notes.rstrip() + "\n\n" + verification_block(release_body(promotion))


def release_body_matches(body: str, canonical: str) -> bool:
    """Whether ``body`` is exactly the ``release_body`` text ``canonical`` or human text ending in its block.

    The block must close the body, start on a new line after non-blank text,
    and hold the only reserved summary. The human text before it may differ in
    any way, including its line endings; the block may not differ by one byte.
    """
    if body == canonical:
        return True
    block = verification_block(canonical)
    prefix = body.removesuffix(block)
    return (len(prefix) < len(body) and prefix.endswith("\n") and bool(prefix.strip())
            and body.count(VERIFICATION_SUMMARY) == 1)


class Absent(Enum):
    """The destination explicitly answered that the object does not exist.

    Only I/O that parsed that explicit answer may supply it.
    """

    ABSENT = "absent"


class Delivery(Enum):
    SKIP = "skip"
    ACT = "act"


def _refuse_resume_mutation(resume_published: bool) -> None:
    """``resume_published`` finishes delivery without any npm or GitHub-release mutation."""
    if resume_published:
        raise VerificationError("candidate_resume_mutation_refused", STAGE)


def _entry(manifest: CandidateManifest, kind: ArtifactKind) -> ArtifactEntry:
    (entry,) = (item for item in manifest.artifacts if item.kind is kind)
    return entry


@dataclass(frozen=True)
class ReleaseAsset:
    """One listed release asset and the digest of its bytes."""

    name: str
    sha256: str

    def __post_init__(self) -> None:
        if (not isinstance(self.name, str) or not self.name
                or not isinstance(self.sha256, str) or HEX_SHA256.fullmatch(self.sha256) is None):
            raise VerificationError("candidate_release_observation_invalid", STAGE)


@dataclass(frozen=True)
class ObservedRelease:
    """The release for the candidate tag as returned by a successful read.

    ``target_commit`` is the commit the release names: a draft's
    ``target_commitish`` or a published release's resolved tag commit.
    ``assets`` is the complete listing and ``body`` the release text as read.
    """

    tag: str
    target_commit: str
    draft: bool
    prerelease: bool
    assets: tuple[ReleaseAsset, ...]
    body: str

    def __post_init__(self) -> None:
        names = [asset.name for asset in self.assets if isinstance(asset, ReleaseAsset)]
        if (not isinstance(self.tag, str) or not isinstance(self.target_commit, str)
                or not isinstance(self.draft, bool) or not isinstance(self.prerelease, bool)
                or not isinstance(self.assets, tuple) or len(names) != len(self.assets)
                or len(set(names)) != len(names) or not isinstance(self.body, str)):
            raise VerificationError("candidate_release_observation_invalid", STAGE)


@dataclass(frozen=True)
class ReleasePlan:
    """Remaining release steps; ``publish`` runs only after uploads and the npm step."""

    create_draft: bool
    upload: tuple[str, ...]
    publish: bool

    @property
    def complete(self) -> bool:
        return not (self.create_draft or self.upload or self.publish)


def decide_release(*, promotion: ReleaseSubject, observed: ObservedRelease | Absent,
                   resume_published: bool) -> ReleasePlan:
    """Complete only missing assets of a matching release; refuse any other difference.

    A release whose body ``release_body_matches`` refuses is a different
    release, whether draft or published; it is never rewritten.
    """
    expected = {subject.name: subject.sha256 for subject in promotion.subjects}
    if observed is Absent.ABSENT:
        _refuse_resume_mutation(resume_published)
        return ReleasePlan(create_draft=True, upload=tuple(expected), publish=True)
    if (observed.tag, observed.prerelease) != (promotion.manifest.build_tag, False):
        raise VerificationError("candidate_release_identity_mismatch", STAGE)
    if observed.target_commit != promotion.manifest.source.commit:
        raise VerificationError("candidate_release_target_mismatch", STAGE)
    if not release_body_matches(observed.body, release_body(promotion)):
        raise VerificationError("candidate_release_body_mismatch", STAGE)
    present = {asset.name: asset.sha256 for asset in observed.assets}
    if not set(present) <= set(expected):
        raise VerificationError("candidate_release_asset_unexpected", STAGE)
    if any(expected[name] != digest for name, digest in present.items()):
        raise VerificationError("candidate_release_asset_conflict", STAGE)
    missing = tuple(name for name in expected if name not in present)
    if not observed.draft:
        if missing:
            raise VerificationError("candidate_release_published_incomplete", STAGE)
        return ReleasePlan(create_draft=False, upload=(), publish=False)
    _refuse_resume_mutation(resume_published)
    return ReleasePlan(create_draft=False, upload=missing, publish=True)


def _version_key(value: object) -> tuple[int, int, int]:
    """Numeric order of a canonical three-component version; ``0.9.0`` precedes ``0.10.0``."""
    major, minor, patch = text(value, VERSION_RE, STAGE).split(".")
    return int(major), int(minor), int(patch)


def require_latest_delivery(*, version: str, latest_version: str | Absent, resume_published: bool) -> None:
    """Refuse delivering ``version`` unless it is, or becomes, the latest published release.

    ``latest_version`` is the version of the destination's latest published
    release, or ``Absent.ABSENT`` when a successful read found none. Normal
    promotion requires a strictly newer version. ``resume_published`` asserts
    that ``decide_release`` already found the complete matching published
    release, so the only accepted latest is that same version; this function
    does not verify that assertion. An older version is always refused.
    """
    if type(resume_published) is not bool:
        raise VerificationError("candidate_field_invalid", STAGE)
    candidate = _version_key(version)
    latest = None if latest_version is Absent.ABSENT else _version_key(latest_version)
    if resume_published:
        if latest != candidate:
            raise VerificationError("candidate_version_resume_not_latest", STAGE)
        return
    if latest is not None and candidate <= latest:
        raise VerificationError("candidate_version_not_newer", STAGE)


@dataclass(frozen=True)
class NpmPackage:
    """The registry identity of the exact candidate TGZ, derived from its bytes."""

    version: str
    integrity: str
    shasum: str


def npm_package(manifest: CandidateManifest, data: bytes) -> NpmPackage:
    if sha(data) != _entry(manifest, ArtifactKind.NPM_PACKAGE).sha256:
        raise VerificationError("candidate_npm_package_digest_mismatch", STAGE)
    return NpmPackage(version=manifest.version, integrity=npm_integrity(data), shasum=npm_shasum(data))


def decide_npm(*, expected: NpmPackage, observed: PackageIdentity | Absent,
               resume_published: bool) -> Delivery:
    """Decide from a ``parse_npm_view`` identity; that parser owns the registry ``gitHead`` rule."""
    if observed is Absent.ABSENT:
        _refuse_resume_mutation(resume_published)
        return Delivery.ACT
    if (observed.name, observed.version) != (NPM_PACKAGE, expected.version):
        raise VerificationError("candidate_npm_identity_mismatch", STAGE)
    if (observed.integrity, observed.shasum) != (expected.integrity, expected.shasum):
        raise VerificationError("candidate_npm_conflict", STAGE)
    return Delivery.SKIP


@dataclass(frozen=True)
class TapFormula:
    """The tap's current formula file and its Git blob name, as read by a successful request."""

    content: bytes
    blob_sha: str

    def __post_init__(self) -> None:
        if not isinstance(self.content, bytes) or git_blob_sha(self.content) != self.blob_sha:
            raise VerificationError("candidate_tap_observation_invalid", STAGE)


@dataclass(frozen=True)
class TapPlan:
    """``base_blob_sha`` is the compare-and-swap precondition of a replacement; None creates the file."""

    action: Delivery
    base_blob_sha: str | None


def decide_tap(*, manifest: CandidateManifest, formula: bytes, observed: TapFormula | Absent) -> TapPlan:
    """Write the exact candidate formula bytes unless the tap already holds them.

    Tap completion stays allowed under ``resume_published``.
    """
    if sha(formula) != _entry(manifest, ArtifactKind.HOMEBREW_FORMULA).sha256:
        raise VerificationError("candidate_formula_digest_mismatch", STAGE)
    if observed is Absent.ABSENT:
        return TapPlan(action=Delivery.ACT, base_blob_sha=None)
    if observed.content == formula:
        return TapPlan(action=Delivery.SKIP, base_blob_sha=None)
    return TapPlan(action=Delivery.ACT, base_blob_sha=observed.blob_sha)
