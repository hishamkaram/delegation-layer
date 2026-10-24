"""Builders for validated development-candidate facts used by policy tests."""

from __future__ import annotations

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.native_evidence.contract import expected_cases
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    AttemptIntent, BlockReason, CellResult, CellStatus, ChangeScope, Closure, MatrixScope,
    PlatformClaim, PlatformProof, ProviderImport, Replacement, ReviewedChangeScope, ReviewedSource,
    ReviewProvenance, SourceReview, claim_sha256, encode_review, provider_family,
)
from scripts.delegate_acceptance.release_verification.candidate_closure import (
    ClosureResolution, ClosureResolutionRef, ClosureSubject, ResourceDisposition, ResourceResolution,
)
from scripts.delegate_acceptance.release_verification.candidate_history import (
    CandidateHistory, FindingRef, PublicationAuthorization, ReplacementApproval, ReviewedTriage,
    TriageRecord, encode_triage,
)
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    BOOTSTRAP_OUTCOME, BOOTSTRAP_SHUTDOWN, BOOTSTRAP_TERMINATION_CAUSE, BOOTSTRAP_WORKER_RESULT, CHANNEL_RULES,
    EXPECTED_DEFAULT_STATE, INSTALLER_REPO_PATH, REQUEST_SERVED, WITNESS_ROLE_KINDS, ArtifactRef,
    BootstrapObservation, CandidateInstallFacts, HarnessIdentity, InstallChannel, InstallerBinding,
    InstallObservation, InstallProof, ManifestArtifactBinding, RequestRecord, SourceFileBinding,
    encode_install_witness, release_url_pair,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    PLATFORMS, ArtifactEntry, ArtifactKind, BuildInputs, CandidateLocator, CandidateManifest,
    SourceIdentity, Toolchains, encode_manifest, expected_inventory,
)
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    BoundCandidate, SourceObservation,
)
from scripts.delegate_acceptance.release_verification.contract import BinaryDigests


VERSION = "0.1.37"
COMMIT = "a" * 40
TREE = "b" * 40
BASE = "c" * 40
BASE_TREE = "0" * 40
CHANGED_PATH_COUNT = 3
CHANGED_PATHS_SHA256 = sha(b"fixture complete changed-path inventory")
SUBSYSTEM_REGISTRY_SHA256 = sha(b"fixture canonical subsystem registry")
RATIONALE = "Fixture review: the changed paths touch only shared release tooling."
HARNESS = "d" * 40
HARNESS_SHA256 = "a" * 64
REVIEWER = "maintainer"
TRANSACTION = "e" * 40
DIAGNOSIS = ("3" * 64,)
FACT = "8" * 64
CCR_IMPORTS_CLAUDE = (ProviderImport(importer="ccr", imported="claude"),)
REPOSITORY = "hishamkaram/delegation-layer"
NATIVE_BINARIES = BinaryDigests(delegate="4" * 64, delegate_run="5" * 64, pueue="6" * 64, pueued="7" * 64)
BOOTSTRAP_ROOT_ID = "1" * 32
BOOTSTRAP_TASK_ID = "2" * 32
INSTALL_SH = b"#!/bin/sh\necho candidate install fixture\n"
INSTALL_HARNESS = HarnessIdentity(commit="9" * 40, sha256="9" * 64)
NATIVE_HARNESS_REVISION = "6" * 40
NATIVE_HARNESS_SHA256 = "6" * 64
OPENCODE_MODEL = "opencode/fixture-model"
CCR_MODEL = "ccr/fixture-model"


def manifest(version: str = VERSION, *, harness_sha256: str = HARNESS_SHA256) -> CandidateManifest:
    artifacts = tuple(
        ArtifactEntry(path=path, kind=kind, os=os_name, arch=arch, size=1000 + index,
                      sha256=sha(path.encode()))
        for index, (path, kind, os_name, arch) in enumerate(expected_inventory(version)))
    return CandidateManifest(
        version=version, build_tag="v" + version,
        source=SourceIdentity(commit=COMMIT, tree=TREE, base_commit=BASE),
        build=BuildInputs(harness_commit=HARNESS, harness_sha256=harness_sha256,
                          goreleaser_config_sha256="1" * 64,
                          go_sum_sha256="2" * 64,
                          toolchains=Toolchains(go="1.27.3", goreleaser="2.18.1",
                                                python="3.12.13", node="22.11.0")),
        artifacts=artifacts,
    )


def locator_for(value: CandidateManifest) -> CandidateLocator:
    return CandidateLocator(repository=REPOSITORY, run_id=901, run_attempt=1,
                            artifact_id=77, artifact_archive_sha256="f" * 64,
                            manifest_sha256=sha(encode_manifest(value)))


