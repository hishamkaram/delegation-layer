"""Pure semantic replay of one candidate's structurally verified store history.

``replay_candidate`` decodes every stored record with its canonical codec
owner and applies each recorded transition, in its actual order, through the
canonical candidate policy: the launch and claim decisions on the history
before the transition, the native candidate proof validator, the install
reader and the publication authorization. It copies none of their rules.

``admit_append`` runs the same admission on a proposed group appended at the
verified head, so an append the store acknowledges is one replay admits. One
private fold serves both: admission decodes and decides a group, and only an
actually observed transaction then attaches its introducing commit's
provenance to a reviewed artifact.

The keyword inputs are trusted facts that only their named owners may
supply: the store I/O edge, after loading the verified store branch, names
the introducing transaction of each reviewed artifact and reads the exact
installer source bytes at the candidate's source commit; the native lifetime
owner produces the closure resolutions. A reviewer is an unauthenticated
label that provenance binds, not an account. Replay never builds these facts
from labels, authors or parsed records, and it refuses a missing, foreign or
unused fact. The one exception is a stored proof closure supplement: its
semantic reader derives the resolution from the supplement bytes and the
validated proof this history already holds, so every caller replays it the
same way without a trusted fact.

A snapshot is a set of facts. It grants no launch, readiness or publication
authority; a fresh readiness verdict still needs a current source
observation from its caller. Its freeze commit is provenance only: which
observed commit introduced the frozen record, never an approval of it.
"""

from __future__ import annotations

from dataclasses import dataclass, replace

from ..evidence.digests import sha
from ..native_evidence.candidate_proof import validate_candidate_proof
from ..native_evidence.public_contract import PublicProofError, ValidatedCandidateProof
from .candidate_attempts import (
    ReviewedSource, ReviewProvenance, SourceReview, candidate_proof_expectation, encode_claim, encode_intent,
    encode_review, parse_claim, parse_intent, parse_review, platform_proof,
)
from .candidate_closure import ClosureResolution, encode_closure_evidence
from .candidate_history import (
    CandidateHistory, FindingRef, ReviewedTriage, TriageRecord, encode_publication, encode_triage,
    parse_publication, parse_triage, require_triage_inventory,
)
from .candidate_install_contract import (
    SourceFileBinding, encode_install_proof, parse_install_proof, read_install_proof,
)
from .candidate_manifest import bind_manifest, encode_locator, encode_manifest, parse_locator
from .candidate_proof_closure import is_proof_closure, read_proof_closure
from .candidate_readiness import (
    BoundCandidate, LaunchDecision, SourceObservation, authorize_publication, decide_launch,
    decide_platform_claim,
)
from .candidate_store_contract import (
    GIT_OBJECT_RE, CandidateHistoryAtHead, EntryFamily, EntryName, RawCandidateSnapshot, RawTransaction,
    StoredBlob, TransitionKind, candidate_commit_count_valid, classify_transition, parse_entry_name,
)
from .contract import VerificationError


STAGE = "replay-candidate-store"


# The closed set of verified histories replay reads. Only the boundary-observed
# snapshot may be admitted or written on; replay never reads the boundary.
_History = RawCandidateSnapshot | CandidateHistoryAtHead


class ReplayDenied(VerificationError):
    """The canonical policy refused a recorded transition; ``findings`` are its exact findings."""

    def __init__(self, code: str, findings: tuple[FindingRef, ...]):
        super().__init__(code, STAGE)
        self.findings = findings


@dataclass(frozen=True)
class CandidateSnapshot:
    """The decoded facts of one registered candidate at store head ``head``.

    ``reviews`` holds every source review whose introducing transaction the
    trusted I/O edge verified, in store order. ``freeze_commit`` is the
    observed commit that introduced ``history.publication``, and None exactly
    when the candidate is not frozen; ``head`` stays the store head and the
    record's ``evidence_parent`` stays that commit's parent. The snapshot
    holds no launch, readiness or publication decision.
    """

    head: str
    candidate: BoundCandidate
    history: CandidateHistory
    reviews: tuple[ReviewedSource, ...]
    freeze_commit: str | None

    def __post_init__(self) -> None:
        """A freeze commit is a Git commit identity exactly when the history holds a publication."""
        unfrozen = self.freeze_commit is None and self.history.publication is None
        frozen = (self.history.publication is not None and isinstance(self.freeze_commit, str)
                  and GIT_OBJECT_RE.fullmatch(self.freeze_commit) is not None)
        if not (unfrozen or frozen):
            raise VerificationError("store_snapshot_invalid", STAGE)


