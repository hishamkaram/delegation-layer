"""The development-candidate target of the native coordinator.

The invocation receipt, under the exclusive invocation lock, is the only owner of
these stages. This adapter composes the existing owners: the candidate loader
reads the hosted payload, the archive verifier extracts and hashes the bundled
binaries in process, the store's public append and load decide and record claim
and proof history, the native process owner runs and waits for the one native
child, and the native evidence owner stages, builds and validates the proof.

Only a fresh claim intent and this call's exact acknowledged claim append lead
to the native launch. A saved intent, a stored claim, a readback or a resumed
receipt never does. Closure facts have no producer yet, so every store call
passes none; a history that needs them refuses.
"""

from __future__ import annotations

from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.evidence.bounded_files import read_bounded_regular_file
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.native_evidence.public_contract import CandidateProofExpectation
from scripts.delegate_acceptance.release_verification.assets import (
    extract_archive, inspect_binaries, platform_identity,
)
from scripts.delegate_acceptance.release_verification.candidate_artifact import load_candidate_artifact
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    PlatformClaim, candidate_proof_expectation, encode_claim, parse_claim,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    MANIFEST_PATH, MAX_MANIFEST_BYTES, ArtifactKind, CandidateManifest, bind_manifest, encode_locator,
    entry_limit,
)
from scripts.delegate_acceptance.release_verification.candidate_store import append, load_candidate
from scripts.delegate_acceptance.release_verification.candidate_store_contract import (
    AppendAmbiguous, AppendConfirmed, AppendOutcome, EntryFamily, EntryName, StoredBlob, TransitionKind, entry_name,
    record_blob,
)
from scripts.delegate_acceptance.release_verification.candidate_store_replay import ReplayDenied
from scripts.delegate_acceptance.release_verification.contract import (
    BINARIES, BinaryDigests, VerificationError, parse_binary_digests,
)
from scripts.delegate_acceptance.release_verification.filesystem import bounded_digest
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from .cleanup import cleanup_candidate
from .contract import CandidateRequest, Invocation, ReleaseError, candidate_request, object_value, text_value
from .execution import (
    NATIVE_TIMEOUT_SECONDS, _child_transaction, candidate_environment, native_argv, observe_child,
)
from .preflight import observe_executing_harness, require_python
from .projection import project_candidate, read_candidate_proof
from .receipt import Receipt


STAGE = "native-candidate"


def execute_candidate(receipt: Receipt, client: GitHubClient) -> None:
    request = candidate_request(receipt.invocation)
    _preflight(receipt, client, request)
    _download(receipt, client, request)
    _archive(receipt, request)
    _claim_and_launch(receipt, client, request)
    _finish(receipt, client, request)


def resume_candidate(receipt: Receipt, client: GitHubClient) -> None:
    """Observe only: no download, extraction, claim append or native launch.

    A proven native completion goes on to projection, the first proof append
    of a still pending record, readback of an intended one, and cleanup.
    """
    request = candidate_request(receipt.invocation)
    if any(receipt.stage(name).state != "complete" for name in ("preflight", "download", "archive")):
        raise ReleaseError("candidate_pre_claim_incomplete_no_rerun", incomplete=True)
    claim = receipt.stage("claim")
    if claim.state != "complete":
        raise ReleaseError("candidate_claim_unresolved_no_launch", incomplete=True)
    if claim.result.get("disposition") != "confirmed":
        raise ReleaseError("candidate_claim_refused")
    native = receipt.stage("native")
    if native.state == "pending":
        raise ReleaseError("candidate_native_not_launched_no_rerun", incomplete=True)
    if native.state == "intended":
        receipt.complete("native", observe_child(receipt, "native"))
    _finish(receipt, client, request)


