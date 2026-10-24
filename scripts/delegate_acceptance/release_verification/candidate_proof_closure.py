"""The one semantic reader of a retained closure supplement for a stored proof.

A native proof whose case closure stayed unresolved stays immutable. The
native observation owner may later read that invocation's retained private
state and record a supplement: the compact lifetime closure of each
unresolved case and the digests of the records it read. This module is the
only place that turns supplement bytes into a ``ClosureResolution``: it binds
the supplement to a proof the history actually holds, its original
invocation and native completion, exactly its unresolved cases and their
validated anchors, and applies the canonical resource closure predicate.
Store admission, strict replay and hosted replay all reach it through replay.

The native observation owner classifies each case's bound identities into
ordinary tasks and its model discovery inspection from the case's bound
failure diagnostics and the retained state; the reader checks that witness
against the anchor's exact inventory and the closure. Parsing alone grants
nothing. The reader checks bindings and canonical predicates; it does not
authenticate the unsigned private facts the witness and observation digests
name, so it never claims to exclude a consistent fabrication or an invisible
historical fault. A resolved resource closure is not a PASS: the case keeps
its verdict.
"""

from __future__ import annotations

from dataclasses import dataclass

from ..evidence.digests import sha
from ..native_evidence.lifetime_closure import resource_closure_error
from ..native_evidence.public_contract import TASK_ID, CaseClosureAnchor, ValidatedCandidateProof
from .candidate_attempts import MAX_LIST_ITEMS, MAX_RECORD_BYTES, SCHEMA_VERSION
from .candidate_closure import ClosureResolution, ProofSubject, ResourceDisposition, ResourceResolution
from .candidate_codec import (
    OPAQUE_ID_RE, encode_record, exact_fields, load_record, object_list, positive_int, sha256_hex, text,
)
from .contract import VerificationError


STAGE = "validate-candidate-proof-closure"


PROOF_CLOSURE_KIND = "delegation-layer-candidate-proof-closure-supplement"


@dataclass(frozen=True)
class RetainedObservation:
    """Digests of the retained private records the native observation read.

    ``coordinator_receipt_sha256`` names the original coordinator receipt,
    ``directory_identity_sha256`` its recorded directory identities and pins,
    and ``capture_records_sha256`` the ``capture_count`` capture invocation,
    start and Wait-completion records. A hosted reader can check only their
    shape; the native observation owner verified the records themselves.
    """

    coordinator_receipt_sha256: str
    directory_identity_sha256: str
    capture_records_sha256: str
    capture_count: int

    def __post_init__(self) -> None:
        for digest in (self.coordinator_receipt_sha256, self.directory_identity_sha256,
                       self.capture_records_sha256):
            sha256_hex(digest, STAGE)
        positive_int(self.capture_count, STAGE)


@dataclass(frozen=True)
class OrdinaryTask:
    """One known ordinary task and the digests of its spec and meta the diagnostics recorded."""

    task_id: str
    spec_sha256: str
    meta_sha256: str

    def __post_init__(self) -> None:
        text(self.task_id, TASK_ID, STAGE)
        sha256_hex(self.spec_sha256, STAGE)
        sha256_hex(self.meta_sha256, STAGE)


@dataclass(frozen=True)
class CaseClosure:
    """The observed resource closure of one unresolved case.

    The four digests are the case's private records the anchor binds.
    ``ordinary_tasks`` and ``discovery_inspection_id`` classify every bound
    identity. ``lifetime_closure`` is the canonical JSON value the
    managed-lifetime root projection produced; only the canonical predicate
    judges it.
    """

    case_id: str
    source_compact_sha256: str
    source_receipt_sha256: str
    cleanup_receipt_sha256: str
    failure_diagnostics_sha256: str
    ordinary_tasks: tuple[OrdinaryTask, ...]
    discovery_inspection_id: str | None
    lifetime_closure: object

    def __post_init__(self) -> None:
        text(self.case_id, OPAQUE_ID_RE, STAGE)
        for digest in (self.source_compact_sha256, self.source_receipt_sha256, self.cleanup_receipt_sha256,
                       self.failure_diagnostics_sha256):
            sha256_hex(digest, STAGE)
        if self.discovery_inspection_id is not None:
            text(self.discovery_inspection_id, TASK_ID, STAGE)
        ids = [task.task_id for task in self.ordinary_tasks if isinstance(task, OrdinaryTask)]
        if (not isinstance(self.ordinary_tasks, tuple) or len(ids) != len(self.ordinary_tasks)
                or ids != sorted(set(ids)) or self.discovery_inspection_id in ids):
            raise VerificationError("candidate_proof_closure_invalid", STAGE)

    @property
    def ordinary_task_ids(self) -> tuple[str, ...]:
        return tuple(task.task_id for task in self.ordinary_tasks)