@dataclass(frozen=True)
class _TrustedFacts:
    """The caller's trusted facts, each indexed by the one identity that selects it."""

    provenance: dict[str, ReviewProvenance]
    closures: dict[str, ClosureResolution]
    installer_sources: dict[SourceFileBinding, bytes]


def _unique_index(keys: list[object], code: str) -> None:
    if len(set(keys)) != len(keys):
        raise VerificationError(code, STAGE)


def _trusted_facts(review_provenance: object, closure_resolutions: object,
                   installer_sources: object) -> _TrustedFacts:
    """Index the trusted inputs; a malformed or ambiguous input refuses before any record is read."""
    if (not isinstance(review_provenance, tuple)
            or not all(isinstance(item, ReviewProvenance) for item in review_provenance)):
        raise VerificationError("store_review_provenance_invalid", STAGE)
    if (not isinstance(closure_resolutions, tuple)
            or not all(isinstance(item, ClosureResolution) for item in closure_resolutions)):
        raise VerificationError("store_closure_resolution_invalid", STAGE)
    if (not isinstance(installer_sources, tuple)
            or not all(isinstance(item, tuple) and len(item) == 2 and isinstance(item[0], SourceFileBinding)
                       and isinstance(item[1], bytes) and sha(item[1]) == item[0].sha256
                       for item in installer_sources)):
        raise VerificationError("store_installer_source_invalid", STAGE)
    _unique_index([item.artifact_sha256 for item in review_provenance], "store_review_provenance_invalid")
    _unique_index([item.evidence_sha256 for item in closure_resolutions], "store_closure_resolution_invalid")
    _unique_index([binding for binding, _ in installer_sources], "store_installer_source_invalid")
    return _TrustedFacts(provenance={item.artifact_sha256: item for item in review_provenance},
                         closures={item.evidence_sha256: item for item in closure_resolutions},
                         installer_sources=dict(installer_sources))


@dataclass(frozen=True)
class _Replay:
    """The facts replayed so far; each transition returns a new value.

    ``closures`` are the trusted closure facts consumed so far and
    ``derived_closures`` the resolutions read from stored supplements;
    ``proofs`` are the validated proofs the supplements bind to.
    """

    candidate: BoundCandidate
    history: CandidateHistory
    reviews: tuple[ReviewedSource, ...]
    closures: tuple[ClosureResolution, ...]
    installer_bindings: tuple[SourceFileBinding, ...]
    derived_closures: tuple[ClosureResolution, ...] = ()
    proofs: tuple[ValidatedCandidateProof, ...] = ()


def _require_exact(data: bytes, encoded: bytes) -> None:
    """The stored bytes are the canonical encoding of the value decoded from them."""
    if encoded != data:
        raise VerificationError("store_record_not_canonical", STAGE)


_Blobs = tuple[StoredBlob, ...]


def _named(blobs: _Blobs, family: EntryFamily) -> list[tuple[EntryName, StoredBlob]]:
    return [(name, blob) for blob in blobs if (name := parse_entry_name(blob.entry.name)).family is family]


def _single(blobs: _Blobs, family: EntryFamily) -> tuple[EntryName, StoredBlob]:
    found = _named(blobs, family)
    if len(found) != 1:
        raise VerificationError("store_transition_invalid", STAGE)
    return found[0]


def _require_digest_name(name: EntryName, blob: StoredBlob) -> None:
    """A digest-named record's file name is the SHA-256 of its exact bytes."""
    if name.digest != blob.sha256:
        raise VerificationError("store_entry_digest_mismatch", STAGE)


def _require_candidate(state: _Replay, candidate_id: str) -> None:
    if candidate_id != state.candidate.candidate_id:
        raise VerificationError("store_record_candidate_mismatch", STAGE)


def _require_allowed(decision: LaunchDecision, code: str) -> None:
    if not decision.allowed:
        raise ReplayDenied(code, decision.findings)


