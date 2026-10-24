"""Run native release or development-candidate acceptance, publish a frozen candidate, or observe an invocation."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import re
import sys

from scripts.delegate_acceptance.directories.journal import (
    acquire_exclusive_file_lock, release_exclusive_file_lock,
)
from scripts.delegate_acceptance.evidence.bounded_files import BoundedReadError, read_bounded_regular_file
from scripts.delegate_acceptance.native_evidence.contract import EvidenceError
from scripts.delegate_acceptance.native_evidence.public_contract import PublicProofError
from scripts.delegate_acceptance.release_verification.candidate_manifest import MAX_LOCATOR_BYTES, parse_locator
from scripts.delegate_acceptance.release_verification.candidate_publication_contract import CandidateReference
from scripts.delegate_acceptance.release_verification.candidate_store_contract import StoreLocation
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.published_contract import UNRESOLVED_OWNERSHIP_CODES
from .contract import (
    CANONICAL_REPOSITORY, PRODUCTION_STORE, PUBLICATION_STAGES, CandidateRequest, PublicationRequest, ReleaseError,
    ReleaseRequest, Request,
)
from .coordinator import execute, github_client, resume
from .preflight import require_python
from .publication import execute_publication, publication_status, resume_publication
from .receipt import Receipt, lock_name
from .storage import create_receipt, load_receipt, state_base


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)
    run = commands.add_parser("run")
    run.add_argument("--tag", required=True)
    run.add_argument("--opencode-model", required=True)
    run.add_argument("--ccr-model")
    run.add_argument("--agy-model")
    run.add_argument("--no-submit", action="store_true")
    run.add_argument("--repo", default=CANONICAL_REPOSITORY)
    candidate = commands.add_parser("run-candidate")
    candidate.add_argument("--locator", required=True)
    candidate.add_argument("--store-branch", required=True)
    candidate.add_argument("--store-genesis-commit", required=True)
    candidate.add_argument("--store-marker-blob", required=True)
    candidate.add_argument("--attempt", required=True, type=int)
    candidate.add_argument("--opencode-model", required=True)
    candidate.add_argument("--ccr-model")
    candidate.add_argument("--agy-model")
    recovery = commands.add_parser("resume")
    recovery.add_argument("--receipt", required=True)
    publication = commands.add_parser("publish-candidate")
    publication.add_argument("--candidate-id", required=True)
    publication.add_argument("--freeze-commit", required=True)
    publication.add_argument("--merged-commit", required=True)
    publication.add_argument("--readiness-run-id", required=True)
    publication.add_argument("--readiness-run-attempt", required=True)
    publication.add_argument("--resume-published", required=True, choices=("true", "false"))
    observation = commands.add_parser("resume-publication")
    observation.add_argument("--receipt", required=True)
    return result


def _report(receipt: Receipt) -> int:
    request = receipt.invocation.request
    print(json.dumps(_candidate_report(receipt, request) if isinstance(request, CandidateRequest) else {
        "status": receipt.status, "reason": receipt.reason,
        "receipt": str(receipt.path), "invocation_id": receipt.invocation.invocation_id,
        "proof": str(receipt.invocation.proof) if receipt.stage("project").state == "complete" else None,
        "native_status": receipt.stage("native").result.get("status"),
        "native_proof": receipt.stage("project").result,
        "asset": receipt.stage("upload").result, "validation": receipt.stage("dispatch").result,
    }, sort_keys=True))
    return {"PROMOTABLE": 0, "LOCAL_ONLY": 0, "FAIL": 1, "BLOCKED": 2, "INCOMPLETE": 3}[receipt.status]


def _candidate_report(receipt: Receipt, request: CandidateRequest) -> dict[str, object]:
    """Candidate facts only; a candidate has no upload, dispatch or watch stage to read."""
    invocation = receipt.invocation
    stages = {name: {"state": receipt.stage(name).state, "result": receipt.stage(name).result}
              for name in ("claim", "record", "cleanup")}
    return {"status": receipt.status, "reason": receipt.reason, "receipt": str(receipt.path),
            "invocation_id": invocation.invocation_id, "candidate_id": request.candidate_id,
            "attempt": request.attempt, "platform": {"os": invocation.platform_os, "arch": invocation.platform_arch},
            "proof": str(invocation.proof) if receipt.stage("project").state == "complete" else None,
            "native_proof": receipt.stage("project").result, **stages}


def _publication_report(receipt: Receipt, request: PublicationRequest) -> int:
    """Publication facts only; the exit code follows the receipt's finalized status."""
    stages = {name: {"state": receipt.stage(name).state, "result": receipt.stage(name).result}
              for name in PUBLICATION_STAGES}
    print(json.dumps({"status": receipt.status, "reason": receipt.reason, "receipt": str(receipt.path),
                      "invocation_id": receipt.invocation.invocation_id, "candidate_id": request.candidate_id,
                      "stages": stages}, sort_keys=True))
    return {"PUBLISHED": 0, "BLOCKED": 2, "INCOMPLETE": 3}[receipt.status]