def _preflight(receipt: Receipt, client: GitHubClient, request: CandidateRequest) -> None:
    """Bind the executing native harness, this platform and the exact registered locator."""
    invocation = receipt.invocation
    if not receipt.intend("preflight", {"request": request.document()}):
        raise ReleaseError("candidate_preflight_incomplete_no_rerun", incomplete=True)
    require_python()
    platform = platform_identity()
    if (platform["os"], platform["arch"]) != (invocation.platform_os, invocation.platform_arch):
        raise ReleaseError("candidate_platform_changed")
    harness = observe_executing_harness(client.receipt, client.environment, checkout=invocation.checkout)
    snapshot = load_candidate(client, request.store, request.candidate_id, closure_resolutions=())
    if snapshot is None:
        raise ReleaseError("candidate_not_registered")
    # Proof admission derives its expectation from the registered locator, so a
    # rebuilt artifact with the same manifest is still a different candidate run.
    if encode_locator(snapshot.candidate.locator) != encode_locator(request.locator):
        raise ReleaseError("candidate_locator_mismatch")
    receipt.complete("preflight", {**harness, "store_head": snapshot.head})


def _download(receipt: Receipt, client: GitHubClient, request: CandidateRequest) -> None:
    """Load the hosted payload into the owned runner; keep its exact verified manifest bytes."""
    invocation = receipt.invocation
    runner = invocation.root / "runner"
    if not receipt.intend("download", {"runner": str(runner), "payload": str(runner / "payload")}):
        raise ReleaseError("candidate_download_incomplete_no_rerun", incomplete=True)
    owner = DirectoryOwner()
    try:
        try:
            stage = owner.create("candidate private stage", invocation.root / "private-stage", invocation.root)
        except FileExistsError as error:
            raise ReleaseError("private_stage_identity_unavailable", incomplete=True) from error
        identities = {"stage_identity": stage,
                      "runner_identity": owner.create("candidate runner", runner, invocation.root),
                      "assets_identity": owner.create("candidate assets", invocation.assets, runner),
                      "payload_identity": owner.create("candidate payload", runner / "payload", runner),
                      "store_identity": owner.create("candidate store requests", runner / "store", runner)}
        artifact = load_candidate_artifact(client, request.locator, container=runner / "candidate-artifact.zip",
                                           payload=runner / "payload")
        data = read_bounded_regular_file(runner / "payload" / MANIFEST_PATH, MAX_MANIFEST_BYTES)
        if bind_manifest(request.locator, data) != artifact.manifest:
            raise ReleaseError("candidate_manifest_changed")
        receipt.complete("download", {
            **{name: directory_identity_record(token) for name, token in identities.items()},
            "observation": artifact.observation_record(), "manifest": data.decode("utf-8"),
        })
    finally:
        owner.release()


def _manifest(receipt: Receipt, request: CandidateRequest) -> CandidateManifest:
    """The verified manifest from its persisted exact bytes; usable after runner cleanup."""
    text = text_value(receipt.stage("download").result.get("manifest"))
    return bind_manifest(request.locator, text.encode("utf-8"))


def _archive(receipt: Receipt, request: CandidateRequest) -> None:
    """Extract this platform's archive and hash its binaries in process; nothing is executed."""
    invocation = receipt.invocation
    entry = _manifest(receipt, request).archive(invocation.platform_os, invocation.platform_arch)
    archive = invocation.root / "runner" / "payload" / entry.path
    extracted = invocation.assets / "extracted"
    if not receipt.intend("archive", {"archive": entry.path, "sha256": entry.sha256,
                                      "extracted": str(extracted)}):
        raise ReleaseError("candidate_archive_incomplete_no_rerun", incomplete=True)
    if bounded_digest(archive, entry_limit(ArtifactKind.ARCHIVE), "candidate_archive_unreadable") != entry.sha256:
        raise ReleaseError("candidate_archive_mismatch")
    identities: dict[str, object] = {}
    record: dict[str, dict[str, object]] = {"artifact": {}, "cleanup": {"directory_identities": identities}}
    owner = DirectoryOwner()
    try:
        extract_archive(archive, extracted, owner, record)
    finally:
        owner.release()
    receipt.complete("archive", {"archive_sha256": entry.sha256, "extraction_identity": identities,
                                 "binaries": _binaries(invocation).as_mapping()})


