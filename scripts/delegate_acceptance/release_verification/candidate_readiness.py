"""Pure readiness, launch, publication-freeze and publication-source policy for development candidates.

This is the one decision point that turns validated candidate facts into a
verdict. It reads no storage, network, process or clock. Required cells come
from the canonical native case matrix and an explicit change scope that the
source review attests; a later attempt never rewrites an earlier attempt's
verdict.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum

from ..evidence.digests import sha
from ..native_evidence.contract import CaseDefinition, expected_cases
from .candidate_attempts import (
    AttemptIntent, BlockReason, CellStatus, ChangeScope, Closure, PlatformClaim, PlatformProof,
    ReviewedSource, claim_sha256, provider_family,
)
from .candidate_closure import ClaimSubject, ClosureSubject, ProofSubject, subject_key
from .candidate_history import CandidateHistory, FindingRef, PublicationAuthorization, ReviewedTriage
from .candidate_install_contract import InstallChannel
from .candidate_manifest import (
    REQUIRED_OSES, ArtifactKind, CandidateLocator, CandidateManifest, encode_manifest,
)
from .contract import REVISION_RE, VerificationError


STAGE = "decide-candidate-readiness"


# Only a real unavailable credential or executable is neutral, and only in a
# full-matrix scope for a provider the change did not affect.
NEUTRAL_BLOCK_REASONS = frozenset({BlockReason.AUTH_UNAVAILABLE, BlockReason.EXECUTABLE_UNAVAILABLE})


@dataclass(frozen=True)
class BoundCandidate:
    """A manifest whose canonical bytes match its external locator digest."""

    locator: CandidateLocator
    manifest: CandidateManifest

    def __post_init__(self) -> None:
        if sha(encode_manifest(self.manifest)) != self.locator.manifest_sha256:
            raise VerificationError("candidate_manifest_digest_mismatch", STAGE)

    @property
    def candidate_id(self) -> str:
        return self.locator.candidate_id


@dataclass(frozen=True)
class SourceObservation:
    """The current pull-request head and tree observed by the caller."""

    head_commit: str
    tree: str

    def __post_init__(self) -> None:
        for value in (self.head_commit, self.tree):
            if not isinstance(value, str) or REVISION_RE.fullmatch(value) is None:
                raise VerificationError("candidate_source_observation_invalid", STAGE)


class AttemptState(Enum):
    PASS = "PASS"
    FAIL = "FAIL"
    INCOMPLETE = "INCOMPLETE"


@dataclass(frozen=True)
class AttemptVerdict:
    attempt: int
    state: AttemptState
    findings: tuple[FindingRef, ...]


@dataclass(frozen=True)
class Readiness:
    """Readiness verdict; a ready verdict binds every fact it relied on.

    ``replacements`` holds, for each later attempt, the reviewed triage with
    its provenance and closure resolutions that authorized it.
    ``install_sha256s`` holds the proof digest of each required install of
    the latest attempt, never another install the history records.
    """

    candidate_id: str
    ready: bool
    findings: tuple[FindingRef, ...]
    attempt_verdicts: tuple[AttemptVerdict, ...]
    proof_sha256s: tuple[str, ...]
    install_sha256s: tuple[str, ...]
    replacements: tuple[ReviewedTriage, ...]
    review_sha256: str | None


@dataclass(frozen=True)
class LaunchDecision:
    allowed: bool
    findings: tuple[FindingRef, ...]


@dataclass(frozen=True)
class PublicationSource:
    """A squash merge publishes the built tree; its commit stays a separate fact."""

    build_commit: str
    merged_commit: str
    tree: str


def required_cases(scope: ChangeScope) -> tuple[CaseDefinition, ...]:
    affected = scope.affected_providers()
    return tuple(case for case in expected_cases()
                 if scope.full_matrix or provider_family(case.provider) in affected)


def _cell_finding(scope: ChangeScope, case: CaseDefinition, status: CellStatus,
                  reason: BlockReason | None) -> str | None:
    if status is CellStatus.PASS:
        return None
    if status is CellStatus.FAIL:
        return "cell_failed"
    if provider_family(case.provider) in scope.affected_providers():
        return "affected_cell_blocked"
    if scope.full_matrix and reason in NEUTRAL_BLOCK_REASONS:
        return None
    return "cell_blocked"


def _proof_findings(manifest: CandidateManifest, scope: ChangeScope,
                    proof: PlatformProof) -> tuple[list[FindingRef], bool]:
    """Return findings and whether any required cell passed."""
    platform = f"{proof.os}/{proof.arch}"
    findings, passed = [], False
    if proof.archive_sha256 != manifest.archive(proof.os, proof.arch).sha256:
        findings.append(FindingRef(code="artifact_mismatch", subject=platform))
    if proof.closure is not Closure.COMPLETE:
        findings.append(FindingRef(code="closure_unresolved", subject=platform))
    results = {cell.case_id: cell for cell in proof.cells}
    required = required_cases(scope)
    for case in required:
        cell = results.get(case.case_id)
        code = "coverage_missing" if cell is None else _cell_finding(
            scope, case, cell.status, cell.block_reason)
        passed = passed or (cell is not None and cell.status is CellStatus.PASS)
        if code is not None:
            findings.append(FindingRef(code=code, subject=f"{platform}/{case.case_id}"))
    required_ids = {case.case_id for case in required}
    for cell in proof.cells:
        if cell.case_id not in required_ids and cell.status is CellStatus.FAIL:
            findings.append(FindingRef(code="cell_failed", subject=f"{platform}/{cell.case_id}"))
    return findings, passed


def evaluate_attempt(manifest: CandidateManifest, history: CandidateHistory,
                     attempt: int) -> AttemptVerdict:
    """Judge one attempt against its own recorded intent scope."""
    intent = history.intents[attempt - 1]
    proofs = {(proof.os, proof.arch): proof for proof in history.proofs_for(attempt)}
    findings: list[FindingRef] = []
    passing_oses: set[str] = set()
    for os_name, arch in intent.platforms:
        proof = proofs.get((os_name, arch))
        if proof is None:
            findings.append(FindingRef(code="proof_missing", subject=f"{os_name}/{arch}"))
            continue
        proof_findings, passed = _proof_findings(manifest, intent.scope, proof)
        findings.extend(proof_findings)
        if passed:
            passing_oses.add(os_name)
    planned_oses = {os_name for os_name, _ in intent.platforms}
    for os_name in REQUIRED_OSES:
        if os_name not in planned_oses:
            findings.append(FindingRef(code="required_platform_missing", subject=os_name))
        elif os_name not in passing_oses and len(proofs) == len(intent.platforms):
            findings.append(FindingRef(code="required_os_without_pass", subject=os_name))
    if len(proofs) != len(intent.platforms):
        state = AttemptState.INCOMPLETE
    else:
        state = AttemptState.FAIL if findings else AttemptState.PASS
    return AttemptVerdict(attempt=attempt, state=state, findings=tuple(sorted(set(findings))))


def _approval_findings(manifest: CandidateManifest, intent: AttemptIntent,
                       triage: ReviewedTriage) -> list[FindingRef]:
    approval = triage.record.approval
    subject = str(intent.attempt)
    if approval is None:
        return [FindingRef(code="replacement_unapproved", subject=subject)]
    expected = (manifest.source.commit, intent.attempt, intent.scope, intent.platforms)
    if (approval.source_commit, approval.next_attempt, approval.scope, approval.platforms) != expected:
        return [FindingRef(code="replacement_target_mismatch", subject=subject)]
    return []


@dataclass(frozen=True)
class _ClosureObligation:
    """One prior lifetime a replacement must see positively closed.

    ``resources`` is the exact inventory a proof left unresolved; a claim
    without a proof has no proof inventory, so its resolution's producer owns
    the inventory and this policy checks only its claim-closure shape.
    """

    subject: ClosureSubject
    invocation_id: str
    resources: tuple[str, ...] | None
    label: str


def _closure_obligations(history: CandidateHistory, attempt: int) -> list[_ClosureObligation]:
    obligations = []
    proofs = {(proof.os, proof.arch): proof for proof in history.proofs_for(attempt)}
    for claim in history.claims_for(attempt):
        label = f"{claim.os}/{claim.arch}"
        proof = proofs.get(claim.platform)
        if proof is None:
            obligations.append(_ClosureObligation(subject=ClaimSubject(claim_sha256=claim_sha256(claim)),
                                                  invocation_id=claim.invocation_id, resources=None,
                                                  label=label))
        elif proof.closure is Closure.UNRESOLVED:
            obligations.append(_ClosureObligation(subject=ProofSubject(proof_sha256=proof.proof_sha256),
                                                  invocation_id=proof.invocation_id,
                                                  resources=proof.unresolved_resources, label=label))
    return obligations


def _closure_findings(triage: ReviewedTriage, history: CandidateHistory, attempt: int) -> list[FindingRef]:
    """Bind every unproven claim and unresolved proof to a validated resolution of its own invocation."""
    obligations = _closure_obligations(history, attempt)
    references = {ref.key: ref for ref in triage.record.closure_resolutions}
    resolved = {fact.evidence_sha256: fact for fact in triage.resolutions}
    expected = {subject_key(item.subject) for item in obligations}
    findings = [FindingRef(code="closure_resolution_foreign", subject=f"{kind}:{digest}")
                for kind, digest in sorted(set(references) - expected)]
    for obligation in obligations:
        reference = references.get(subject_key(obligation.subject))
        fact = resolved.get(reference.evidence_sha256) if reference is not None else None
        if reference is None:
            findings.append(FindingRef(code="closure_resolution_missing", subject=obligation.label))
        elif fact is None:
            findings.append(FindingRef(code="closure_resolution_unverified", subject=obligation.label))
        elif (fact.subject != obligation.subject or fact.invocation_id != obligation.invocation_id
              or reference.invocation_id != obligation.invocation_id
              or (obligation.resources is not None and fact.resource_ids != obligation.resources)):
            findings.append(FindingRef(code="closure_resolution_mismatch", subject=obligation.label))
    return findings


def _replacement_findings(manifest: CandidateManifest, history: CandidateHistory,
                          intent: AttemptIntent, prior: AttemptVerdict) -> list[FindingRef]:
    """The one replacement policy: a reviewed triage explains the prior attempt and approves this one.

    An incomplete prior attempt keeps its verdict; it may be replaced only
    when every claim without a proof has a validated positive closure.
    """
    subject = str(intent.attempt)
    if intent.replaces is None or not history.triages_for(prior.attempt):
        return [FindingRef(code="replacement_unexplained", subject=subject)]
    triage = history.selected_triage(prior.attempt, intent.replaces.triage_sha256)
    if triage is None:
        return [FindingRef(code="triage_mismatch", subject=subject)]
    record = triage.record
    findings = _approval_findings(manifest, intent, triage)
    claims = tuple(sorted(claim_sha256(claim) for claim in history.claims_for(prior.attempt)))
    if record.prior_claim_sha256s != claims:
        findings.append(FindingRef(code="triage_claim_mismatch", subject=str(prior.attempt)))
    proofs = tuple(sorted(proof.proof_sha256 for proof in history.proofs_for(prior.attempt)))
    if record.prior_proof_sha256s != proofs:
        findings.append(FindingRef(code="triage_proof_mismatch", subject=str(prior.attempt)))
    if not set(prior.findings) <= set(record.explained_findings):
        findings.append(FindingRef(code="triage_incomplete", subject=str(prior.attempt)))
    if not set(record.explained_findings) <= set(prior.findings):
        findings.append(FindingRef(code="triage_foreign_finding", subject=str(prior.attempt)))
    if record.unresolved_product_findings:
        findings.append(FindingRef(code="product_finding_unresolved", subject=str(prior.attempt)))
    return findings + _closure_findings(triage, history, prior.attempt)


def _history_assessment(candidate: BoundCandidate, history: CandidateHistory,
                        ) -> tuple[tuple[AttemptVerdict, ...], list[FindingRef]]:
    """The one complete-history policy shared by paid launch and final readiness.

    Every recorded attempt keeps its own verdict, and every recorded
    replacement edge must satisfy the same replacement policy.
    """
    findings = []
    if history.candidate_id != candidate.candidate_id:
        findings.append(FindingRef(code="candidate_mismatch", subject=history.candidate_id))
    for intent in history.intents:
        if intent.source_commit != candidate.manifest.source.commit:
            findings.append(FindingRef(code="attempt_source_mismatch", subject=str(intent.attempt)))
    publication = history.publication
    if publication is not None and publication.attempt != len(history.intents):
        findings.append(FindingRef(code="publication_stale", subject=publication.sha256))
    verdicts = tuple(evaluate_attempt(candidate.manifest, history, attempt)
                     for attempt in range(1, len(history.intents) + 1))
    for intent in history.intents[1:]:
        findings.extend(_replacement_findings(candidate.manifest, history, intent,
                                              verdicts[intent.attempt - 2]))
    return verdicts, findings


def _review_findings(candidate: BoundCandidate, scope: ChangeScope,
                     review: ReviewedSource | None) -> list[FindingRef]:
    """Bind the review to the candidate source, its manifest baseline and the evaluated scope."""
    if review is None:
        return [FindingRef(code="review_missing", subject=candidate.candidate_id)]
    if not isinstance(review, ReviewedSource):
        return [FindingRef(code="review_unverified", subject=candidate.candidate_id)]
    source = candidate.manifest.source
    record = review.record
    findings = []
    if (record.candidate_id, record.head_commit, record.tree) != (
            candidate.candidate_id, source.commit, source.tree):
        findings.append(FindingRef(code="review_binding_mismatch", subject=review.sha256))
    if record.reviewed_scope.base_commit != source.base_commit:
        findings.append(FindingRef(code="review_baseline_mismatch", subject=review.sha256))
    if record.reviewed_scope.scope != scope:
        findings.append(FindingRef(code="review_scope_mismatch", subject=review.sha256))
    if record.unresolved_findings:
        findings.append(FindingRef(code="review_findings_unresolved", subject=review.sha256))
    return findings


def _install_assessment(candidate: BoundCandidate,
                        history: CandidateHistory) -> tuple[list[FindingRef], tuple[str, ...]]:
    """Every planned platform of the latest attempt needs every channel on its native binaries.

    An install consumes the platform's manifest archive and must install the
    exact binaries the latest native proof of that platform bound. Return the
    findings and the sorted proof digests of exactly the required installs.
    """
    latest = history.intents[-1]
    native = {(proof.os, proof.arch): proof for proof in history.proofs_for(latest.attempt)}
    findings, selected = [], []
    for platform in latest.platforms:
        archive = candidate.manifest.archive(*platform).sha256
        proof = native.get(platform)
        for channel in InstallChannel:
            subject = f"{platform[0]}/{platform[1]}/{channel.value}"
            install = history.install_for(platform, channel)
            if install is None:
                findings.append(FindingRef(code="install_proof_missing", subject=subject))
                continue
            selected.append(install.sha256)
            if install.consumed_ref(ArtifactKind.ARCHIVE).sha256 != archive:
                findings.append(FindingRef(code="install_archive_mismatch", subject=subject))
            if proof is not None and install.binaries != proof.binaries:
                findings.append(FindingRef(code="install_binary_mismatch", subject=subject))
    return findings, tuple(sorted(selected))


def evaluate_readiness(*, candidate: BoundCandidate, observed: SourceObservation,
                       scope: ChangeScope, history: CandidateHistory,
                       review: ReviewedSource | None) -> Readiness:
    """Return READY only when the latest attempt and its installs prove the reviewed scope and source.

    The reviewed scope, the latest intent's scope and ``scope`` must be equal.
    """
    verdicts, findings = _history_assessment(candidate, history)
    source = candidate.manifest.source
    if (observed.head_commit, observed.tree) != (source.commit, source.tree):
        findings.append(FindingRef(code="source_changed", subject=observed.head_commit))
    findings.extend(_review_findings(candidate, scope, review))
    install_sha256s: tuple[str, ...] = ()
    if not verdicts:
        findings.append(FindingRef(code="attempt_missing", subject=candidate.candidate_id))
    else:
        latest = history.intents[-1]
        if latest.scope != scope:
            findings.append(FindingRef(code="attempt_scope_stale", subject=str(latest.attempt)))
        if verdicts[-1].state is AttemptState.INCOMPLETE:
            findings.append(FindingRef(code="attempt_incomplete", subject=str(latest.attempt)))
        findings.extend(verdicts[-1].findings)
        install_findings, install_sha256s = _install_assessment(candidate, history)
        findings.extend(install_findings)
    if findings or not isinstance(review, ReviewedSource):
        return Readiness(candidate_id=candidate.candidate_id, ready=False,
                         findings=tuple(sorted(set(findings))), attempt_verdicts=verdicts,
                         proof_sha256s=(), install_sha256s=(), replacements=(), review_sha256=None)
    proofs = history.proofs_for(len(history.intents))
    return Readiness(
        candidate_id=candidate.candidate_id, ready=True, findings=(), attempt_verdicts=verdicts,
        proof_sha256s=tuple(sorted(proof.proof_sha256 for proof in proofs)),
        install_sha256s=install_sha256s,
        replacements=tuple(_selected_triages(history)),
        review_sha256=review.sha256,
    )


def _selected_triages(history: CandidateHistory) -> list[ReviewedTriage]:
    """Return the digest-selected triage authorizing each recorded replacement, in order."""
    selected = []
    for intent in history.intents[1:]:
        replaces = intent.replaces
        triage = None if replaces is None else history.selected_triage(replaces.prior_attempt,
                                                                       replaces.triage_sha256)
        if triage is None:
            raise VerificationError("candidate_replacement_unselected", STAGE)
        selected.append(triage)
    return selected


def decide_launch(*, candidate: BoundCandidate, history: CandidateHistory,
                  intent: AttemptIntent) -> LaunchDecision:
    """Authorize recording one new paid-attempt intent; resume never relaunches."""
    verdicts, findings = _history_assessment(candidate, history)
    subject = str(intent.attempt)
    if history.publication is not None:
        findings.append(FindingRef(code="candidate_frozen", subject=history.publication.sha256))
    if intent.candidate_id != candidate.candidate_id:
        findings.append(FindingRef(code="candidate_mismatch", subject=intent.candidate_id))
    if intent.source_commit != candidate.manifest.source.commit:
        findings.append(FindingRef(code="attempt_source_mismatch", subject=subject))
    if intent.attempt != len(history.intents) + 1:
        findings.append(FindingRef(code="attempt_number_invalid", subject=subject))
    planned = {os_name for os_name, _ in intent.platforms}
    for os_name in REQUIRED_OSES:
        if os_name not in planned:
            findings.append(FindingRef(code="required_platform_missing", subject=os_name))
    if not findings and intent.attempt > 1:
        findings.extend(_replacement_findings(candidate.manifest, history, intent, verdicts[-1]))
    return LaunchDecision(allowed=not findings, findings=tuple(sorted(set(findings))))


def decide_platform_claim(*, candidate: BoundCandidate, history: CandidateHistory,
                          claim: PlatformClaim) -> LaunchDecision:
    """Authorize appending one platform claim before its paid native run.

    Only a planned, unclaimed platform of the latest attempt may be claimed.
    An existing claim, even with the same invocation, is observe-only. The
    decision authorizes the append; only its acknowledged commit grants launch.
    """
    _, findings = _history_assessment(candidate, history)
    subject = f"{claim.os}/{claim.arch}"
    if history.publication is not None:
        findings.append(FindingRef(code="candidate_frozen", subject=history.publication.sha256))
    if claim.candidate_id != candidate.candidate_id:
        findings.append(FindingRef(code="candidate_mismatch", subject=claim.candidate_id))
    if not history.intents:
        findings.append(FindingRef(code="attempt_missing", subject=candidate.candidate_id))
    elif claim.attempt != len(history.intents):
        findings.append(FindingRef(code="claim_attempt_stale", subject=str(claim.attempt)))
    elif claim.platform not in history.intents[-1].platforms:
        findings.append(FindingRef(code="claim_platform_unplanned", subject=subject))
    elif history.claim_for(claim.attempt, claim.platform) is not None:
        findings.append(FindingRef(code="claim_exists", subject=subject))
    if any(existing.invocation_id == claim.invocation_id for existing in history.claims):
        findings.append(FindingRef(code="claim_invocation_reused", subject=claim.invocation_id))
    return LaunchDecision(allowed=not findings, findings=tuple(sorted(set(findings))))


def authorize_publication(*, candidate: BoundCandidate, observed: SourceObservation,
                          scope: ChangeScope, history: CandidateHistory,
                          review: ReviewedSource | None, evidence_parent: str) -> PublicationAuthorization:
    """Return the freeze record of a ready, not yet frozen history.

    ``evidence_parent`` is the evidence commit the verified store observed
    before the freeze. Store replay calls this on the history before the
    freeze and requires the stored record to equal the result exactly.
    """
    if history.publication is not None:
        raise VerificationError("candidate_frozen", STAGE)
    readiness = evaluate_readiness(candidate=candidate, observed=observed, scope=scope, history=history,
                                   review=review)
    if not readiness.ready or readiness.review_sha256 is None:
        raise VerificationError("publication_not_ready", STAGE)
    source = candidate.manifest.source
    return PublicationAuthorization(
        candidate_id=readiness.candidate_id, attempt=len(history.intents), evidence_parent=evidence_parent,
        source_commit=source.commit, source_tree=source.tree, proof_sha256s=readiness.proof_sha256s,
        install_sha256s=readiness.install_sha256s,
        triage_sha256s=tuple(sorted(triage.sha256 for triage in readiness.replacements)),
        review_sha256=readiness.review_sha256,
    )


def publication_source(*, manifest: CandidateManifest, merged_commit: str,
                       merged_tree: str) -> PublicationSource:
    """Require the squash-merged tree to equal the built tree exactly."""
    for value in (merged_commit, merged_tree):
        if not isinstance(value, str) or REVISION_RE.fullmatch(value) is None:
            raise VerificationError("publication_source_invalid", STAGE)
    if merged_tree != manifest.source.tree:
        raise VerificationError("publication_tree_mismatch", STAGE)
    return PublicationSource(build_commit=manifest.source.commit, merged_commit=merged_commit,
                             tree=merged_tree)