def bound() -> BoundCandidate:
    value = manifest()
    return BoundCandidate(locator=locator_for(value), manifest=value)


def observed(head: str = COMMIT, tree: str = TREE) -> SourceObservation:
    return SourceObservation(head_commit=head, tree=tree)


def scope(matrix: MatrixScope = MatrixScope.SHARED,
          changed: tuple[str, ...] = ()) -> ChangeScope:
    return ChangeScope(matrix=matrix, changed_providers=changed, provider_imports=CCR_IMPORTS_CLAUDE)


def cells(*, families: frozenset[str] | None = None,
          overrides: dict[str, tuple[CellStatus, BlockReason | None]] | None = None,
          ) -> tuple[CellResult, ...]:
    """Return PASS cells for every canonical case (or chosen families), with overrides."""
    overrides = overrides or {}
    result = []
    for case in expected_cases():
        if families is not None and provider_family(case.provider) not in families:
            continue
        status, reason = overrides.get(case.case_id, (CellStatus.PASS, None))
        result.append(CellResult(case_id=case.case_id, status=status, block_reason=reason))
    return tuple(result)


def case_id(provider: str, scenario: str) -> str:
    return next(case.case_id for case in expected_cases()
                if case.provider == provider and case.scenario == scenario)


def invocation_id(attempt: int, platform: tuple[str, str]) -> str:
    os_name, arch = platform
    return f"invocation-{attempt}-{os_name}-{arch}"


def claim(candidate: BoundCandidate, *, attempt: int, platform: tuple[str, str],
          invocation: str | None = None) -> PlatformClaim:
    """Build a claim with the execution expectations its coordinator verified; AGY uses its default."""
    os_name, arch = platform
    return PlatformClaim(candidate_id=candidate.candidate_id, attempt=attempt, os=os_name, arch=arch,
                         invocation_id=invocation or invocation_id(attempt, platform),
                         harness_revision=NATIVE_HARNESS_REVISION, harness_sha256=NATIVE_HARNESS_SHA256,
                         binaries=NATIVE_BINARIES, opencode_model=OPENCODE_MODEL, ccr_model=CCR_MODEL,
                         agy_model=None)


def claims_for(candidate: BoundCandidate, records: tuple[PlatformProof, ...]) -> tuple[PlatformClaim, ...]:
    """Return the claim each recorded proof binds."""
    return tuple(claim(candidate, attempt=item.attempt, platform=(item.os, item.arch)) for item in records)


def proof(candidate: BoundCandidate, *, attempt: int, platform: tuple[str, str],
          results: tuple[CellResult, ...] | None = None, closure: Closure = Closure.COMPLETE,
          archive_sha256: str | None = None, binaries: BinaryDigests = NATIVE_BINARIES) -> PlatformProof:
    os_name, arch = platform
    unresolved = ("pueue-task-7", "workspace-lease-7") if closure is Closure.UNRESOLVED else ()
    return PlatformProof(
        candidate_id=candidate.candidate_id, attempt=attempt, os=os_name, arch=arch,
        archive_sha256=archive_sha256 or candidate.manifest.archive(os_name, arch).sha256,
        aggregate_sha256=sha(f"aggregate-{attempt}-{os_name}-{arch}".encode()),
        proof_sha256=sha(f"proof-{attempt}-{os_name}-{arch}".encode()),
        invocation_id=invocation_id(attempt, platform), closure=closure,
        unresolved_resources=unresolved, cells=cells() if results is None else results,
        binaries=binaries,
    )


def proofs(candidate: BoundCandidate, *, attempt: int,
           results: tuple[CellResult, ...] | None = None) -> tuple[PlatformProof, ...]:
    return tuple(proof(candidate, attempt=attempt, platform=platform, results=results)
                 for platform in PLATFORMS)


def consumed(candidate: BoundCandidate, *, platform: tuple[str, str],
             channel: InstallChannel) -> tuple[ArtifactRef, ...]:
    """Return the exact manifest artifacts a channel consumes on one platform."""
    archive = candidate.manifest.archive(*platform)
    entries = [archive, *(entry for entry in candidate.manifest.artifacts
                          if entry.kind in CHANNEL_RULES[channel].consumed
                          and entry.kind is not ArtifactKind.ARCHIVE)]
    return tuple(sorted((ArtifactRef(kind=entry.kind, path=entry.path, sha256=entry.sha256)
                         for entry in entries), key=lambda ref: ref.path))


