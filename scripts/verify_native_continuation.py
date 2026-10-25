#!/usr/bin/env python3
"""Verify supplemental native-continuation evidence against one candidate attempt's retained native proof.

The candidate coordinator owns the claim and proof association. This edge only
reads that association from the candidate receipt, through the coordinator's own
readers, then hands the supplemental inputs to their validator and requires both
to name one canonical aggregate. Nothing is written, deleted, compacted or
launched, so a refused or repeated verification leaves every input unchanged.
The receipt is read at its original retained path; relocated copies are refused.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import sys
from typing import Mapping

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.evidence.bounded_files import BoundedReadError
from scripts.delegate_acceptance.native_evidence.candidate_proof import completion_record, validate_candidate_proof
from scripts.delegate_acceptance.native_evidence.contract import AGGREGATE_RECEIPT_NAME, EvidenceError
from scripts.delegate_acceptance.native_evidence.public_contract import (
    CASE_IDS, CandidateProofExpectation, PublicProofError, ValidatedCandidateProof, canonical_json, sha256,
)
from scripts.delegate_acceptance.native_evidence.public_stage import NATIVE_COMPLETION_NAME
from scripts.delegate_acceptance.native_evidence.supplemental import validate_supplemental
from scripts.delegate_acceptance.native_release.candidate import candidate_expectation
from scripts.delegate_acceptance.native_release.contract import (
    ReleaseError, candidate_request, receipt_stages, text_value,
)
from scripts.delegate_acceptance.native_release.projection import read_candidate_proof
from scripts.delegate_acceptance.native_release.receipt import Receipt
from scripts.delegate_acceptance.release_verification.candidate_attempts import parse_claim
from scripts.delegate_acceptance.release_verification.contract import VerificationError


class CandidateAssociationError(Exception):
    """The receipt, claim and proof do not describe one completed local native proof."""


def _require(condition: bool, code: str) -> None:
    if not condition:
        raise CandidateAssociationError(code)


def _require_completed_receipt(receipt: Receipt, candidate_id: str) -> CandidateProofExpectation:
    """Bind the caller's candidate to a completed LOCAL_ONLY receipt and return its claim's expectation."""
    invocation = receipt.invocation
    request = candidate_request(invocation)
    _require(request.candidate_id == candidate_id, "candidate_id_mismatch")
    _require(receipt.status == "LOCAL_ONLY", "candidate_receipt_not_local_only")
    for name in receipt_stages(request):
        _require(receipt.stage(name).state == "complete", f"candidate_stage_not_complete:{name}")
    for name in ("claim", "record"):
        # A readback proves the record is stored but not the store command's closure.
        _require(receipt.stage(name).result.get("disposition") == "confirmed",
                 f"candidate_{name}_not_confirmed")
    expected = candidate_expectation(receipt, request)
    # The expectation validated these claim bytes but does not bind the request's attempt to them.
    claim = parse_claim(text_value(receipt.stage("claim").intent.get("claim")).encode("utf-8"))
    _require(claim.attempt == request.attempt, "candidate_claim_attempt_mismatch")
    # The validating host's platform is deliberately not consulted: the receipt records its own.
    _require((expected.invocation_id, expected.platform_os, expected.platform_arch)
             == (invocation.invocation_id, invocation.platform_os, invocation.platform_arch),
             "candidate_claim_invocation_mismatch")
    return expected


def _require_passing(validated: ValidatedCandidateProof) -> None:
    _require(validated.status == "PASS" and validated.case_results_verdict == "PASS", "candidate_proof_not_pass")
    _require(tuple(case.case_id for case in validated.cases) == CASE_IDS
             and all(case.status == "PASS" for case in validated.cases), "candidate_proof_cases_not_all_pass")
    _require(validated.unresolved_case_ids == (), "candidate_proof_unresolved_cases")


def _require_stage_associations(receipt: Receipt, expected: CandidateProofExpectation,
                                validated: ValidatedCandidateProof) -> None:
    """Each stage names the facts the validated proof carries; the native stage holds no aggregate digest."""
    native = receipt.stage("native").result
    _require(type(native.get("exit_code")) is int and native["exit_code"] == 0, "candidate_native_exit_nonzero")
    _require(native.get("completion_sha256") == validated.completion.completed_sha256,
             "candidate_native_completion_mismatch")
    intent = receipt.stage("project").intent
    sources = intent.get("source_digests")
    _require(intent.get("claim_sha256") == expected.claim_sha256, "candidate_project_claim_mismatch")
    _require(isinstance(sources, dict) and sources.get(AGGREGATE_RECEIPT_NAME) == validated.aggregate_sha256,
             "candidate_project_aggregate_mismatch")
    _require(sources.get(NATIVE_COMPLETION_NAME) == sha256(canonical_json(completion_record(validated.completion))),
             "candidate_project_completion_mismatch")
    _require(receipt.stage("record").intent.get("proof_sha256") == validated.public_zip_sha256,
             "candidate_record_proof_mismatch")


def verify_native_continuation(*, candidate_receipt: Path, candidate_id: str, supplemental_root: Path,
                               canonical_aggregate: Path, source_revision: str | None,
                               candidate_delegate: Path, pi_control: Path, environment: Mapping[str, str],
                               checkout: Path) -> dict[str, object]:
    """Return the supplemental summary extended with the candidate it was proven for, or refuse."""
    receipt = Receipt.load(Path(os.path.abspath(candidate_receipt)))
    expected = _require_completed_receipt(receipt, candidate_id)
    data, _ = read_candidate_proof(receipt, expected)
    validated = validate_candidate_proof(data, expected)
    _require_passing(validated)
    _require_stage_associations(receipt, expected, validated)
    summary = validate_supplemental(
        supplemental_root=supplemental_root, canonical_aggregate=canonical_aggregate,
        source_revision=source_revision, candidate_delegate=candidate_delegate, pi_control=pi_control,
        environment=environment, checkout=checkout)
    _require(summary["canonical_aggregate_sha256"] == validated.aggregate_sha256,
             "canonical_aggregate_not_candidate_aggregate")
    return {**summary, "candidate_id": expected.candidate_id,
            "attempt": candidate_request(receipt.invocation).attempt, "claim_sha256": expected.claim_sha256}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--supplemental-root", required=True)
    parser.add_argument("--canonical-aggregate", required=True)
    parser.add_argument("--source-revision", required=True)
    parser.add_argument("--candidate-delegate", required=True)
    parser.add_argument("--pi-control", required=True)
    parser.add_argument("--candidate-receipt", required=True)
    parser.add_argument("--candidate-id", required=True)
    args = parser.parse_args(argv)
    try:
        summary = verify_native_continuation(
            candidate_receipt=Path(args.candidate_receipt), candidate_id=args.candidate_id,
            supplemental_root=Path(args.supplemental_root), canonical_aggregate=Path(args.canonical_aggregate),
            source_revision=args.source_revision, candidate_delegate=Path(args.candidate_delegate),
            pi_control=Path(args.pi_control), environment=os.environ,
            checkout=Path(__file__).resolve().parents[1])
    except ReleaseError as error:
        print(f"native continuation verification failed: {error.diagnostic}", file=sys.stderr)
        return 1
    except (CandidateAssociationError, EvidenceError, PublicProofError, VerificationError, BoundedReadError,
            OSError, ValueError) as error:
        print(f"native continuation verification failed: {error}", file=sys.stderr)
        return 1
    print(json.dumps(summary, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
