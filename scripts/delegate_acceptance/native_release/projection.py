"""Bind immutable public bytes to verified archive and private native receipts."""

from __future__ import annotations

from dataclasses import asdict

from scripts.delegate_acceptance.evidence.bounded_files import read_bounded_regular_file
from scripts.delegate_acceptance.native_evidence.candidate_proof import completion_record, validate_candidate_proof
from scripts.delegate_acceptance.native_evidence.public_contract import (
    MAX_PUBLIC_ZIP_BYTES, CandidateProofExpectation, ProofExpectation, canonical_json, sha256,
    validate_expectation,
)
from scripts.delegate_acceptance.native_evidence.contract import AGGREGATE_RECEIPT_NAME, EvidenceError
from scripts.delegate_acceptance.native_evidence.public_projection import build_candidate_proof, build_public_proof
from scripts.delegate_acceptance.native_evidence.public_case_validation import agy_case_model_bound
from scripts.delegate_acceptance.native_evidence.public_stage import NATIVE_COMPLETION_NAME
from scripts.delegate_acceptance.native_evidence.public_validation import (
    inspect_public_proof, validate_public_identity, validate_public_proof,
)
from scripts.delegate_acceptance.native_evidence.staging import stage_candidate_evidence, stage_evidence
from .contract import ReleaseError, digest_value, integer, object_value, release_request, text_value
from .execution import file_sha, observe_child_completion, read_object
from .receipt import Receipt


def expectation_from_record(value: object) -> ProofExpectation:
    record = object_value(value)
    ccr_model = record.get("ccr_model")
    if ccr_model is not None and not isinstance(ccr_model, str):
        raise ReleaseError("proof_ccr_model_invalid")
    result = ProofExpectation(
        repo=text_value(record.get("repo")), tag=text_value(record.get("tag")),
        release_revision=text_value(record.get("release_revision")),
        harness_revision=text_value(record.get("harness_revision")),
        platform_os=text_value(record.get("platform_os")), platform_arch=text_value(record.get("platform_arch")),
        opencode_model=text_value(record.get("opencode_model")), ccr_model=ccr_model,
        source_harness_sha256=digest_value(record.get("source_harness_sha256")),
        archive_sha256=digest_value(record.get("archive_sha256")),
        delegate_sha256=digest_value(record.get("delegate_sha256")),
        runner_sha256=digest_value(record.get("runner_sha256")),
        pueue_sha256=digest_value(record.get("pueue_sha256")),
        pueued_sha256=digest_value(record.get("pueued_sha256")),
    )
    validate_expectation(result)
    return result


def expectation(receipt: Receipt) -> ProofExpectation:
    invocation = receipt.invocation
    request = release_request(invocation)
    if file_sha(invocation.archive_receipt) != receipt.stage("archive").result.get("receipt_sha256"):
        raise ReleaseError("original_archive_receipt_changed", incomplete=True)
    archive = read_object(invocation.archive_receipt)
    facts = receipt.stage("preflight").result
    binaries = object_value(archive.get("binaries"))
    record: dict[str, object] = {
        "repo": request.repo, "tag": request.tag,
        "release_revision": facts["release_revision"], "harness_revision": facts["harness_revision"],
        "source_harness_sha256": facts["source_harness_sha256"],
        "platform_os": invocation.platform_os, "platform_arch": invocation.platform_arch,
        "opencode_model": request.opencode_model, "ccr_model": request.ccr_model,
        "archive_sha256": object_value(archive.get("artifact")).get("archive_sha256"),
    }
    for source, target in (("delegate", "delegate_sha256"), ("delegate-run", "runner_sha256"),
                           ("pueue", "pueue_sha256"), ("pueued", "pueued_sha256")):
        record[target] = object_value(binaries.get(source)).get("sha256")
    return expectation_from_record(record)


def read_public(receipt: Receipt) -> tuple[bytes, dict[str, object]]:
    stage = receipt.stage("project")
    expected = expectation_from_record(stage.intent.get("expectation"))
    data = read_bounded_regular_file(receipt.invocation.proof, MAX_PUBLIC_ZIP_BYTES)
    inspect_public_proof(data)
    validate_public_identity(data, expected)
    result = validate_public_proof(data, expected)
    facts: dict[str, object] = {
        "proof_sha256": file_sha(receipt.invocation.proof), "size": len(data),
        "status": result.get("status"), "case_results_verdict": result.get("case_results_verdict"),
        "pass_count": integer(result.get("pass_count", 0)),
        "failed_count": integer(result.get("failed_count", 1)),
        "unresolved_case_count": integer(result.get("unresolved_case_count", 1)),
    }
    if stage.state == "complete" and stage.result != facts:
        raise ReleaseError("public_proof_readback_changed", incomplete=True)
    _require_agy_model_bound(result, release_request(receipt.invocation).agy_model)
    return data, facts


def _require_agy_model_bound(result: dict[str, object], requested: str | None) -> None:
    """Bind a PASS verdict's AGY PASS cases to the maintainer's requested model.

    The canonical validator already requires recorded options on every PASS
    case; FAIL and neutral BLOCKED cases keep their own verdict path.
    """
    if result.get("case_results_verdict") != "PASS":
        return
    cases = result.get("cases")
    if not isinstance(cases, list):
        raise ReleaseError("agy_requested_model_unbound")
    for value in cases:
        if not agy_case_model_bound(object_value(value), requested):
            raise ReleaseError("agy_requested_model_unbound")


