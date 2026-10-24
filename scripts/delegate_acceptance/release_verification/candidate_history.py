"""Reviewed triage, publication freeze and the candidate history read model.

One triage artifact explains every canonical finding of a prior attempt and
may embed the typed approval of the next paid attempt. Parsing yields only
the raw record. A ``ReviewedTriage`` additionally requires the provenance
fact from the trusted Git maintainer transaction reader and the typed closure
resolutions from the existing native lifetime validator; a hash or a human
claim alone never establishes review or resource closure.
"""

from __future__ import annotations

from dataclasses import dataclass
import re

from ..evidence.digests import sha
from .candidate_attempts import (
    MAX_LIST_ITEMS, MAX_RECORD_BYTES, REVIEWER_RE, SCHEMA_VERSION, AttemptIntent, ChangeScope, PlatformClaim,
    PlatformProof, ReviewProvenance, load_kind, parse_platforms, parse_scope, platforms_record,
    scope_record, sorted_identifiers, valid_platforms,
)
from .candidate_closure import (
    ClosureResolution, ClosureResolutionRef, parse_subject, subject_record,
)
from .candidate_codec import (
    HEX_SHA256, encode_record, exact_fields, identifier_tuple,
    object_list, positive_int, revision, sha256_hex, text,
)
from .candidate_install_contract import InstallChannel, InstallProof
from .contract import VerificationError


STAGE = "validate-candidate-triage"


TRIAGE_KIND = "delegation-layer-candidate-attempt-triage"


FINDING_CODE_RE = re.compile(r"^[a-z][a-z_]{0,63}$")


FINDING_SUBJECT_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/-]{0,191}$")


@dataclass(frozen=True, order=True)
class FindingRef:
    """A canonical policy finding: a stable code and the fact it concerns."""

    code: str
    subject: str

    def __post_init__(self) -> None:
        text(self.code, FINDING_CODE_RE, STAGE)
        text(self.subject, FINDING_SUBJECT_RE, STAGE)


@dataclass(frozen=True)
class ReplacementApproval:
    """The exact next attempt a reviewer approves; not a hash of its future intent."""

    source_commit: str
    next_attempt: int
    scope: ChangeScope
    platforms: tuple[tuple[str, str], ...]

    def __post_init__(self) -> None:
        revision(self.source_commit, STAGE)
        positive_int(self.next_attempt, STAGE)
        if not isinstance(self.scope, ChangeScope) or not valid_platforms(self.platforms):
            raise VerificationError("candidate_triage_invalid", STAGE)


def _sorted_digests(values: tuple[str, ...]) -> bool:
    return (all(isinstance(value, str) and HEX_SHA256.fullmatch(value) is not None for value in values)
            and list(values) == sorted(set(values)))


def _sorted_unique(values: tuple[object, ...], kind: type) -> bool:
    return all(isinstance(value, kind) for value in values) and list(values) == sorted(set(values))


@dataclass(frozen=True)
class TriageRecord:
    candidate_id: str
    attempt: int
    prior_claim_sha256s: tuple[str, ...]
    prior_proof_sha256s: tuple[str, ...]
    explained_findings: tuple[FindingRef, ...]
    diagnosis_sha256s: tuple[str, ...]
    unresolved_product_findings: tuple[str, ...]
    reviewer: str
    closure_resolutions: tuple[ClosureResolutionRef, ...]
    approval: ReplacementApproval | None

    def __post_init__(self) -> None:
        sha256_hex(self.candidate_id, STAGE)
        positive_int(self.attempt, STAGE)
        text(self.reviewer, REVIEWER_RE, STAGE)
        keys = [ref.key for ref in self.closure_resolutions if isinstance(ref, ClosureResolutionRef)]
        if (not self.prior_claim_sha256s or not _sorted_digests(self.prior_claim_sha256s)
                or not _sorted_digests(self.prior_proof_sha256s)
                or not _sorted_unique(self.explained_findings, FindingRef)
                or not _sorted_digests(self.diagnosis_sha256s)
                or (bool(self.explained_findings) and not self.diagnosis_sha256s)
                or not sorted_identifiers(self.unresolved_product_findings)
                or len(keys) != len(self.closure_resolutions) or keys != sorted(set(keys))
                or not (self.approval is None or (isinstance(self.approval, ReplacementApproval)
                                                  and self.approval.next_attempt == self.attempt + 1))):
            raise VerificationError("candidate_triage_invalid", STAGE)