def _run_identity(value: str) -> int:
    """One canonical positive decimal: no sign, space, leading zero or digits beyond GitHub's 64-bit IDs."""
    if re.fullmatch(r"[1-9][0-9]{0,17}", value) is None:
        raise ReleaseError("publication_request_invalid")
    return int(value)


def _publication(args: argparse.Namespace) -> PublicationRequest:
    """The production-store request of the explicit candidate, F, M, readiness attempt and resume choice."""
    reference = CandidateReference(candidate_id=args.candidate_id, freeze_commit=args.freeze_commit,
                                   merged_commit=args.merged_commit)
    return PublicationRequest(store=PRODUCTION_STORE, reference=reference,
                              readiness_run_id=_run_identity(args.readiness_run_id),
                              readiness_run_attempt=_run_identity(args.readiness_run_attempt),
                              resume_published=args.resume_published == "true")


def _candidate(args: argparse.Namespace) -> CandidateRequest:
    locator = parse_locator(read_bounded_regular_file(Path(args.locator), MAX_LOCATOR_BYTES))
    store = StoreLocation(repository=locator.repository, branch=args.store_branch,
                          genesis_commit=args.store_genesis_commit, marker_blob=args.store_marker_blob)
    return CandidateRequest(locator=locator, store=store, attempt=args.attempt,
                            opencode_model=args.opencode_model, ccr_model=args.ccr_model,
                            agy_model=args.agy_model)


def _locked(args: argparse.Namespace, argv: list[str]) -> int:
    receipt: Receipt | None = None
    request: Request
    if args.command == "resume":
        receipt = load_receipt(Path(args.receipt))
        checkout, request = receipt.invocation.checkout, receipt.invocation.request
        # This command resumes native acceptance only; a publication has its own resume owner.
        if not isinstance(request, (ReleaseRequest, CandidateRequest)):
            raise ReleaseError("native_acceptance_request_required")
    elif args.command == "resume-publication":
        receipt = load_receipt(Path(args.receipt))
        checkout, request = receipt.invocation.checkout, receipt.invocation.request
        # Publication resume observes only; a native receipt keeps its own resume owner.
        if not isinstance(request, PublicationRequest):
            raise ReleaseError("publication_request_required")
    elif args.command == "publish-candidate":
        checkout, request = Path(__file__).resolve().parents[3], _publication(args)
    elif args.command == "run-candidate":
        checkout, request = Path(__file__).resolve().parents[3], _candidate(args)
    else:
        checkout = Path(__file__).resolve().parents[3]
        request = ReleaseRequest(repo=args.repo, tag=args.tag, opencode_model=args.opencode_model,
                                 ccr_model=args.ccr_model, no_submit=args.no_submit,
                                 agy_model=args.agy_model)
    base = state_base(checkout)
    base.mkdir(mode=0o700, parents=True, exist_ok=True)
    lock_fd = acquire_exclusive_file_lock(base / lock_name(request))
    try:
        if receipt is not None:
            receipt = load_receipt(receipt.path)
        else:
            receipt = create_receipt(checkout, request, argv)
        if isinstance(request, PublicationRequest):
            print(f"Candidate publication receipt: {receipt.path}", file=sys.stderr, flush=True)
            unsaved = _publish(receipt, recovery=args.command == "resume-publication")
            if unsaved is not None:
                return _unpersisted_report(receipt, request, unsaved)
            return _publication_report(receipt, request)
        print(f"Native release receipt: {receipt.path}", file=sys.stderr, flush=True)
        unsaved = _execute(receipt, recovery=args.command == "resume")
        if unsaved is not None:
            return _unpersisted_report(receipt, request, unsaved)
        return _report(receipt)
    finally:
        release_exclusive_file_lock(lock_fd)


