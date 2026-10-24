"""Closure identity and evidence for claimed platforms and their proofs.

A closure subject is either a platform claim that never produced a proof or
a proof whose lifetime closure stayed unresolved. The existing native
lifetime and receipt owners produce ``ClosureResolution`` facts after they
validate real evidence; this module only fixes the exact identity, the
resource inventory shape and the canonical evidence digest that a reviewed
triage references. A missing or empty observation is never a resolution.
Parsing evidence bytes recovers the exact record only; it never establishes
that the native owner produced it or that its inventory is complete.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum

from ..evidence.digests import sha
from .candidate_attempts import (
    MAX_LIST_ITEMS, MAX_RECORD_BYTES, SCHEMA_VERSION, load_kind, sorted_opaque_ids,
)
from .candidate_codec import OPAQUE_ID_RE, encode_record, exact_fields, object_list, sha256_hex, text
from .contract import VerificationError


STAGE = "validate-candidate-closure"


CLOSURE_EVIDENCE_KIND = "delegation-layer-candidate-closure-evidence"


# The native acceptance child of a claimed invocation; it is the only launcher
# of that invocation's cases, so every claim closure must resolve it first.
NATIVE_CHILD_RESOURCE = "native-child"


@dataclass(frozen=True)
class ClaimSubject:
    """A claimed platform invocation that produced no proof."""

    claim_sha256: str

    def __post_init__(self) -> None:
        sha256_hex(self.claim_sha256, STAGE)


@dataclass(frozen=True)
class ProofSubject:
    """A proof whose native lifetime closure stayed unresolved."""

    proof_sha256: str

    def __post_init__(self) -> None:
        sha256_hex(self.proof_sha256, STAGE)


ClosureSubject = ClaimSubject | ProofSubject


def subject_key(subject: ClosureSubject) -> tuple[str, str]:
    """Return a total-order key: the subject kind and its digest."""
    if isinstance(subject, ClaimSubject):
        return "claim", subject.claim_sha256
    if isinstance(subject, ProofSubject):
        return "proof", subject.proof_sha256
    raise VerificationError("candidate_closure_subject_invalid", STAGE)


def subject_record(subject: ClosureSubject) -> dict[str, str]:
    kind, digest = subject_key(subject)
    return {f"{kind}_sha256": digest}


def parse_subject(value: object) -> ClosureSubject:
    if isinstance(value, dict) and set(value) == {"claim_sha256"}:
        return ClaimSubject(claim_sha256=value["claim_sha256"])
    if isinstance(value, dict) and set(value) == {"proof_sha256"}:
        return ProofSubject(proof_sha256=value["proof_sha256"])
    raise VerificationError("candidate_closure_subject_invalid", STAGE)


class ResourceDisposition(Enum):
    """How the original invocation's owner positively resolved one resource."""

    WAITED = "waited"
    NOT_STARTED = "not-started"


@dataclass(frozen=True)
class ResourceResolution:
    """One owned resource and the digest of the owner fact that resolved it."""

    resource_id: str
    disposition: ResourceDisposition
    fact_sha256: str

    def __post_init__(self) -> None:
        text(self.resource_id, OPAQUE_ID_RE, STAGE)
        sha256_hex(self.fact_sha256, STAGE)
        if not isinstance(self.disposition, ResourceDisposition):
            raise VerificationError("candidate_resolution_invalid", STAGE)


def _claim_inventory_valid(resources: tuple[ResourceResolution, ...]) -> bool:
    """A claim closure resolves the native child; if it never started, nothing else exists."""
    child = next((item for item in resources if item.resource_id == NATIVE_CHILD_RESOURCE), None)
    if child is None:
        return False
    return child.disposition is ResourceDisposition.WAITED or len(resources) == 1


@dataclass(frozen=True)
class ClosureResolution:
    """A positive closure fact for the exact resource inventory of one subject."""

    subject: ClosureSubject
    invocation_id: str
    resources: tuple[ResourceResolution, ...]

    def __post_init__(self) -> None:
        subject_key(self.subject)
        text(self.invocation_id, OPAQUE_ID_RE, STAGE)
        if (not isinstance(self.resources, tuple) or not self.resources
                or len(self.resources) > MAX_LIST_ITEMS
                or not all(isinstance(item, ResourceResolution) for item in self.resources)
                or not sorted_opaque_ids(self.resource_ids)
                or (isinstance(self.subject, ClaimSubject) and not _claim_inventory_valid(self.resources))):
            raise VerificationError("candidate_resolution_invalid", STAGE)

    @property
    def resource_ids(self) -> tuple[str, ...]:
        return tuple(item.resource_id for item in self.resources)

    @property
    def evidence_sha256(self) -> str:
        return sha(encode_closure_evidence(self))


def encode_closure_evidence(resolution: ClosureResolution) -> bytes:
    """Return the canonical evidence bytes whose digest a triage references.

    The encoder refuses exactly the records the bounded reader would refuse.
    """
    data = encode_record({
        "schema_version": SCHEMA_VERSION, "kind": CLOSURE_EVIDENCE_KIND,
        "subject": subject_record(resolution.subject), "invocation_id": resolution.invocation_id,
        "resources": [{"resource_id": item.resource_id, "disposition": item.disposition.value,
                       "fact_sha256": item.fact_sha256} for item in resolution.resources],
    }, STAGE)
    if len(data) > MAX_RECORD_BYTES:
        raise VerificationError("candidate_record_too_large", STAGE)
    return data


_RESOURCE_FIELDS = frozenset({"resource_id", "disposition", "fact_sha256"})


def _parse_resource(value: object) -> ResourceResolution:
    fields = exact_fields(value, _RESOURCE_FIELDS, STAGE)
    try:
        disposition = ResourceDisposition(fields["disposition"])
    except ValueError:
        raise VerificationError("candidate_resolution_invalid", STAGE) from None
    return ResourceResolution(resource_id=fields["resource_id"], disposition=disposition,
                              fact_sha256=fields["fact_sha256"])


def parse_closure_evidence(data: bytes) -> ClosureResolution:
    """Return the exact record ``encode_closure_evidence`` wrote; this grants no authority.

    A valid record proves neither that the native lifetime owner produced it
    nor that its inventory is complete. The reader must first bind the bytes
    to a trusted producer, then compare the inventory with its own obligation.
    """
    fields = load_kind(data, CLOSURE_EVIDENCE_KIND, frozenset({"subject", "invocation_id", "resources"}))
    resources = object_list(fields["resources"], max_items=MAX_LIST_ITEMS, stage=STAGE)
    return ClosureResolution(subject=parse_subject(fields["subject"]), invocation_id=fields["invocation_id"],
                             resources=tuple(_parse_resource(item) for item in resources))


@dataclass(frozen=True)
class ClosureResolutionRef:
    """A triage's reference to the closure evidence of one subject."""

    subject: ClosureSubject
    invocation_id: str
    evidence_sha256: str

    def __post_init__(self) -> None:
        subject_key(self.subject)
        text(self.invocation_id, OPAQUE_ID_RE, STAGE)
        sha256_hex(self.evidence_sha256, STAGE)

    @property
    def key(self) -> tuple[str, str]:
        return subject_key(self.subject)