def encode_triage(record: TriageRecord) -> bytes:
    approval = record.approval
    return encode_record({
        "schema_version": SCHEMA_VERSION, "kind": TRIAGE_KIND,
        "candidate_id": record.candidate_id, "attempt": record.attempt,
        "prior_claim_sha256s": list(record.prior_claim_sha256s),
        "prior_proof_sha256s": list(record.prior_proof_sha256s),
        "explained_findings": [{"code": item.code, "subject": item.subject}
                               for item in record.explained_findings],
        "diagnosis_sha256s": list(record.diagnosis_sha256s),
        "unresolved_product_findings": list(record.unresolved_product_findings),
        "reviewer": record.reviewer,
        "closure_resolutions": [{"subject": subject_record(ref.subject), "invocation_id": ref.invocation_id,
                                 "evidence_sha256": ref.evidence_sha256}
                                for ref in record.closure_resolutions],
        "approval": None if approval is None else {
            "source_commit": approval.source_commit, "next_attempt": approval.next_attempt,
            "scope": scope_record(approval.scope), "platforms": platforms_record(approval.platforms)},
    }, STAGE)


@dataclass(frozen=True)
class ReviewedTriage:
    """A triage record bound to its validated provenance and closure resolutions."""

    record: TriageRecord
    provenance: ReviewProvenance
    resolutions: tuple[ClosureResolution, ...]

    def __post_init__(self) -> None:
        evidence = [item.evidence_sha256 for item in self.resolutions
                    if isinstance(item, ClosureResolution)]
        if (not isinstance(self.record, TriageRecord) or not isinstance(self.provenance, ReviewProvenance)
                or not self.provenance.binds(artifact_sha256=self.sha256, reviewer=self.record.reviewer)
                or len(evidence) != len(self.resolutions) or evidence != sorted(set(evidence))):
            raise VerificationError("candidate_triage_provenance_mismatch", STAGE)

    @property
    def sha256(self) -> str:
        return sha(encode_triage(self.record))


def _digest_tuple(value: object) -> tuple[str, ...]:
    return tuple(sha256_hex(item, STAGE)
                 for item in object_list(value, max_items=MAX_LIST_ITEMS, stage=STAGE))


def _parse_approval(value: object) -> ReplacementApproval | None:
    if value is None:
        return None
    fields = exact_fields(value, frozenset({"source_commit", "next_attempt", "scope", "platforms"}), STAGE)
    return ReplacementApproval(source_commit=fields["source_commit"], next_attempt=fields["next_attempt"],
                               scope=parse_scope(fields["scope"]),
                               platforms=parse_platforms(fields["platforms"]))


def _parse_finding(value: object) -> FindingRef:
    fields = exact_fields(value, frozenset({"code", "subject"}), STAGE)
    return FindingRef(code=fields["code"], subject=fields["subject"])


def _parse_closure_ref(value: object) -> ClosureResolutionRef:
    fields = exact_fields(value, frozenset({"subject", "invocation_id", "evidence_sha256"}), STAGE)
    return ClosureResolutionRef(subject=parse_subject(fields["subject"]), invocation_id=fields["invocation_id"],
                                evidence_sha256=fields["evidence_sha256"])


def parse_triage(data: bytes) -> TriageRecord:
    """Return the raw record only; review is established by ``ReviewedTriage``."""
    fields = load_kind(data, TRIAGE_KIND, frozenset({
        "candidate_id", "attempt", "prior_claim_sha256s", "prior_proof_sha256s", "explained_findings",
        "diagnosis_sha256s",
        "unresolved_product_findings", "reviewer", "closure_resolutions", "approval"}))
    findings = object_list(fields["explained_findings"], max_items=MAX_LIST_ITEMS, stage=STAGE)
    closures = object_list(fields["closure_resolutions"], max_items=MAX_LIST_ITEMS, stage=STAGE)
    return TriageRecord(
        candidate_id=fields["candidate_id"], attempt=fields["attempt"],
        prior_claim_sha256s=_digest_tuple(fields["prior_claim_sha256s"]),
        prior_proof_sha256s=_digest_tuple(fields["prior_proof_sha256s"]),
        explained_findings=tuple(_parse_finding(item) for item in findings),
        diagnosis_sha256s=_digest_tuple(fields["diagnosis_sha256s"]),
        unresolved_product_findings=identifier_tuple(
            fields["unresolved_product_findings"], max_items=MAX_LIST_ITEMS, stage=STAGE),
        reviewer=fields["reviewer"],
        closure_resolutions=tuple(_parse_closure_ref(item) for item in closures),
        approval=_parse_approval(fields["approval"]),
    )