@dataclass(frozen=True)
class _Admitted:
    """The decoded facts one admitted group adds; it lives within one call and grants nothing.

    ``history`` is the next history, except that a reviewed ``artifact``
    (a source review or a triage) is not in it yet: only an actual introducing
    transaction can supply its provenance. ``resolutions`` are a triage's
    stored closure facts; ``closures`` and ``installer_binding`` are the
    trusted facts this group consumed. ``derived_closures`` are the
    resolutions its supplements established and ``proof`` its validated proof.
    """

    candidate: BoundCandidate
    history: CandidateHistory
    artifact: SourceReview | TriageRecord | None = None
    resolutions: tuple[ClosureResolution, ...] = ()
    closures: tuple[ClosureResolution, ...] = ()
    installer_binding: SourceFileBinding | None = None
    derived_closures: tuple[ClosureResolution, ...] = ()
    proof: ValidatedCandidateProof | None = None


def _register(blobs: _Blobs, raw: _History) -> _Admitted:
    _, locator_blob = _single(blobs, EntryFamily.LOCATOR)
    _, manifest_blob = _single(blobs, EntryFamily.MANIFEST)
    locator = parse_locator(locator_blob.data)
    _require_exact(locator_blob.data, encode_locator(locator))
    manifest = bind_manifest(locator, manifest_blob.data)
    _require_exact(manifest_blob.data, encode_manifest(manifest))
    if locator.candidate_id != raw.candidate_id:
        raise VerificationError("store_candidate_path_mismatch", STAGE)
    if locator.repository != raw.location.repository:
        raise VerificationError("store_candidate_repository_mismatch", STAGE)
    candidate = BoundCandidate(locator=locator, manifest=manifest)
    return _Admitted(candidate=candidate, history=CandidateHistory(
        candidate_id=candidate.candidate_id, intents=(), claims=(), proofs=(), installs=(), triages=(),
        publication=None))


def _intent(state: _Replay, blobs: _Blobs) -> _Admitted:
    name, blob = _single(blobs, EntryFamily.INTENT)
    intent = parse_intent(blob.data)
    _require_exact(blob.data, encode_intent(intent))
    if name.attempt != intent.attempt:
        raise VerificationError("store_entry_body_mismatch", STAGE)
    _require_allowed(decide_launch(candidate=state.candidate, history=state.history, intent=intent),
                     "store_launch_denied")
    return _Admitted(candidate=state.candidate,
                     history=replace(state.history, intents=state.history.intents + (intent,)))


def _claim(state: _Replay, blobs: _Blobs) -> _Admitted:
    name, blob = _single(blobs, EntryFamily.CLAIM)
    claim = parse_claim(blob.data)
    _require_exact(blob.data, encode_claim(claim))
    if (name.attempt, name.platform) != (claim.attempt, claim.platform):
        raise VerificationError("store_entry_body_mismatch", STAGE)
    _require_allowed(decide_platform_claim(candidate=state.candidate, history=state.history, claim=claim),
                     "store_claim_denied")
    return _Admitted(candidate=state.candidate,
                     history=replace(state.history, claims=state.history.claims + (claim,)))


def _proof(state: _Replay, blobs: _Blobs) -> _Admitted:
    """Validate the stored ZIP against the expectation of its claim; a valid FAIL proof is a fact too."""
    name, blob = _single(blobs, EntryFamily.PROOF)
    if name.attempt is None or name.platform is None:
        raise VerificationError("store_transition_invalid", STAGE)
    claim = state.history.claim_for(name.attempt, name.platform)
    if claim is None:
        raise VerificationError("store_proof_unclaimed", STAGE)
    candidate = state.candidate
    expectation = candidate_proof_expectation(candidate.manifest, candidate.locator, claim)
    try:
        validated = validate_candidate_proof(blob.data, expectation)
    except PublicProofError as error:
        raise VerificationError("store_proof_invalid", STAGE) from error
    proof = platform_proof(validated, claim)
    return _Admitted(candidate=candidate, history=replace(state.history, proofs=state.history.proofs + (proof,)),
                     proof=validated)