def _execute(receipt: Receipt, *, recovery: bool) -> str | None:
    """Run native acceptance; return the fault whose outcome could not be persisted, else None."""
    try:
        resume(receipt) if recovery else execute(receipt)
    except KeyboardInterrupt:
        return _record_fault(receipt, "INCOMPLETE", "invocation_interrupted_native_ownership_retained")
    except (ReleaseError, VerificationError, EvidenceError, PublicProofError,
            BoundedReadError, OSError, ValueError, RuntimeError) as error:
        if isinstance(receipt.invocation.request, CandidateRequest):
            return _record_fault(receipt, _candidate_status(receipt, error), _diagnostic(error))
        native_claimed = receipt.stage("native").state != "pending"
        incomplete = (isinstance(error, ReleaseError) and error.incomplete) or _control_closure_unproven(error)
        submission_intended = any(
            receipt.stage(name).state == "intended" for name in ("upload", "dispatch", "watch")
        )
        status = "INCOMPLETE" if incomplete or submission_intended or (
            native_claimed and receipt.stage("project").state != "complete"
        ) else "BLOCKED"
        code = _diagnostic(error)
        reason = (f"submission_intent_unresolved_resume_required:{code}"
                  if submission_intended else code)
        return _record_fault(receipt, status, reason)
    return None


def _publish(receipt: Receipt, *, recovery: bool) -> str | None:
    """Run the publication; return the fault whose outcome could not be persisted, else None."""
    try:
        (resume_publication if recovery else execute_publication)(receipt, github_client())
    except KeyboardInterrupt:
        return _record_fault(receipt, publication_status(receipt), "publication_interrupted_resume_observes_only")
    except (ReleaseError, VerificationError, EvidenceError, PublicProofError,
            BoundedReadError, OSError, ValueError, RuntimeError) as error:
        return _record_fault(receipt, publication_status(receipt), _diagnostic(error))
    return None


def _record_fault(receipt: Receipt, status: str, reason: str) -> str | None:
    """Persist the caller's fault outcome once; a save that fails too is returned for reporting, never retried."""
    try:
        receipt.report(status, reason)
    except OSError:
        return reason
    return None


def _unpersisted_report(receipt: Receipt, request: Request, fault: str) -> int:
    """A truthful nonzero outcome when the receipt could not record it; in-memory progress is not durable.

    ``report`` assigned the selected status before its save failed, so that status
    is reported as an unpersisted observation with the last receipt path; the file
    keeps whatever its last successful save wrote.
    """
    reason = ("publication_receipt_not_persisted" if isinstance(request, PublicationRequest)
              else "release_receipt_unavailable")
    report: dict[str, object] = {"status": receipt.status, "reason": reason, "fault": fault,
                                 "receipt": str(receipt.path), "receipt_persisted": False,
                                 "invocation_id": receipt.invocation.invocation_id}
    if isinstance(request, (CandidateRequest, PublicationRequest)):
        report["candidate_id"] = request.candidate_id
    print(json.dumps(report, sort_keys=True))
    return {"BLOCKED": 2, "INCOMPLETE": 3}[receipt.status]


def _diagnostic(error: Exception) -> str:
    return (error.diagnostic if isinstance(error, ReleaseError)
            else error.code if isinstance(error, VerificationError)
            else "release_evidence_or_io_invalid")


def _control_closure_unproven(error: Exception) -> bool:
    """A control child whose closure the canonical runner could not prove; never a definite refusal."""
    return isinstance(error, VerificationError) and error.code in UNRESOLVED_OWNERSHIP_CODES


def _candidate_status(receipt: Receipt, error: Exception) -> str:
    """BLOCKED only for a definite refusal before any paid run; anything later or unknown is INCOMPLETE."""
    if (isinstance(error, ReleaseError) and error.incomplete) or _control_closure_unproven(error):
        return "INCOMPLETE"
    if any(receipt.stage(name).state == "intended" for name in ("claim", "native", "project", "record", "cleanup")):
        return "INCOMPLETE"
    if receipt.stage("claim").result.get("disposition") == "confirmed":
        return "INCOMPLETE"
    return "BLOCKED"


def main(argv: list[str] | None = None) -> int:
    arguments = sys.argv[1:] if argv is None else argv
    args = parser().parse_args(arguments)
    try:
        require_python()
        return _locked(args, arguments)
    except (ReleaseError, VerificationError, PublicProofError, BoundedReadError,
            OSError, ValueError, RuntimeError) as error:
        code = error.code if isinstance(error, (ReleaseError, VerificationError)) else "release_receipt_unavailable"
        print(json.dumps({"status": "BLOCKED", "reason": code}, sort_keys=True))
        return 2