PUBLICATION_STAGE = "validate-candidate-publication"


PUBLICATION_KIND = "delegation-layer-candidate-publication"


# The publication reader uses the shared compact-record bound; the largest
# valid record, with every inventory at ``MAX_LIST_ITEMS``, stays below it.
MAX_PUBLICATION_BYTES = MAX_RECORD_BYTES


def _digest_inventory(values: object, *, nonempty: bool) -> bool:
    """A bounded, strictly sorted, duplicate-free tuple of digests; never a caller's list."""
    return (isinstance(values, tuple) and len(values) <= MAX_LIST_ITEMS and (bool(values) or not nonempty)
            and _sorted_digests(values))


@dataclass(frozen=True)
class PublicationAuthorization:
    """The freeze record: the exact facts a ready verdict bound when the candidate was frozen.

    Only ``candidate_readiness.authorize_publication`` decides that these
    facts may be frozen; a valid or parsed record alone authorizes nothing.
    Each replacement selected exactly one triage, so an attempt binds one
    fewer triage than its number.
    """

    candidate_id: str
    attempt: int
    evidence_parent: str
    source_commit: str
    source_tree: str
    proof_sha256s: tuple[str, ...]
    install_sha256s: tuple[str, ...]
    triage_sha256s: tuple[str, ...]
    review_sha256: str

    def __post_init__(self) -> None:
        sha256_hex(self.candidate_id, PUBLICATION_STAGE)
        positive_int(self.attempt, PUBLICATION_STAGE)
        for value in (self.evidence_parent, self.source_commit, self.source_tree):
            revision(value, PUBLICATION_STAGE)
        sha256_hex(self.review_sha256, PUBLICATION_STAGE)
        if (not _digest_inventory(self.proof_sha256s, nonempty=True)
                or not _digest_inventory(self.install_sha256s, nonempty=True)
                or not _digest_inventory(self.triage_sha256s, nonempty=False)
                or len(self.triage_sha256s) != self.attempt - 1):
            raise VerificationError("candidate_publication_invalid", PUBLICATION_STAGE)

    @property
    def sha256(self) -> str:
        return sha(encode_publication(self))


def encode_publication(record: PublicationAuthorization) -> bytes:
    return encode_record({
        "schema_version": SCHEMA_VERSION, "kind": PUBLICATION_KIND,
        "candidate_id": record.candidate_id, "attempt": record.attempt,
        "evidence_parent": record.evidence_parent, "source_commit": record.source_commit,
        "source_tree": record.source_tree, "proof_sha256s": list(record.proof_sha256s),
        "install_sha256s": list(record.install_sha256s), "triage_sha256s": list(record.triage_sha256s),
        "review_sha256": record.review_sha256,
    }, PUBLICATION_STAGE)


def parse_publication(data: bytes) -> PublicationAuthorization:
    """Return the exact freeze record only; it neither authorizes publication nor proves review."""
    fields = load_kind(data, PUBLICATION_KIND, frozenset({
        "candidate_id", "attempt", "evidence_parent", "source_commit", "source_tree", "proof_sha256s",
        "install_sha256s", "triage_sha256s", "review_sha256"}))
    return PublicationAuthorization(
        candidate_id=fields["candidate_id"], attempt=fields["attempt"],
        evidence_parent=fields["evidence_parent"], source_commit=fields["source_commit"],
        source_tree=fields["source_tree"], proof_sha256s=_digest_tuple(fields["proof_sha256s"]),
        install_sha256s=_digest_tuple(fields["install_sha256s"]),
        triage_sha256s=_digest_tuple(fields["triage_sha256s"]), review_sha256=fields["review_sha256"],
    )


def require_triage_inventory(candidate_id: str, attempts: int, records: tuple[TriageRecord, ...]) -> None:
    """Every triage belongs to the candidate, explains one of its ``attempts`` attempts and is recorded once.

    The history checks its reviewed triages with this rule; store admission
    checks a proposed raw record with it before the record has provenance.
    Identity is the digest of the exact canonical record bytes.
    """
    if not isinstance(records, tuple) or not all(isinstance(record, TriageRecord) for record in records):
        raise VerificationError("candidate_history_invalid", STAGE)
    digests = [sha(encode_triage(record)) for record in records]
    if (any(record.candidate_id != candidate_id for record in records) or len(set(digests)) != len(digests)
            or any(not 0 < record.attempt <= attempts for record in records)):
        raise VerificationError("candidate_history_invalid", STAGE)