def _binaries(invocation: Invocation) -> BinaryDigests:
    """Inspect the extracted binaries with the canonical verifier; read their digests, run nothing."""
    inspected: dict[str, object] = {}
    inspect_binaries(invocation.assets / "extracted", {"binaries": inspected})
    return parse_binary_digests({name: object_value(inspected.get(name)).get("sha256") for name in BINARIES},
                                STAGE)


def _claim(receipt: Receipt, request: CandidateRequest) -> PlatformClaim:
    """Compose the claim from the observed harness, platform and re-inspected binaries."""
    invocation = receipt.invocation
    binaries = _binaries(invocation)
    if binaries.as_mapping() != receipt.stage("archive").result.get("binaries"):
        raise ReleaseError("candidate_binaries_changed")
    facts = receipt.stage("preflight").result
    return PlatformClaim(
        candidate_id=request.candidate_id, attempt=request.attempt, os=invocation.platform_os,
        arch=invocation.platform_arch, invocation_id=invocation.invocation_id,
        harness_revision=text_value(facts.get("harness_revision")),
        harness_sha256=text_value(facts.get("harness_sha256")), binaries=binaries,
        opencode_model=request.opencode_model, ccr_model=request.ccr_model, agy_model=request.agy_model)


def _claim_and_launch(receipt: Receipt, client: GitHubClient, request: CandidateRequest) -> None:
    """The only native launch: a fresh claim intent, then this call's exact acknowledged claim."""
    claim = _claim(receipt, request)
    data = encode_claim(claim)
    digest = sha(data)
    if not receipt.intend("claim", {"claim": data.decode("utf-8"), "claim_sha256": digest}):
        raise ReleaseError("candidate_claim_intent_exists_no_launch", incomplete=True)
    name = EntryName(EntryFamily.CLAIM, attempt=claim.attempt, platform=claim.platform)
    outcome = _append(receipt, client, request, "claim", record_blob(name, data))
    _require_confirmed(receipt, "claim", outcome, kind=TransitionKind.CLAIM, digest=digest)
    invocation = receipt.invocation
    env = candidate_environment(invocation, claim.harness_revision)
    result = _child_transaction(receipt, "native", native_argv(invocation), env, timeout=NATIVE_TIMEOUT_SECONDS)
    receipt.complete("native", result)


def _append(receipt: Receipt, client: GitHubClient, request: CandidateRequest, stage: str,
            blob: StoredBlob) -> AppendOutcome:
    """One public store append; only its definite refusal is recorded as refused.

    A store refusal raises before any write. Everything after the append,
    including persisting its outcome, stays outside this handler, so a lost
    result leaves the stage intended rather than refused.
    """
    try:
        return append(client, request.store, request.candidate_id, (blob,), closure_resolutions=(),
                      work_directory=receipt.invocation.root / "runner" / "store",
                      observe_failure=lambda error: receipt.report(
                          "INCOMPLETE", f"store_append_ambiguous:{error.code}"))
    except VerificationError as error:
        findings = error.findings if isinstance(error, ReplayDenied) else ()
        receipt.complete(stage, {"disposition": "refused", "code": error.code,
                                 "findings": [{"code": item.code, "subject": item.subject} for item in findings]})
        raise ReleaseError(f"candidate_{stage}_refused:{error.code}", incomplete=stage == "record") from error


def _require_confirmed(receipt: Receipt, stage: str, outcome: AppendOutcome, *, kind: TransitionKind,
                       digest: str) -> None:
    """Persist only an acknowledgement of exactly this one record; anything else stays intended."""
    if isinstance(outcome, AppendAmbiguous):
        raise ReleaseError(f"candidate_{stage}_append_ambiguous:{outcome.failure}", incomplete=True)
    if not isinstance(outcome, AppendConfirmed):
        raise ReleaseError(f"candidate_{stage}_append_unconfirmed", incomplete=True)
    if outcome.kind is not kind or outcome.record_sha256s != (digest,):
        raise ReleaseError(f"candidate_{stage}_ack_mismatch", incomplete=True)
    receipt.complete(stage, {"disposition": "confirmed", "commit": outcome.commit, "parent": outcome.parent})


