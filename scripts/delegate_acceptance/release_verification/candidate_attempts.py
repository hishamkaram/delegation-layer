"""Immutable facts recorded for development-candidate acceptance attempts.

Values here are validated facts read from the append-only development
evidence branch, the native proof reader, and trusted reviewer inputs. They
hold no decisions; the readiness policy consumes them. Reviewed documents are
bound by the SHA-256 of their exact bytes, never by a trusted boolean.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
import re

from ..evidence.digests import sha
from ..native_evidence.block_reason import encode_block_reason
from ..native_evidence.contract import BlockReason, EvidenceError, expected_cases
from ..native_evidence.public_contract import (
    PUBLIC_TEXT, CandidateProofExpectation, CaseOutcome, PublicProofError, ValidatedCandidateProof,
)
from .candidate_codec import (
    IDENTIFIER_RE, OPAQUE_ID_RE, encode_record, exact_fields, identifier_tuple, load_record,
    non_negative_int, object_list, positive_int, require_header, revision, sha256_hex, text,
)
from .candidate_manifest import PLATFORMS, CandidateLocator, CandidateManifest, encode_manifest
from .contract import BinaryDigests, VerificationError, parse_binary_digests


STAGE = "validate-candidate-attempt"


SCHEMA_VERSION = 1


INTENT_KIND = "delegation-layer-candidate-attempt-intent"


REVIEW_KIND = "delegation-layer-candidate-source-review"


CLAIM_KIND = "delegation-layer-candidate-platform-claim"


MAX_CLAIM_BYTES = 4 * 1024


MAX_RECORD_BYTES = 256 * 1024


MAX_LIST_ITEMS = 1024


REVIEWER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9-]{0,38}$")


MAX_RATIONALE_CHARS = 4096


def provider_family(provider: str) -> str:
    return provider.split(":", 1)[0]


# Derived from the one native case-matrix owner; never a copied inventory.
PROVIDER_FAMILIES = tuple(sorted({provider_family(case.provider) for case in expected_cases()}))


# A tuple compares by equality, so malformed unhashable values fail validation.
_CASE_ID_LIST = tuple(case.case_id for case in expected_cases())


class MatrixScope(Enum):
    AFFECTED_PROVIDERS = "affected-providers"
    SHARED = "shared"
    FINAL_VERSION = "final-version"


class CellStatus(Enum):
    PASS = "PASS"
    FAIL = "FAIL"
    BLOCKED = "BLOCKED"


class Closure(Enum):
    COMPLETE = "complete"
    UNRESOLVED = "unresolved"


def sorted_identifiers(values: tuple[str, ...]) -> bool:
    return (all(isinstance(value, str) and IDENTIFIER_RE.fullmatch(value) is not None for value in values)
            and list(values) == sorted(set(values)))


@dataclass(frozen=True, order=True)
class ProviderImport:
    """An observed import from one provider package to another."""

    importer: str
    imported: str

    def __post_init__(self) -> None:
        if (self.importer not in PROVIDER_FAMILIES or self.imported not in PROVIDER_FAMILIES
                or self.importer == self.imported):
            raise VerificationError("candidate_scope_invalid", STAGE)


@dataclass(frozen=True)
class ChangeScope:
    matrix: MatrixScope
    changed_providers: tuple[str, ...]
    provider_imports: tuple[ProviderImport, ...]

    def __post_init__(self) -> None:
        if (not isinstance(self.matrix, MatrixScope)
                or not sorted_identifiers(self.changed_providers)
                or not set(self.changed_providers) <= set(PROVIDER_FAMILIES)
                or not all(isinstance(edge, ProviderImport) for edge in self.provider_imports)
                or list(self.provider_imports) != sorted(set(self.provider_imports))
                or (self.matrix is MatrixScope.AFFECTED_PROVIDERS and not self.changed_providers)):
            raise VerificationError("candidate_scope_invalid", STAGE)

    @property
    def full_matrix(self) -> bool:
        return self.matrix is not MatrixScope.AFFECTED_PROVIDERS

    def affected_providers(self) -> frozenset[str]:
        """Changed providers plus every provider that transitively imports one."""
        affected = set(self.changed_providers)
        grew = True
        while grew:
            importers = {edge.importer for edge in self.provider_imports if edge.imported in affected}
            grew = not importers <= affected
            affected |= importers
        return frozenset(affected)


@dataclass(frozen=True)
class CellResult:
    case_id: str
    status: CellStatus
    block_reason: BlockReason | None

    def __post_init__(self) -> None:
        if self.case_id not in _CASE_ID_LIST or not isinstance(self.status, CellStatus):
            raise VerificationError("candidate_cell_invalid", STAGE)
        try:
            encode_block_reason(self.status.value, self.block_reason)
        except EvidenceError as error:
            raise VerificationError("candidate_cell_invalid", STAGE) from error


def sorted_opaque_ids(values: tuple[str, ...]) -> bool:
    return (all(isinstance(value, str) and OPAQUE_ID_RE.fullmatch(value) is not None for value in values)
            and list(values) == sorted(set(values)))


@dataclass(frozen=True)
class PlatformProof:
    """One native os/arch proof archive of an attempt, as read from its bytes.

    ``invocation_id`` is the original native invocation identity and
    ``unresolved_resources`` the owned resource identities whose lifetime
    closure that invocation could not establish. ``binaries`` are the
    receipt's top-level bundled binary digests that every case bound.
    """

    candidate_id: str
    attempt: int
    os: str
    arch: str
    archive_sha256: str
    aggregate_sha256: str
    proof_sha256: str
    invocation_id: str
    closure: Closure
    unresolved_resources: tuple[str, ...]
    cells: tuple[CellResult, ...]
    binaries: BinaryDigests

    def __post_init__(self) -> None:
        sha256_hex(self.candidate_id, STAGE)
        positive_int(self.attempt, STAGE)
        for value in (self.archive_sha256, self.aggregate_sha256, self.proof_sha256):
            sha256_hex(value, STAGE)
        text(self.invocation_id, OPAQUE_ID_RE, STAGE)
        case_ids = [cell.case_id for cell in self.cells]
        if ((self.os, self.arch) not in PLATFORMS or not isinstance(self.closure, Closure)
                or not sorted_opaque_ids(self.unresolved_resources)
                or (self.closure is Closure.COMPLETE) != (not self.unresolved_resources)
                or not all(isinstance(cell, CellResult) for cell in self.cells)
                or not isinstance(self.binaries, BinaryDigests)
                or len(set(case_ids)) != len(case_ids)):
            raise VerificationError("candidate_proof_invalid", STAGE)


@dataclass(frozen=True)
class PlatformClaim:
    """Exclusive claim of one planned platform of an attempt for one native invocation.

    The claim is appended before the paid native run starts. It is distinct
    from the proof that run may later produce; every proof binds its claim.
    It also records the execution expectations the coordinator verified before
    appending it: the native harness revision and source fingerprint (distinct
    from the build harness), the bundled binary digests of the manifest-bound
    archive, and every selected model. An unselected optional model is None.
    """

    candidate_id: str
    attempt: int
    os: str
    arch: str
    invocation_id: str
    harness_revision: str
    harness_sha256: str
    binaries: BinaryDigests
    opencode_model: str
    ccr_model: str | None
    agy_model: str | None

    def __post_init__(self) -> None:
        sha256_hex(self.candidate_id, STAGE)
        positive_int(self.attempt, STAGE)
        text(self.invocation_id, OPAQUE_ID_RE, STAGE)
        revision(self.harness_revision, STAGE)
        sha256_hex(self.harness_sha256, STAGE)
        text(self.opencode_model, PUBLIC_TEXT, STAGE)
        for model in (self.ccr_model, self.agy_model):
            if model is not None:
                text(model, PUBLIC_TEXT, STAGE)
        if not isinstance(self.binaries, BinaryDigests):
            raise VerificationError("candidate_field_invalid", STAGE)
        if (self.os, self.arch) not in PLATFORMS:
            raise VerificationError("candidate_platform_invalid", STAGE)

    @property
    def platform(self) -> tuple[str, str]:
        return self.os, self.arch


def encode_claim(claim: PlatformClaim) -> bytes:
    return encode_record({
        "schema_version": SCHEMA_VERSION, "kind": CLAIM_KIND,
        "candidate_id": claim.candidate_id, "attempt": claim.attempt,
        "os": claim.os, "arch": claim.arch, "invocation_id": claim.invocation_id,
        "harness_revision": claim.harness_revision, "harness_sha256": claim.harness_sha256,
        "binaries": claim.binaries.as_mapping(), "opencode_model": claim.opencode_model,
        "ccr_model": claim.ccr_model, "agy_model": claim.agy_model,
    }, STAGE)


def claim_sha256(claim: PlatformClaim) -> str:
    return sha(encode_claim(claim))


def parse_claim(data: bytes) -> PlatformClaim:
    value = load_record(data, max_bytes=MAX_CLAIM_BYTES, stage=STAGE)
    require_header(value, schema_version=SCHEMA_VERSION, kind=CLAIM_KIND,
                   error="candidate_record_kind_invalid", stage=STAGE)
    fields = exact_fields(value, frozenset({
        "schema_version", "kind", "candidate_id", "attempt", "os", "arch", "invocation_id",
        "harness_revision", "harness_sha256", "binaries", "opencode_model", "ccr_model", "agy_model",
    }), STAGE)
    return PlatformClaim(candidate_id=fields["candidate_id"], attempt=fields["attempt"],
                         os=fields["os"], arch=fields["arch"], invocation_id=fields["invocation_id"],
                         harness_revision=fields["harness_revision"], harness_sha256=fields["harness_sha256"],
                         binaries=parse_binary_digests(fields["binaries"], STAGE),
                         opencode_model=fields["opencode_model"], ccr_model=fields["ccr_model"],
                         agy_model=fields["agy_model"])


def candidate_proof_expectation(manifest: CandidateManifest, locator: CandidateLocator,
                                claim: PlatformClaim) -> CandidateProofExpectation:
    """Derive the trusted proof expectation of one claim from its candidate, never from the ZIP."""
    if (sha(encode_manifest(manifest)) != locator.manifest_sha256
            or claim.candidate_id != locator.candidate_id):
        raise VerificationError("candidate_claim_mismatch", STAGE)
    archive = manifest.archive(claim.os, claim.arch)
    try:
        return CandidateProofExpectation(
            repository=locator.repository, candidate_id=locator.candidate_id, run_id=locator.run_id,
            run_attempt=locator.run_attempt, artifact_id=locator.artifact_id,
            artifact_archive_sha256=locator.artifact_archive_sha256,
            source_commit=manifest.source.commit, source_tree=manifest.source.tree,
            platform_os=claim.os, platform_arch=claim.arch, archive_name=archive.path,
            archive_sha256=archive.sha256, claim_sha256=claim_sha256(claim),
            invocation_id=claim.invocation_id, harness_revision=claim.harness_revision,
            harness_sha256=claim.harness_sha256, binaries=claim.binaries,
            opencode_model=claim.opencode_model, ccr_model=claim.ccr_model, agy_model=claim.agy_model,
        )
    except PublicProofError as error:
        raise VerificationError("candidate_expectation_invalid", STAGE) from error


def _cell(outcome: CaseOutcome) -> CellResult:
    # The validated outcome already carries its typed reason, non-null exactly
    # for BLOCKED; readiness alone decides which reasons are neutral.
    return CellResult(case_id=outcome.case_id, status=CellStatus(outcome.status),
                      block_reason=outcome.block_reason)


def platform_proof(validated: ValidatedCandidateProof, claim: PlatformClaim) -> PlatformProof:
    """Map the validated candidate proof of ``claim`` to its platform proof."""
    if (not isinstance(validated, ValidatedCandidateProof) or validated.claim_sha256 != claim_sha256(claim)
            or validated.candidate_id != claim.candidate_id or validated.invocation_id != claim.invocation_id
            or (validated.platform_os, validated.platform_arch) != claim.platform
            or validated.binaries != claim.binaries):
        raise VerificationError("candidate_proof_claim_mismatch", STAGE)
    unresolved = validated.unresolved_case_ids
    return PlatformProof(
        candidate_id=claim.candidate_id, attempt=claim.attempt, os=claim.os, arch=claim.arch,
        archive_sha256=validated.archive_sha256, aggregate_sha256=validated.aggregate_sha256,
        proof_sha256=validated.public_zip_sha256, invocation_id=claim.invocation_id,
        closure=Closure.UNRESOLVED if unresolved else Closure.COMPLETE, unresolved_resources=unresolved,
        cells=tuple(_cell(outcome) for outcome in validated.cases), binaries=validated.binaries,
    )


def valid_platforms(platforms: tuple[tuple[str, str], ...]) -> bool:
    return (bool(platforms) and all(platform in PLATFORMS for platform in platforms)
            and list(platforms) == sorted(set(platforms)))


@dataclass(frozen=True)
class Replacement:
    """The reviewed triage artifact that explains the prior attempt and approves this one."""

    prior_attempt: int
    triage_sha256: str

    def __post_init__(self) -> None:
        positive_int(self.prior_attempt, STAGE)
        sha256_hex(self.triage_sha256, STAGE)


@dataclass(frozen=True)
class AttemptIntent:
    candidate_id: str
    attempt: int
    source_commit: str
    scope: ChangeScope
    platforms: tuple[tuple[str, str], ...]
    replaces: Replacement | None

    def __post_init__(self) -> None:
        sha256_hex(self.candidate_id, STAGE)
        positive_int(self.attempt, STAGE)
        revision(self.source_commit, STAGE)
        expected_prior = self.attempt - 1 if self.attempt > 1 else None
        observed_prior = self.replaces.prior_attempt if self.replaces is not None else None
        if (not isinstance(self.scope, ChangeScope) or not valid_platforms(self.platforms)
                or observed_prior != expected_prior):
            raise VerificationError("candidate_intent_invalid", STAGE)


@dataclass(frozen=True)
class ReviewProvenance:
    """The Git maintainer transaction that appended one reviewed artifact.

    Only the trusted transaction reader constructs this after verifying the
    transaction externally; a parsed artifact and its hash are not provenance.
    """

    artifact_sha256: str
    reviewer: str
    transaction_commit: str

    def __post_init__(self) -> None:
        sha256_hex(self.artifact_sha256, STAGE)
        text(self.reviewer, REVIEWER_RE, STAGE)
        revision(self.transaction_commit, STAGE)

    def binds(self, *, artifact_sha256: str, reviewer: str) -> bool:
        return (self.artifact_sha256, self.reviewer) == (artifact_sha256, reviewer)


def _rationale(value: object) -> bool:
    """Bounded, trimmed printable ASCII prose; line breaks are the only control characters."""
    return (isinstance(value, str) and 0 < len(value) <= MAX_RATIONALE_CHARS and value.isascii()
            and value == value.strip() and all(char == "\n" or char.isprintable() for char in value))


@dataclass(frozen=True)
class ReviewedChangeScope:
    """The change scope a reviewer attested for one complete base-to-head source diff.

    ``changed_path_count`` and ``changed_paths_sha256`` identify the complete
    changed-path inventory, and ``subsystem_registry_sha256`` the canonical
    subsystem registry the reviewer classified it against. The record holds
    attested values only: the I/O edge that writes a review observes the diff
    and its completeness; parsing never proves either. ``rationale`` explains
    the classification and is never executable policy.
    """

    scope: ChangeScope
    base_commit: str
    base_tree: str
    changed_path_count: int
    changed_paths_sha256: str
    subsystem_registry_sha256: str
    rationale: str

    def __post_init__(self) -> None:
        revision(self.base_commit, STAGE)
        revision(self.base_tree, STAGE)
        non_negative_int(self.changed_path_count, STAGE)
        sha256_hex(self.changed_paths_sha256, STAGE)
        sha256_hex(self.subsystem_registry_sha256, STAGE)
        if not isinstance(self.scope, ChangeScope) or not _rationale(self.rationale):
            raise VerificationError("candidate_review_invalid", STAGE)


@dataclass(frozen=True)
class SourceReview:
    """Review of the frozen head, tree, candidate manifest and change scope by one reviewer."""

    candidate_id: str
    head_commit: str
    tree: str
    reviewer: str
    unresolved_findings: tuple[str, ...]
    reviewed_scope: ReviewedChangeScope

    def __post_init__(self) -> None:
        sha256_hex(self.candidate_id, STAGE)
        revision(self.head_commit, STAGE)
        revision(self.tree, STAGE)
        text(self.reviewer, REVIEWER_RE, STAGE)
        if (not sorted_identifiers(self.unresolved_findings)
                or not isinstance(self.reviewed_scope, ReviewedChangeScope)):
            raise VerificationError("candidate_review_invalid", STAGE)


@dataclass(frozen=True)
class ReviewedSource:
    """A source review bound to the verified transaction that recorded its exact bytes."""

    record: SourceReview
    provenance: ReviewProvenance

    def __post_init__(self) -> None:
        if (not isinstance(self.record, SourceReview) or not isinstance(self.provenance, ReviewProvenance)
                or not self.provenance.binds(artifact_sha256=self.sha256, reviewer=self.record.reviewer)):
            raise VerificationError("candidate_review_provenance_mismatch", STAGE)

    @property
    def sha256(self) -> str:
        return sha(encode_review(self.record))


def scope_record(scope: ChangeScope) -> dict[str, object]:
    return {"matrix": scope.matrix.value, "changed_providers": list(scope.changed_providers),
            "provider_imports": [{"importer": edge.importer, "imported": edge.imported}
                                 for edge in scope.provider_imports]}


def platforms_record(platforms: tuple[tuple[str, str], ...]) -> list[dict[str, str]]:
    return [{"os": os_name, "arch": arch} for os_name, arch in platforms]


def encode_intent(intent: AttemptIntent) -> bytes:
    replaces = intent.replaces
    return encode_record({
        "schema_version": SCHEMA_VERSION, "kind": INTENT_KIND,
        "candidate_id": intent.candidate_id, "attempt": intent.attempt,
        "source_commit": intent.source_commit, "scope": scope_record(intent.scope),
        "platforms": platforms_record(intent.platforms),
        "replaces": None if replaces is None else {
            "prior_attempt": replaces.prior_attempt, "triage_sha256": replaces.triage_sha256},
    }, STAGE)


def load_kind(data: bytes, kind: str, fields: frozenset[str]) -> dict[str, object]:
    value = load_record(data, max_bytes=MAX_RECORD_BYTES, stage=STAGE)
    require_header(value, schema_version=SCHEMA_VERSION, kind=kind,
                   error="candidate_record_kind_invalid", stage=STAGE)
    return exact_fields(value, fields | {"schema_version", "kind"}, STAGE)


def parse_scope(value: object) -> ChangeScope:
    fields = exact_fields(value, frozenset({"matrix", "changed_providers", "provider_imports"}), STAGE)
    edges = []
    for item in object_list(fields["provider_imports"], max_items=MAX_LIST_ITEMS, stage=STAGE):
        edge = exact_fields(item, frozenset({"importer", "imported"}), STAGE)
        edges.append(ProviderImport(importer=text(edge["importer"], IDENTIFIER_RE, STAGE),
                                    imported=text(edge["imported"], IDENTIFIER_RE, STAGE)))
    try:
        matrix = MatrixScope(fields["matrix"])
    except ValueError:
        raise VerificationError("candidate_scope_invalid", STAGE) from None
    return ChangeScope(
        matrix=matrix,
        changed_providers=identifier_tuple(fields["changed_providers"], max_items=MAX_LIST_ITEMS,
                                           stage=STAGE),
        provider_imports=tuple(edges),
    )


def parse_platforms(value: object) -> tuple[tuple[str, str], ...]:
    platforms = []
    for item in object_list(value, max_items=len(PLATFORMS), stage=STAGE):
        fields = exact_fields(item, frozenset({"os", "arch"}), STAGE)
        platform = (fields["os"], fields["arch"])
        if platform not in PLATFORMS:
            raise VerificationError("candidate_platform_invalid", STAGE)
        platforms.append(platform)
    return tuple(platforms)


def parse_intent(data: bytes) -> AttemptIntent:
    fields = load_kind(data, INTENT_KIND, frozenset({
        "candidate_id", "attempt", "source_commit", "scope", "platforms", "replaces"}))
    replaces = None
    if fields["replaces"] is not None:
        prior = exact_fields(fields["replaces"], frozenset({"prior_attempt", "triage_sha256"}), STAGE)
        replaces = Replacement(prior_attempt=prior["prior_attempt"], triage_sha256=prior["triage_sha256"])
    return AttemptIntent(
        candidate_id=fields["candidate_id"], attempt=fields["attempt"],
        source_commit=fields["source_commit"], scope=parse_scope(fields["scope"]),
        platforms=parse_platforms(fields["platforms"]), replaces=replaces,
    )


def encode_review(record: SourceReview) -> bytes:
    reviewed = record.reviewed_scope
    return encode_record({
        "schema_version": SCHEMA_VERSION, "kind": REVIEW_KIND,
        "candidate_id": record.candidate_id, "head_commit": record.head_commit, "tree": record.tree,
        "reviewer": record.reviewer, "unresolved_findings": list(record.unresolved_findings),
        "reviewed_scope": {
            "scope": scope_record(reviewed.scope), "base_commit": reviewed.base_commit,
            "base_tree": reviewed.base_tree, "changed_path_count": reviewed.changed_path_count,
            "changed_paths_sha256": reviewed.changed_paths_sha256,
            "subsystem_registry_sha256": reviewed.subsystem_registry_sha256,
            "rationale": reviewed.rationale,
        },
    }, STAGE)


def _parse_reviewed_scope(value: object) -> ReviewedChangeScope:
    fields = exact_fields(value, frozenset({
        "scope", "base_commit", "base_tree", "changed_path_count", "changed_paths_sha256",
        "subsystem_registry_sha256", "rationale"}), STAGE)
    return ReviewedChangeScope(
        scope=parse_scope(fields["scope"]), base_commit=fields["base_commit"], base_tree=fields["base_tree"],
        changed_path_count=fields["changed_path_count"], changed_paths_sha256=fields["changed_paths_sha256"],
        subsystem_registry_sha256=fields["subsystem_registry_sha256"], rationale=fields["rationale"],
    )


def parse_review(data: bytes) -> SourceReview:
    """Return the raw record only; review authority is established by ``ReviewedSource``."""
    fields = load_kind(data, REVIEW_KIND, frozenset({
        "candidate_id", "head_commit", "tree", "reviewer", "unresolved_findings", "reviewed_scope"}))
    return SourceReview(
        candidate_id=fields["candidate_id"], head_commit=fields["head_commit"], tree=fields["tree"],
        reviewer=fields["reviewer"],
        unresolved_findings=identifier_tuple(fields["unresolved_findings"], max_items=MAX_LIST_ITEMS,
                                             stage=STAGE),
        reviewed_scope=_parse_reviewed_scope(fields["reviewed_scope"]),
    )