def project(receipt: Receipt) -> None:
    invocation = receipt.invocation
    old = receipt.stage("project")
    if old.state == "complete":
        read_public(receipt)
        return
    if old.state == "pending":
        expected = expectation(receipt)
        identity = object_value(receipt.stage("download").result.get("stage_identity"))
        receipt.intend("project", {"expectation": asdict(expected),
                                   "directory_identity": identity,
                                   "source_digests": projection_source_digests(receipt)})
    else:
        expected = expectation_from_record(old.intent.get("expectation"))
    intent = receipt.stage("project").intent
    sources = projection_source_digests(receipt)
    if intent.get("source_digests") != sources:
        raise ReleaseError("private_projection_sources_changed", incomplete=True)
    _stage_projection(receipt, expected, sources)
    if not invocation.proof.exists():
        if not (invocation.root / "private-stage" / "manifest.json").is_file():
            raise ReleaseError("private_projection_stage_incomplete", incomplete=True)
        build_public_proof(invocation.root / "private-stage", invocation.proof, expected)
    _, result = read_public(receipt)
    receipt.complete("project", result)


def _stage_projection(receipt: Receipt, expected: ProofExpectation, sources: dict[str, str]) -> None:
    invocation = receipt.invocation
    identity = object_value(receipt.stage("project").intent.get("directory_identity"))
    try:
        stage_evidence(
            invocation.aggregate, invocation.root / "private-stage",
            archive_receipt=invocation.archive_receipt, native_receipt=invocation.native_receipt,
            expected_tag=release_request(invocation).tag, expected_source_revision=expected.harness_revision,
            directory_identity=identity, expected_source_digests=sources,
        )
    except EvidenceError as error:
        if error.args == ("stage_source_digest_changed",):
            raise ReleaseError("private_projection_sources_changed", incomplete=True) from error
        raise


def projection_source_digests(receipt: Receipt) -> dict[str, str]:
    return {
        "aggregate-receipt.json": digest_value(receipt.stage("native").result.get("aggregate_sha256")),
        "archive-receipt.json": digest_value(receipt.stage("archive").result.get("receipt_sha256")),
        "native-acceptance-receipt.json": digest_value(receipt.stage("finalize").result.get("receipt_sha256")),
    }


def locally_promotable(receipt: Receipt) -> bool:
    request = release_request(receipt.invocation)
    result = receipt.stage("project").result
    return (receipt.stage("preflight").result.get("candidate_harness") is False
            and not request.no_submit
            and result.get("status") == "PASS" and result.get("case_results_verdict") == "PASS")


def read_candidate_proof(receipt: Receipt,
                         expected: CandidateProofExpectation) -> tuple[bytes, dict[str, object]]:
    """Validate the retained candidate proof against the claim's expectation; never reads scratch."""
    stage = receipt.stage("project")
    data = read_bounded_regular_file(receipt.invocation.proof, MAX_PUBLIC_ZIP_BYTES)
    validated = validate_candidate_proof(data, expected)
    statuses = [case.status for case in validated.cases]
    facts: dict[str, object] = {
        "proof_sha256": validated.public_zip_sha256, "size": len(data), "status": validated.status,
        "case_results_verdict": validated.case_results_verdict, "pass_count": statuses.count("PASS"),
        "failed_count": statuses.count("FAIL"), "unresolved_case_ids": list(validated.unresolved_case_ids),
    }
    if stage.state == "complete" and stage.result != facts:
        raise ReleaseError("public_proof_readback_changed", incomplete=True)
    return data, facts


def project_candidate(receipt: Receipt, expected: CandidateProofExpectation) -> None:
    """Stage the native run's aggregate and observed completion, then build its candidate proof once.

    The completion is the native child's natural Wait only; a case whose
    closure stayed unproven is projected as unresolved, never closed.
    """
    invocation = receipt.invocation
    if receipt.stage("project").state == "complete":
        read_candidate_proof(receipt, expected)
        return
    completion = observe_child_completion(receipt, "native")
    if completion.completed_sha256 != receipt.stage("native").result.get("completion_sha256"):
        raise ReleaseError("candidate_native_completion_changed", incomplete=True)
    sources = {AGGREGATE_RECEIPT_NAME: file_sha(invocation.aggregate),
               NATIVE_COMPLETION_NAME: sha256(canonical_json(completion_record(completion)))}
    if receipt.stage("project").state == "pending":
        identity = object_value(receipt.stage("download").result.get("stage_identity"))
        receipt.intend("project", {"claim_sha256": expected.claim_sha256, "directory_identity": identity,
                                   "source_digests": sources})
    intent = receipt.stage("project").intent
    if intent.get("source_digests") != sources or intent.get("claim_sha256") != expected.claim_sha256:
        raise ReleaseError("private_projection_sources_changed", incomplete=True)
    stage = invocation.root / "private-stage"
    try:
        stage_candidate_evidence(invocation.aggregate, stage, completion=completion, expectation=expected,
                                 directory_identity=object_value(intent.get("directory_identity")),
                                 expected_source_digests=sources)
    except EvidenceError as error:
        if error.args == ("stage_source_digest_changed",):
            raise ReleaseError("private_projection_sources_changed", incomplete=True) from error
        raise
    if not invocation.proof.exists():
        if not (stage / "manifest.json").is_file():
            raise ReleaseError("private_projection_stage_incomplete", incomplete=True)
        build_candidate_proof(stage, invocation.proof, expected)
    _, facts = read_candidate_proof(receipt, expected)
    receipt.complete("project", facts)
