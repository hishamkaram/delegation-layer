"""Candidate publication reference, promotion statement, release body and delivery decisions.

This module reads no storage, network, process or clock and stores no state.
The canonical manifest and locator own the artifact inventory;
``candidate_readiness`` owns readiness, the freeze authorization and
source-tree equality; ``candidate_history`` owns the freeze record. This module
owns only the external publication reference encoding, the truthful promotion
statement and its release body, the monotonic delivery version rule, and the
decision whether one delivery step acts, skips or refuses given a positive
destination observation. No value here grants launch or
publication authority. A transport or command failure is an I/O error and is
never represented as absence.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum

from ..evidence.digests import sha
from .candidate_codec import VERSION_RE, encode_record, exact_fields, load_record, revision, sha256_hex, text
from .candidate_history import PublicationAuthorization
from .candidate_manifest import (
    MANIFEST_PATH, ArtifactEntry, ArtifactKind, CandidateManifest, payload_paths,
)
from .candidate_readiness import BoundCandidate, PublicationSource, publication_source
from .contract import HEX_SHA256, VerificationError
from .published_contract import NPM_PACKAGE, PackageIdentity, git_blob_sha, npm_integrity, npm_shasum


REFERENCE_STAGE = "validate-candidate-reference"


PROMOTION_STAGE = "validate-candidate-promotion"


STAGE = "decide-candidate-delivery"


# The largest canonical reference (three 64-hex values) is 250 bytes; this bounds decoding only.
MAX_REFERENCE_BYTES = 256


PROMOTION_SCHEMA_VERSION = 1


PROMOTION_KIND = "delegation-layer-candidate-promotion"


_REFERENCE_FIELDS = frozenset({"candidate_id", "freeze_commit", "merged_commit"})


@dataclass(frozen=True)
class CandidateReference:
    """The exact publication dispatch input: which candidate, which freeze, which merge.

    It is untrusted data. A reader must still validate the committed freeze at
    the pinned store and the merge on main before relying on any field.
    """

    candidate_id: str
    freeze_commit: str
    merged_commit: str

    def __post_init__(self) -> None:
        sha256_hex(self.candidate_id, REFERENCE_STAGE)
        revision(self.freeze_commit, REFERENCE_STAGE)
        revision(self.merged_commit, REFERENCE_STAGE)


def encode_reference(reference: CandidateReference) -> bytes:
    return encode_record({
        "candidate_id": reference.candidate_id, "freeze_commit": reference.freeze_commit,
        "merged_commit": reference.merged_commit,
    }, REFERENCE_STAGE)


def parse_reference(data: bytes) -> CandidateReference:
    """Accept only the canonical encoding of exactly the three reference fields."""
    value = load_record(data, max_bytes=MAX_REFERENCE_BYTES, stage=REFERENCE_STAGE)
    fields = exact_fields(value, _REFERENCE_FIELDS, REFERENCE_STAGE)
    return CandidateReference(candidate_id=fields["candidate_id"], freeze_commit=fields["freeze_commit"],
                              merged_commit=fields["merged_commit"])


@dataclass(frozen=True)
class PromotionSubject:
    """One delivered payload file and the digest of its exact bytes."""

    name: str
    sha256: str


@dataclass(frozen=True)
class CandidatePromotion:
    """Which exact candidate bytes a squash merge publishes, and nothing more.

    The candidate's original build (commit, tree, base) and the merge (commit,
    tree) stay separate facts. The statement claims identity and delivery; it
    never claims that the promoting run built the bytes. Construction checks
    only that the supplied values describe one candidate. It neither authorizes
    publication nor selects a freeze; tree equality belongs to
    ``publication_source``.
    """

    reference: CandidateReference
    candidate: BoundCandidate
    freeze: PublicationAuthorization
    source: PublicationSource

    def __post_init__(self) -> None:
        if (not isinstance(self.reference, CandidateReference)
                or not isinstance(self.candidate, BoundCandidate)
                or not isinstance(self.freeze, PublicationAuthorization)
                or not isinstance(self.source, PublicationSource)):
            raise VerificationError("candidate_promotion_invalid", PROMOTION_STAGE)
        built = self.candidate.manifest.source
        if (self.reference.candidate_id != self.candidate.candidate_id
                or self.reference.merged_commit != self.source.merged_commit):
            raise VerificationError("candidate_reference_mismatch", PROMOTION_STAGE)
        if (self.freeze.candidate_id, self.freeze.source_commit, self.freeze.source_tree) != (
                self.candidate.candidate_id, built.commit, built.tree):
            raise VerificationError("candidate_promotion_freeze_mismatch", PROMOTION_STAGE)
        canonical = publication_source(manifest=self.candidate.manifest,
                                       merged_commit=self.source.merged_commit,
                                       merged_tree=self.source.tree)
        if canonical != self.source:
            raise VerificationError("candidate_promotion_source_mismatch", PROMOTION_STAGE)

    @property
    def subjects(self) -> tuple[PromotionSubject, ...]:
        """Every payload file in canonical order, digests read from the bound manifest."""
        manifest = self.candidate.manifest
        digests = {entry.path: entry.sha256 for entry in manifest.artifacts}
        digests[MANIFEST_PATH] = self.candidate.locator.manifest_sha256
        return tuple(PromotionSubject(name=path, sha256=digests[path])
                     for path in payload_paths(manifest.version))


def encode_promotion(promotion: CandidatePromotion) -> bytes:
    """Return the deterministic promotion predicate; subjects belong to the enclosing statement."""
    locator = promotion.candidate.locator
    built = promotion.candidate.manifest.source
    return encode_record({
        "schema_version": PROMOTION_SCHEMA_VERSION, "kind": PROMOTION_KIND,
        "candidate_id": promotion.candidate.candidate_id,
        "locator": {
            "repository": locator.repository, "run_id": locator.run_id, "run_attempt": locator.run_attempt,
            "artifact_id": locator.artifact_id, "artifact_archive_sha256": locator.artifact_archive_sha256,
            "manifest_sha256": locator.manifest_sha256,
        },
        "freeze": {"commit": promotion.reference.freeze_commit, "record_sha256": promotion.freeze.sha256},
        "original_build": {"commit": built.commit, "tree": built.tree, "base_commit": built.base_commit},
        "merged": {"commit": promotion.source.merged_commit, "tree": promotion.source.tree},
    }, PROMOTION_STAGE)


RELEASE_BODY_TITLE = "Development candidate promotion"


def release_body(promotion: CandidatePromotion) -> str:
    """Return the only accepted release text: the canonical predicate and every subject digest.

    The body restates ``encode_promotion`` and ``subjects`` verbatim and adds no
    fact of its own. It grants no authority; readers still validate the freeze
    and the merge.
    """
    predicate = encode_promotion(promotion).decode("utf-8").removesuffix("\n")
    digests = tuple(f"{subject.sha256}  {subject.name}" for subject in promotion.subjects)
    return "\n".join((RELEASE_BODY_TITLE, "", "Promotion predicate (canonical JSON):", predicate, "",
                      "Payload SHA-256 digests:", *digests))


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


def decide_release(*, promotion: CandidatePromotion, observed: ObservedRelease | Absent,
                   resume_published: bool) -> ReleasePlan:
    """Complete only missing assets of a matching release; refuse any other difference.

    A release whose body is not exactly ``release_body(promotion)`` is a
    different release, whether draft or published; it is never rewritten.
    """
    expected = {subject.name: subject.sha256 for subject in promotion.subjects}
    if observed is Absent.ABSENT:
        _refuse_resume_mutation(resume_published)
        return ReleasePlan(create_draft=True, upload=tuple(expected), publish=True)
    if (observed.tag, observed.prerelease) != (promotion.candidate.manifest.build_tag, False):
        raise VerificationError("candidate_release_identity_mismatch", STAGE)
    if observed.target_commit != promotion.source.merged_commit:
        raise VerificationError("candidate_release_target_mismatch", STAGE)
    if observed.body != release_body(promotion):
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