def installer(refs: tuple[ArtifactRef, ...], channel: InstallChannel) -> InstallerBinding:
    """Bind the shell installer to the source commit's install.sh, others to their artifact."""
    kind = CHANNEL_RULES[channel].installer_kind
    if kind is None:
        return SourceFileBinding(source_commit=COMMIT, repo_path=INSTALLER_REPO_PATH, sha256=sha(INSTALL_SH))
    return ManifestArtifactBinding(artifact=next(ref for ref in refs if ref.kind is kind))


def bootstrap(binaries: BinaryDigests = NATIVE_BINARIES) -> BootstrapObservation:
    """The closed installed fixture turn an accepted install of ``binaries`` records."""
    return BootstrapObservation(
        root_id=BOOTSTRAP_ROOT_ID, task_id=BOOTSTRAP_TASK_ID, binaries=binaries, outcome=BOOTSTRAP_OUTCOME,
        worker_result=BOOTSTRAP_WORKER_RESULT, termination_cause=BOOTSTRAP_TERMINATION_CAUSE, scope_empty=True,
        shutdown=BOOTSTRAP_SHUTDOWN)


def witness(candidate: BoundCandidate, *, platform: tuple[str, str], channel: InstallChannel,
            binaries: BinaryDigests = NATIVE_BINARIES) -> CandidateInstallFacts:
    """Build the single-channel witness an install run on ``platform`` would record."""
    refs = consumed(candidate, platform=platform, channel=channel)
    roles = {kind: role for role, kind in WITNESS_ROLE_KINDS.items()}
    tag = candidate.manifest.build_tag
    archive = candidate.manifest.archive(*platform).path
    urls = release_url_pair(REPOSITORY, tag, archive).expected() if CHANNEL_RULES[channel].fetches_release else ()
    observation = InstallObservation(
        channel=channel.value, requests=tuple(RequestRecord(REQUEST_SERVED, url) for url in urls),
        binaries=tuple(sorted(binaries.as_mapping().items())),
        skill_files=3 if channel is InstallChannel.NPM else 0,
        delegate_version=candidate.manifest.version, default_state=EXPECTED_DEFAULT_STATE,
        bootstrap=bootstrap(binaries), installer_sha256=installer(refs, channel).sha256)
    return CandidateInstallFacts(tag=tag, candidate_files=tuple(sorted((roles[ref.kind], ref.sha256) for ref in refs)),
                                 installs=(observation,), platform=platform)


def install(candidate: BoundCandidate, *, platform: tuple[str, str], channel: InstallChannel,
            binaries: BinaryDigests = NATIVE_BINARIES) -> InstallProof:
    """Build the install proof that binds the fixture witness of the same run."""
    refs = consumed(candidate, platform=platform, channel=channel)
    evidence = encode_install_witness(witness(candidate, platform=platform, channel=channel, binaries=binaries))
    os_name, arch = platform
    return InstallProof(
        candidate_id=candidate.candidate_id, os=os_name, arch=arch, channel=channel, consumed=refs,
        installer=installer(refs, channel), harness=INSTALL_HARNESS, binaries=binaries,
        delegate_version=candidate.manifest.version, default_state=EXPECTED_DEFAULT_STATE,
        bootstrap=bootstrap(binaries), witness_sha256=sha(evidence))


def installs_for(candidate: BoundCandidate,
                 platforms: tuple[tuple[str, str], ...]) -> tuple[InstallProof, ...]:
    return tuple(install(candidate, platform=platform, channel=channel)
                 for platform in platforms for channel in InstallChannel)


def intent(candidate: BoundCandidate, *, attempt: int = 1, change: ChangeScope | None = None,
           platforms: tuple[tuple[str, str], ...] = PLATFORMS,
           triage_sha256: str | None = None) -> AttemptIntent:
    replaces = None
    if attempt > 1:
        replaces = Replacement(prior_attempt=attempt - 1, triage_sha256=triage_sha256 or "0" * 64)
    return AttemptIntent(candidate_id=candidate.candidate_id, attempt=attempt, source_commit=COMMIT,
                         scope=change or scope(), platforms=platforms, replaces=replaces)


def approval(*, attempt: int, change: ChangeScope | None = None,
             platforms: tuple[tuple[str, str], ...] = PLATFORMS,
             source: str = COMMIT) -> ReplacementApproval:
    return ReplacementApproval(source_commit=source, next_attempt=attempt, scope=change or scope(),
                               platforms=platforms)