@dataclass(frozen=True)
class CandidateHistory:
    """Read model of one candidate's append-only development evidence.

    Each planned platform of an attempt has at most one claim, and every
    proof binds the claim of its platform and invocation. Install proofs
    belong to the candidate, not to an attempt: at most one per platform and
    channel, recorded only after success. Every triage stays in the history;
    a replacement selects its authority by exact digest.
    """

    candidate_id: str
    intents: tuple[AttemptIntent, ...]
    claims: tuple[PlatformClaim, ...]
    proofs: tuple[PlatformProof, ...]
    installs: tuple[InstallProof, ...]
    triages: tuple[ReviewedTriage, ...]
    publication: PublicationAuthorization | None

    def __post_init__(self) -> None:
        sha256_hex(self.candidate_id, STAGE)
        if (not all(isinstance(item, AttemptIntent) for item in self.intents)
                or not all(isinstance(item, PlatformClaim) for item in self.claims)
                or not all(isinstance(item, PlatformProof) for item in self.proofs)
                or not all(isinstance(item, InstallProof) for item in self.installs)
                or not all(isinstance(item, ReviewedTriage) for item in self.triages)
                or not (self.publication is None or isinstance(self.publication, PublicationAuthorization))):
            raise VerificationError("candidate_history_invalid", STAGE)
        if (tuple(intent.attempt for intent in self.intents) != tuple(range(1, len(self.intents) + 1))
                or not self._claims_valid() or not self._proofs_valid() or not self._installs_valid()):
            raise VerificationError("candidate_history_invalid", STAGE)
        attempts = len(self.intents)
        require_triage_inventory(self.candidate_id, attempts, tuple(triage.record for triage in self.triages))
        if (any(intent.candidate_id != self.candidate_id for intent in self.intents)
                or (self.publication is not None
                    and (self.publication.candidate_id != self.candidate_id
                         or not 0 < self.publication.attempt <= attempts))):
            raise VerificationError("candidate_history_invalid", STAGE)

    def _claims_valid(self) -> bool:
        keys = [(claim.attempt, claim.platform) for claim in self.claims]
        invocations = [claim.invocation_id for claim in self.claims]
        return (len(set(keys)) == len(keys) and len(set(invocations)) == len(invocations)
                and all(claim.candidate_id == self.candidate_id
                        and 0 < claim.attempt <= len(self.intents)
                        and claim.platform in self.intents[claim.attempt - 1].platforms
                        for claim in self.claims))

    def _proofs_valid(self) -> bool:
        keys = [(proof.attempt, proof.os, proof.arch) for proof in self.proofs]
        return len(set(keys)) == len(keys) and all(
            proof.candidate_id == self.candidate_id
            and (claim := self.claim_for(proof.attempt, (proof.os, proof.arch))) is not None
            and claim.invocation_id == proof.invocation_id
            for proof in self.proofs)

    def _installs_valid(self) -> bool:
        keys = [(install.os, install.arch, install.channel) for install in self.installs]
        return len(set(keys)) == len(keys) and all(
            install.candidate_id == self.candidate_id for install in self.installs)

    def install_for(self, platform: tuple[str, str], channel: InstallChannel) -> InstallProof | None:
        return next((install for install in self.installs
                     if install.platform == platform and install.channel is channel), None)

    def claims_for(self, attempt: int) -> tuple[PlatformClaim, ...]:
        return tuple(claim for claim in self.claims if claim.attempt == attempt)

    def claim_for(self, attempt: int, platform: tuple[str, str]) -> PlatformClaim | None:
        return next((claim for claim in self.claims_for(attempt) if claim.platform == platform), None)

    def proofs_for(self, attempt: int) -> tuple[PlatformProof, ...]:
        return tuple(proof for proof in self.proofs if proof.attempt == attempt)

    def triages_for(self, attempt: int) -> tuple[ReviewedTriage, ...]:
        return tuple(triage for triage in self.triages if triage.record.attempt == attempt)

    def selected_triage(self, attempt: int, sha256: str) -> ReviewedTriage | None:
        """Return the triage of ``attempt`` whose exact digest is ``sha256``."""
        return next((triage for triage in self.triages_for(attempt) if triage.sha256 == sha256), None)