@dataclass(frozen=True)
class ProofClosure:
    """A closure supplement for every unresolved case of one stored proof."""

    proof_sha256: str
    invocation_id: str
    started_sha256: str
    invocation_sha256: str
    completed_sha256: str
    observation: RetainedObservation
    cases: tuple[CaseClosure, ...]

    def __post_init__(self) -> None:
        for digest in (self.proof_sha256, self.started_sha256, self.invocation_sha256, self.completed_sha256):
            sha256_hex(digest, STAGE)
        text(self.invocation_id, OPAQUE_ID_RE, STAGE)
        case_ids = [case.case_id for case in self.cases if isinstance(case, CaseClosure)]
        if (not isinstance(self.observation, RetainedObservation) or not isinstance(self.cases, tuple)
                or not self.cases or len(self.cases) > MAX_LIST_ITEMS or len(case_ids) != len(self.cases)
                or case_ids != sorted(set(case_ids))):
            raise VerificationError("candidate_proof_closure_invalid", STAGE)


def encode_proof_closure(closure: ProofClosure) -> bytes:
    """Return the only accepted bytes of ``closure``; the encoder refuses what the reader refuses."""
    observation = closure.observation
    data = encode_record({
        "schema_version": SCHEMA_VERSION, "kind": PROOF_CLOSURE_KIND,
        "proof_sha256": closure.proof_sha256, "invocation_id": closure.invocation_id,
        "native_completion": {"started_sha256": closure.started_sha256,
                              "invocation_sha256": closure.invocation_sha256,
                              "completed_sha256": closure.completed_sha256},
        "observation": {"coordinator_receipt_sha256": observation.coordinator_receipt_sha256,
                        "directory_identity_sha256": observation.directory_identity_sha256,
                        "capture_records_sha256": observation.capture_records_sha256,
                        "capture_count": observation.capture_count},
        "cases": [_case_record(case) for case in closure.cases],
    }, STAGE)
    if len(data) > MAX_RECORD_BYTES:
        raise VerificationError("candidate_record_too_large", STAGE)
    return data


def _case_record(case: CaseClosure) -> dict[str, object]:
    return {
        "case_id": case.case_id, "source_compact_sha256": case.source_compact_sha256,
        "source_receipt_sha256": case.source_receipt_sha256, "cleanup_receipt_sha256": case.cleanup_receipt_sha256,
        "failure_diagnostics_sha256": case.failure_diagnostics_sha256,
        "ordinary_tasks": [{"task_id": task.task_id, "spec_sha256": task.spec_sha256,
                            "meta_sha256": task.meta_sha256} for task in case.ordinary_tasks],
        "discovery_inspection_id": case.discovery_inspection_id, "lifetime_closure": case.lifetime_closure,
    }


_CASE_FIELDS = frozenset({
    "case_id", "source_compact_sha256", "source_receipt_sha256", "cleanup_receipt_sha256",
    "failure_diagnostics_sha256", "ordinary_tasks", "discovery_inspection_id", "lifetime_closure"})


def _parse_case(value: object) -> CaseClosure:
    case = exact_fields(value, _CASE_FIELDS, STAGE)
    tasks = []
    for item in object_list(case["ordinary_tasks"], max_items=MAX_LIST_ITEMS, stage=STAGE):
        task = exact_fields(item, frozenset({"task_id", "spec_sha256", "meta_sha256"}), STAGE)
        tasks.append(OrdinaryTask(task_id=task["task_id"], spec_sha256=task["spec_sha256"],
                                  meta_sha256=task["meta_sha256"]))
    return CaseClosure(
        case_id=case["case_id"], source_compact_sha256=case["source_compact_sha256"],
        source_receipt_sha256=case["source_receipt_sha256"], cleanup_receipt_sha256=case["cleanup_receipt_sha256"],
        failure_diagnostics_sha256=case["failure_diagnostics_sha256"], ordinary_tasks=tuple(tasks),
        discovery_inspection_id=case["discovery_inspection_id"], lifetime_closure=case["lifetime_closure"])


_TOP_FIELDS = frozenset({"schema_version", "kind", "proof_sha256", "invocation_id", "native_completion",
                         "observation", "cases"})


def is_proof_closure(data: bytes) -> bool:
    """Whether ``data`` declares the supplement kind; it says nothing about its validity."""
    try:
        value = load_record(data, max_bytes=MAX_RECORD_BYTES, stage=STAGE)
    except VerificationError:
        return False
    return value.get("kind") == PROOF_CLOSURE_KIND


