"""Bind immutable public bytes to verified archive and private native receipts."""

from __future__ import annotations

from dataclasses import asdict

from scripts.delegate_acceptance.evidence.bounded_files import read_bounded_regular_file
from scripts.delegate_acceptance.native_evidence.public_contract import (
    MAX_PUBLIC_ZIP_BYTES, ProofExpectation, validate_expectation,
)
from scripts.delegate_acceptance.native_evidence.contract import EvidenceError
from scripts.delegate_acceptance.native_evidence.public_projection import build_public_proof
from scripts.delegate_acceptance.native_evidence.public_validation import (
    inspect_public_proof, validate_public_identity, validate_public_proof,
)
from scripts.delegate_acceptance.native_evidence.staging import stage_evidence
from .contract import ReleaseError, digest_value, integer, object_value, text_value
from .execution import file_sha, read_object
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
    if file_sha(invocation.archive_receipt) != receipt.stage("archive").result.get("receipt_sha256"):
        raise ReleaseError("original_archive_receipt_changed", incomplete=True)
    archive = read_object(invocation.archive_receipt)
    facts = receipt.stage("preflight").result
    binaries = object_value(archive.get("binaries"))
    record: dict[str, object] = {
        "repo": invocation.request.repo, "tag": invocation.request.tag,
        "release_revision": facts["release_revision"], "harness_revision": facts["harness_revision"],
        "source_harness_sha256": facts["source_harness_sha256"],
        "platform_os": invocation.platform_os, "platform_arch": invocation.platform_arch,
        "opencode_model": invocation.request.opencode_model, "ccr_model": invocation.request.ccr_model,
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
    return data, facts


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
            expected_tag=invocation.request.tag, expected_source_revision=expected.harness_revision,
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
    result = receipt.stage("project").result
    return (receipt.stage("preflight").result.get("candidate_harness") is False
            and not receipt.invocation.request.no_submit
            and result.get("status") == "PASS" and result.get("case_results_verdict") == "PASS")