def _install(state: _Replay, blobs: _Blobs, facts: _TrustedFacts) -> _Admitted:
    """Read the paired proof and witness with the canonical reader and the trusted installer bytes."""
    name, proof_blob = _single(blobs, EntryFamily.INSTALL_PROOF)
    _, witness_blob = _single(blobs, EntryFamily.INSTALL_WITNESS)
    binding = parse_install_proof(proof_blob.data).installer
    source = None
    if isinstance(binding, SourceFileBinding):
        source = facts.installer_sources.get(binding)
        if source is None:
            raise VerificationError("store_installer_source_missing", STAGE)
    install = read_install_proof(proof_blob.data, witness_blob.data, manifest=state.candidate.manifest,
                                 installer_source=source, repository=state.candidate.locator.repository)
    _require_exact(proof_blob.data, encode_install_proof(install))
    if (name.platform, name.channel) != (install.platform, install.channel):
        raise VerificationError("store_entry_body_mismatch", STAGE)
    return _Admitted(candidate=state.candidate,
                     history=replace(state.history, installs=state.history.installs + (install,)),
                     installer_binding=binding if isinstance(binding, SourceFileBinding) else None)


def _review(state: _Replay, blobs: _Blobs) -> _Admitted:
    name, blob = _single(blobs, EntryFamily.REVIEW)
    _require_digest_name(name, blob)
    record = parse_review(blob.data)
    _require_exact(blob.data, encode_review(record))
    _require_candidate(state, record.candidate_id)
    return _Admitted(candidate=state.candidate, history=state.history, artifact=record)


def _added_closures(state: _Replay, blobs: _Blobs,
                    facts: _TrustedFacts) -> tuple[tuple[ClosureResolution, ...], tuple[ClosureResolution, ...]]:
    """Return the trusted and the supplement-derived resolutions of each added closure file.

    A supplement resolves only through its semantic reader on this history's
    validated proofs; any other closure file needs the native owner's fact
    with exactly its canonical bytes.
    """
    trusted, derived = [], []
    for name, blob in _named(blobs, EntryFamily.CLOSURE):
        _require_digest_name(name, blob)
        if is_proof_closure(blob.data):
            derived.append(read_proof_closure(blob.data, state.proofs))
            continue
        fact = facts.closures.get(blob.sha256)
        if fact is None or encode_closure_evidence(fact) != blob.data:
            raise VerificationError("store_closure_unverified", STAGE)
        trusted.append(fact)
    return tuple(trusted), tuple(derived)


def _triage(state: _Replay, blobs: _Blobs, facts: _TrustedFacts) -> _Admitted:
    """Bind a triage to the stored closure facts it references, earlier ones included.

    A reference without a stored fact stays unresolved; the replacement
    policy reports it when a later intent relies on the triage.
    """
    name, blob = _single(blobs, EntryFamily.TRIAGE)
    _require_digest_name(name, blob)
    record = parse_triage(blob.data)
    _require_exact(blob.data, encode_triage(record))
    _require_candidate(state, record.candidate_id)
    history = state.history
    require_triage_inventory(state.candidate.candidate_id, len(history.intents),
                             tuple(triage.record for triage in history.triages) + (record,))
    added, derived = _added_closures(state, blobs, facts)
    referenced = {ref.evidence_sha256 for ref in record.closure_resolutions}
    if not {fact.evidence_sha256 for fact in added + derived} <= referenced:
        raise VerificationError("store_closure_unreferenced", STAGE)
    known = state.closures + state.derived_closures + added + derived
    resolutions = tuple(sorted((fact for fact in known if fact.evidence_sha256 in referenced),
                               key=lambda fact: fact.evidence_sha256))
    return _Admitted(candidate=state.candidate, history=history, artifact=record, resolutions=resolutions,
                     closures=added, derived_closures=derived)


def _freeze(state: _Replay, blobs: _Blobs, *, evidence_parent: str) -> _Admitted:
    """Recompute the freeze record on the history before it and require the stored record exactly.

    ``evidence_parent`` is the commit the freeze appends to. The source
    target is the candidate's recorded build source, not a fresh observation;
    this proves what was frozen, never current readiness.
    """
    _, blob = _single(blobs, EntryFamily.PUBLICATION)
    record = parse_publication(blob.data)
    _require_exact(blob.data, encode_publication(record))
    review = next((item for item in state.reviews if item.sha256 == record.review_sha256), None)
    if review is None:
        raise VerificationError("store_publication_review_missing", STAGE)
    history = state.history
    if not history.intents:
        raise VerificationError("store_publication_unready", STAGE)
    source = state.candidate.manifest.source
    expected = authorize_publication(
        candidate=state.candidate, observed=SourceObservation(head_commit=source.commit, tree=source.tree),
        scope=history.intents[-1].scope, history=history, review=review, evidence_parent=evidence_parent)
    if expected != record or encode_publication(expected) != blob.data:
        raise VerificationError("store_publication_mismatch", STAGE)
    return _Admitted(candidate=state.candidate, history=replace(history, publication=record))