def _expectation(receipt: Receipt, request: CandidateRequest) -> CandidateProofExpectation:
    """Rebuild the proof expectation from persisted manifest and claim bytes, never from scratch files."""
    intent = receipt.stage("claim").intent
    data = text_value(intent.get("claim")).encode("utf-8")
    claim = parse_claim(data)
    if sha(data) != intent.get("claim_sha256") or encode_claim(claim) != data:
        raise ReleaseError("candidate_claim_record_changed", incomplete=True)
    return candidate_proof_expectation(_manifest(receipt, request), request.locator, claim)


def _finish(receipt: Receipt, client: GitHubClient, request: CandidateRequest) -> None:
    expected = _expectation(receipt, request)
    project_candidate(receipt, expected)
    _record(receipt, client, request, expected)
    cleanup_candidate(receipt)
    _report(receipt)


def _record(receipt: Receipt, client: GitHubClient, request: CandidateRequest,
            expected: CandidateProofExpectation) -> None:
    """Append the proof once from a fresh record intent; an existing intent reads back only."""
    stage = receipt.stage("record")
    if stage.state == "complete":
        if stage.result.get("disposition") == "refused":
            raise ReleaseError(f"candidate_record_refused:{stage.result.get('code')}", incomplete=True)
        return
    data, facts = read_candidate_proof(receipt, expected)
    name = EntryName(EntryFamily.PROOF, attempt=request.attempt,
                     platform=(expected.platform_os, expected.platform_arch))
    digest = text_value(facts["proof_sha256"])
    if receipt.intend("record", {"entry": entry_name(name), "proof_sha256": digest}):
        outcome = _append(receipt, client, request, "record", record_blob(name, data))
        _require_confirmed(receipt, "record", outcome, kind=TransitionKind.PROOF, digest=digest)
        return
    snapshot = load_candidate(client, request.store, request.candidate_id, closure_resolutions=())
    if snapshot is None:
        raise ReleaseError("candidate_proof_record_unresolved", incomplete=True)
    key = (request.attempt, expected.platform_os, expected.platform_arch)
    if not any((proof.attempt, proof.os, proof.arch) == key and proof.proof_sha256 == digest
               and proof.invocation_id == expected.invocation_id for proof in snapshot.history.proofs):
        raise ReleaseError("candidate_proof_record_unresolved", incomplete=True)
    receipt.complete("record", {"disposition": "readback", "head": snapshot.head})


def _report(receipt: Receipt) -> None:
    """Report the recorded outcome; a local proof is never readiness or publication authority."""
    project = object_value(receipt.stage("project").result)
    verdict = text_value(project.get("case_results_verdict"))
    if project.get("failed_count") != 0:
        receipt.report("FAIL", "candidate_native_fail_recorded_evidence_retained")
    elif project.get("unresolved_case_ids") != []:
        receipt.report("INCOMPLETE", "candidate_native_unresolved_recorded_evidence_retained")
    elif verdict == "FAIL":
        receipt.report("FAIL", "candidate_native_fail_recorded_evidence_retained")
    elif receipt.stage("record").result.get("disposition") != "confirmed":
        # A readback is checked before cleanup: a stage completed by an earlier
        # build cannot prove the original store command's closure either.
        receipt.report("INCOMPLETE", "candidate_proof_readback_transport_closure_unproven")
    elif receipt.stage("cleanup").state != "complete":
        receipt.report("INCOMPLETE", "candidate_cleanup_pending")
    else:
        receipt.report("LOCAL_ONLY", f"candidate_native_{verdict.lower()}_recorded_not_readiness_authority")