def triage(candidate: BoundCandidate, *, attempt: int, prior_proofs: tuple[PlatformProof, ...],
           explained: tuple[FindingRef, ...], approved: ReplacementApproval | None = None,
           unresolved: tuple[str, ...] = (), closure_refs: tuple[ClosureResolutionRef, ...] = (),
           resolutions: tuple[ClosureResolution, ...] = (),
           prior_claims: tuple[PlatformClaim, ...] | None = None) -> ReviewedTriage:
    """Build a triage with the provenance fact a trusted transaction reader would supply.

    Without ``prior_claims`` the triage names the claims its prior proofs bind.
    """
    claimed = claims_for(candidate, prior_proofs) if prior_claims is None else prior_claims
    record = TriageRecord(
        candidate_id=candidate.candidate_id, attempt=attempt,
        prior_claim_sha256s=tuple(sorted(claim_sha256(item) for item in claimed)),
        prior_proof_sha256s=tuple(sorted(item.proof_sha256 for item in prior_proofs)),
        explained_findings=tuple(sorted(set(explained))),
        diagnosis_sha256s=DIAGNOSIS if explained else (),
        unresolved_product_findings=unresolved, reviewer=REVIEWER,
        closure_resolutions=tuple(sorted(closure_refs, key=lambda ref: ref.key)), approval=approved)
    provenance = ReviewProvenance(artifact_sha256=sha(encode_triage(record)), reviewer=REVIEWER,
                                  transaction_commit=TRANSACTION)
    return ReviewedTriage(record=record, provenance=provenance,
                          resolutions=tuple(sorted(resolutions, key=lambda item: item.evidence_sha256)))


def resolution(subject: ClosureSubject, *, invocation: str,
               resources: tuple[tuple[str, ResourceDisposition], ...], fact: str = FACT) -> ClosureResolution:
    """Build the closure fact a native lifetime owner would produce after validation."""
    return ClosureResolution(
        subject=subject, invocation_id=invocation,
        resources=tuple(ResourceResolution(resource_id=resource, disposition=disposition, fact_sha256=fact)
                        for resource, disposition in sorted(resources)))


def reference(fact: ClosureResolution, *, invocation: str | None = None) -> ClosureResolutionRef:
    return ClosureResolutionRef(subject=fact.subject, invocation_id=invocation or fact.invocation_id,
                                evidence_sha256=fact.evidence_sha256)


def reviewed_scope(change: ChangeScope | None = None, *, base: str = BASE) -> ReviewedChangeScope:
    """Build the scope a reviewer attests for the fixture's base-to-head diff."""
    return ReviewedChangeScope(scope=change or scope(), base_commit=base, base_tree=BASE_TREE,
                               changed_path_count=CHANGED_PATH_COUNT,
                               changed_paths_sha256=CHANGED_PATHS_SHA256,
                               subsystem_registry_sha256=SUBSYSTEM_REGISTRY_SHA256, rationale=RATIONALE)


def review_record(candidate: BoundCandidate, *, head: str = COMMIT, unresolved: tuple[str, ...] = (),
                  change: ChangeScope | None = None, base: str = BASE) -> SourceReview:
    return SourceReview(candidate_id=candidate.candidate_id, head_commit=head, tree=TREE,
                        reviewer=REVIEWER, unresolved_findings=unresolved,
                        reviewed_scope=reviewed_scope(change, base=base))


def review(candidate: BoundCandidate, *, head: str = COMMIT, unresolved: tuple[str, ...] = (),
           change: ChangeScope | None = None, base: str = BASE) -> ReviewedSource:
    """Build a source review with the provenance a trusted transaction reader would supply."""
    record = review_record(candidate, head=head, unresolved=unresolved, change=change, base=base)
    provenance = ReviewProvenance(artifact_sha256=sha(encode_review(record)), reviewer=REVIEWER,
                                  transaction_commit=TRANSACTION)
    return ReviewedSource(record=record, provenance=provenance)


def history(candidate: BoundCandidate, *, intents: tuple[AttemptIntent, ...],
            proof_records: tuple[PlatformProof, ...] = (),
            triages: tuple[ReviewedTriage, ...] = (),
            publication: PublicationAuthorization | None = None,
            claims: tuple[PlatformClaim, ...] | None = None,
            installs: tuple[InstallProof, ...] | None = None) -> CandidateHistory:
    """Build a history; without ``claims`` it records the claim of every given proof.

    Without ``installs`` it records every channel's install for each platform
    the latest intent plans.
    """
    if installs is None:
        installs = installs_for(candidate, intents[-1].platforms) if intents else ()
    return CandidateHistory(candidate_id=candidate.candidate_id, intents=intents,
                            claims=claims_for(candidate, proof_records) if claims is None else claims,
                            proofs=proof_records, installs=installs, triages=triages,
                            publication=publication)