def _admit(state: _Replay | None, kind: TransitionKind, blobs: _Blobs, *, raw: _History,
           evidence_parent: str, facts: _TrustedFacts) -> _Admitted:
    """Decode one structurally classified group and admit it on the facts before it.

    Registration comes first and exactly once, and nothing follows the
    freeze. Every other check belongs to the canonical codec or policy owner.
    """
    if state is None:
        if kind is not TransitionKind.REGISTER:
            raise VerificationError("store_registration_missing", STAGE)
        return _register(blobs, raw)
    if state.history.publication is not None:
        raise VerificationError("store_candidate_frozen", STAGE)
    match kind:
        case TransitionKind.INTENT:
            return _intent(state, blobs)
        case TransitionKind.CLAIM:
            return _claim(state, blobs)
        case TransitionKind.PROOF:
            return _proof(state, blobs)
        case TransitionKind.INSTALL:
            return _install(state, blobs, facts)
        case TransitionKind.REVIEW:
            return _review(state, blobs)
        case TransitionKind.TRIAGE:
            return _triage(state, blobs, facts)
        case TransitionKind.FREEZE:
            return _freeze(state, blobs, evidence_parent=evidence_parent)
    # Every other kind returned above; only a second registration remains.
    raise VerificationError("store_registration_repeated", STAGE)


def _provenance(facts: _TrustedFacts, introducing_commit: str, artifact_sha256: str) -> ReviewProvenance:
    """Select the trusted provenance of one artifact; it must name its actual introducing commit."""
    provenance = facts.provenance.get(artifact_sha256)
    if provenance is None:
        raise VerificationError("store_review_provenance_missing", STAGE)
    if provenance.transaction_commit != introducing_commit:
        raise VerificationError("store_review_provenance_foreign", STAGE)
    return provenance


def _consumed(state: _Replay | None, admitted: _Admitted,
              ) -> tuple[tuple[ClosureResolution, ...], tuple[SourceFileBinding, ...]]:
    """The closure and installer facts the history before ``admitted`` and ``admitted`` itself consume."""
    binding = () if admitted.installer_binding is None else (admitted.installer_binding,)
    if state is None:
        return admitted.closures, binding
    return state.closures + admitted.closures, state.installer_bindings + binding


def _materialize(state: _Replay | None, admitted: _Admitted, transaction: RawTransaction,
                 facts: _TrustedFacts) -> _Replay:
    """Fold one admitted, actually observed transaction; only a reviewed artifact gains provenance here."""
    reviews = () if state is None else state.reviews
    history = admitted.history
    artifact = admitted.artifact
    if isinstance(artifact, SourceReview):
        provenance = _provenance(facts, transaction.commit, sha(encode_review(artifact)))
        reviews = reviews + (ReviewedSource(record=artifact, provenance=provenance),)
    elif isinstance(artifact, TriageRecord):
        provenance = _provenance(facts, transaction.commit, sha(encode_triage(artifact)))
        triage = ReviewedTriage(record=artifact, provenance=provenance, resolutions=admitted.resolutions)
        history = replace(history, triages=history.triages + (triage,))
    closures, installer_bindings = _consumed(state, admitted)
    derived = admitted.derived_closures if state is None else state.derived_closures + admitted.derived_closures
    proofs = () if state is None else state.proofs
    if admitted.proof is not None:
        proofs = proofs + (admitted.proof,)
    return _Replay(candidate=admitted.candidate, history=history, reviews=reviews, closures=closures,
                   installer_bindings=installer_bindings, derived_closures=derived, proofs=proofs)


def _replayed(raw: _History, facts: _TrustedFacts) -> _Replay | None:
    """Fold every verified transaction in its actual order; unused facts are the caller's to account."""
    state = None
    for transaction in raw.transactions:
        admitted = _admit(state, transaction.transition.kind, transaction.blobs, raw=raw,
                          evidence_parent=transaction.parent, facts=facts)
        state = _materialize(state, admitted, transaction, facts)
    return state