def parse_proof_closure(data: bytes) -> ProofClosure:
    """Return the exact record ``encode_proof_closure`` wrote; this grants no authority."""
    value = load_record(data, max_bytes=MAX_RECORD_BYTES, stage=STAGE)
    fields = exact_fields(value, _TOP_FIELDS, STAGE)
    version = fields["schema_version"]
    if type(version) is not int or version != SCHEMA_VERSION or fields["kind"] != PROOF_CLOSURE_KIND:
        raise VerificationError("candidate_record_kind_invalid", STAGE)
    completion = exact_fields(fields["native_completion"],
                              frozenset({"started_sha256", "invocation_sha256", "completed_sha256"}), STAGE)
    observed = exact_fields(fields["observation"], frozenset({
        "coordinator_receipt_sha256", "directory_identity_sha256", "capture_records_sha256",
        "capture_count"}), STAGE)
    cases = [_parse_case(item) for item in object_list(fields["cases"], max_items=MAX_LIST_ITEMS, stage=STAGE)]
    closure = ProofClosure(
        proof_sha256=fields["proof_sha256"], invocation_id=fields["invocation_id"],
        started_sha256=completion["started_sha256"], invocation_sha256=completion["invocation_sha256"],
        completed_sha256=completion["completed_sha256"],
        observation=RetainedObservation(
            coordinator_receipt_sha256=observed["coordinator_receipt_sha256"],
            directory_identity_sha256=observed["directory_identity_sha256"],
            capture_records_sha256=observed["capture_records_sha256"], capture_count=observed["capture_count"]),
        cases=tuple(cases))
    if encode_proof_closure(closure) != data:
        raise VerificationError("candidate_record_not_canonical", STAGE)
    return closure


def _closed_task_digests(closure: object, task_id: str) -> tuple[object, object]:
    """The spec and meta digests a validated closure recorded for one ordinary task."""
    ordinary = closure.get("ordinary") if isinstance(closure, dict) else None
    tasks = ordinary.get("tasks") if isinstance(ordinary, dict) else None
    fact = tasks.get(task_id) if isinstance(tasks, dict) else None
    return (fact.get("spec_sha256"), fact.get("meta_sha256")) if isinstance(fact, dict) else (None, None)


def _case_error(case: CaseClosure, anchor: CaseClosureAnchor) -> str | None:
    """Bind one case's witness to its anchor, then apply the canonical resource closure predicate."""
    if ((case.source_compact_sha256, case.source_receipt_sha256, case.cleanup_receipt_sha256,
         case.failure_diagnostics_sha256)
            != (anchor.source_compact_sha256, anchor.source_receipt_sha256, anchor.cleanup_receipt_sha256,
                anchor.failure_diagnostics_sha256)):
        return "candidate_proof_closure_case_mismatch"
    ordinary = frozenset(case.ordinary_task_ids)
    inspections = frozenset(() if case.discovery_inspection_id is None else (case.discovery_inspection_id,))
    session_bound = frozenset((*anchor.task_ids, *(task for _, task in anchor.task_session_bindings)))
    if ordinary | inspections != anchor.bound_task_ids or not session_bound <= ordinary:
        return "candidate_proof_closure_inventory_mismatch"
    error = resource_closure_error(case.lifetime_closure, task_ids=case.ordinary_task_ids,
                                   discovery_inspection_id=case.discovery_inspection_id)
    if error is not None:
        return f"candidate_proof_closure_{error}"
    if any(_closed_task_digests(case.lifetime_closure, task.task_id) != (task.spec_sha256, task.meta_sha256)
           for task in case.ordinary_tasks):
        return "candidate_proof_closure_task_digest_mismatch"
    return None


def _require_bound(closure: ProofClosure, proof: ValidatedCandidateProof) -> None:
    completion = proof.completion
    if (closure.invocation_id != proof.invocation_id
            or (closure.started_sha256, closure.invocation_sha256, closure.completed_sha256)
            != (completion.started_sha256, completion.invocation_sha256, completion.completed_sha256)):
        raise VerificationError("candidate_proof_closure_invocation_mismatch", STAGE)
    if tuple(case.case_id for case in closure.cases) != proof.unresolved_case_ids:
        raise VerificationError("candidate_proof_closure_cases_mismatch", STAGE)
    for case, anchor in zip(closure.cases, proof.closure_anchors, strict=True):
        error = _case_error(case, anchor)
        if error is not None:
            raise VerificationError(error, STAGE)


def read_proof_closure(data: bytes, proofs: tuple[ValidatedCandidateProof, ...]) -> ClosureResolution:
    """Return the closure resolution ``data`` establishes for one proof in ``proofs``, or refuse.

    ``proofs`` are the validated proofs of the history before the supplement.
    Each unresolved case resolves as waited by this supplement, so the
    resolution's evidence digest binds the exact stored bytes.
    """
    closure = parse_proof_closure(data)
    proof = next((item for item in proofs if item.public_zip_sha256 == closure.proof_sha256), None)
    if proof is None:
        raise VerificationError("candidate_proof_closure_proof_unknown", STAGE)
    _require_bound(closure, proof)
    supplement = sha(data)
    return ClosureResolution(
        subject=ProofSubject(proof_sha256=proof.public_zip_sha256), invocation_id=proof.invocation_id,
        resources=tuple(ResourceResolution(resource_id=case.case_id, disposition=ResourceDisposition.WAITED,
                                           fact_sha256=supplement) for case in closure.cases))