def _require_all_used(facts: _TrustedFacts, *, provenance: set[str], closures: tuple[ClosureResolution, ...],
                      installer_bindings: tuple[SourceFileBinding, ...]) -> None:
    """Every trusted fact names a record the accounted history actually holds; a foreign fact refuses."""
    if set(facts.provenance) != provenance:
        raise VerificationError("store_review_provenance_unused", STAGE)
    if set(facts.closures) != {fact.evidence_sha256 for fact in closures}:
        raise VerificationError("store_closure_resolution_unused", STAGE)
    if set(facts.installer_sources) != set(installer_bindings):
        raise VerificationError("store_installer_source_unused", STAGE)


def _used_provenance(state: _Replay | None) -> set[str]:
    if state is None:
        return set()
    used = {triage.provenance.artifact_sha256 for triage in state.history.triages}
    return used | {review.provenance.artifact_sha256 for review in state.reviews}


def replay_candidate(raw: _History, *, review_provenance: tuple[ReviewProvenance, ...],
                     closure_resolutions: tuple[ClosureResolution, ...],
                     installer_sources: tuple[tuple[SourceFileBinding, bytes], ...]) -> CandidateSnapshot | None:
    """Return the candidate's decoded facts at ``raw.head``, or None when its verified history is empty.

    Registration comes first and exactly once, and no transaction follows the
    freeze. Any record, binding, policy decision or trusted fact that does
    not hold refuses the whole history. A ``CandidateHistoryAtHead`` replays
    exactly as a snapshot with the same transactions: the result holds the
    facts at that head and says nothing about which head it is.
    """
    if not isinstance(raw, (RawCandidateSnapshot, CandidateHistoryAtHead)):
        raise VerificationError("store_snapshot_invalid", STAGE)
    facts = _trusted_facts(review_provenance, closure_resolutions, installer_sources)
    state = _replayed(raw, facts)
    if state is None:
        if review_provenance or closure_resolutions or installer_sources:
            raise VerificationError("store_trusted_fact_unused", STAGE)
        return None
    _require_all_used(facts, provenance=_used_provenance(state), closures=state.closures,
                      installer_bindings=state.installer_bindings)
    # Nothing follows a replayed freeze, so a frozen history's last transaction introduced it.
    freeze_commit = None if state.history.publication is None else raw.transactions[-1].commit
    return CandidateSnapshot(head=raw.head, candidate=state.candidate, history=state.history,
                             reviews=state.reviews, freeze_commit=freeze_commit)


def admit_append(raw: RawCandidateSnapshot, additions: tuple[StoredBlob, ...], *,
                 review_provenance: tuple[ReviewProvenance, ...],
                 closure_resolutions: tuple[ClosureResolution, ...],
                 installer_sources: tuple[tuple[SourceFileBinding, bytes], ...]) -> None:
    """Refuse unless replaying ``raw`` with ``additions`` appended on ``raw.head`` would admit them.

    The facts are those of the verified history plus the closure and
    installer facts the additions consume. A proposed review or triage has no
    provenance yet, so none may be supplied for it. An append whose resulting
    history the store contract would not hold refuses. Returning grants no
    write, launch, readiness or publication authority.
    """
    if not isinstance(raw, RawCandidateSnapshot):
        raise VerificationError("store_snapshot_invalid", STAGE)
    if not candidate_commit_count_valid(len(raw.transactions) + 1):
        raise VerificationError("store_history_too_long", STAGE)
    if (not isinstance(additions, tuple) or not all(isinstance(blob, StoredBlob) for blob in additions)
            or len({blob.entry for blob in additions}) != len(additions)):
        raise VerificationError("store_transition_invalid", STAGE)
    facts = _trusted_facts(review_provenance, closure_resolutions, installer_sources)
    state = _replayed(raw, facts)
    proposed = frozenset(blob.entry for blob in additions)
    transition = classify_transition(raw.entries, raw.entries | proposed)
    if frozenset(transition.added) != proposed:
        raise VerificationError("store_transition_invalid", STAGE)
    blobs = {blob.entry: blob for blob in additions}
    admitted = _admit(state, transition.kind, tuple(blobs[entry] for entry in transition.added), raw=raw,
                      evidence_parent=raw.head, facts=facts)
    closures, installer_bindings = _consumed(state, admitted)
    _require_all_used(facts, provenance=_used_provenance(state), closures=closures,
                      installer_bindings=installer_bindings)
