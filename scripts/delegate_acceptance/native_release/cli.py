"""Run native release acceptance locally, or observe an existing invocation."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys

from scripts.delegate_acceptance.directories.journal import (
    acquire_exclusive_file_lock, release_exclusive_file_lock,
)
from scripts.delegate_acceptance.evidence.bounded_files import BoundedReadError
from scripts.delegate_acceptance.native_evidence.contract import EvidenceError
from scripts.delegate_acceptance.native_evidence.public_contract import PublicProofError
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from .contract import CANONICAL_REPOSITORY, ReleaseError, ReleaseRequest
from .coordinator import execute, resume
from .preflight import require_python
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
    recovery = commands.add_parser("resume")
    recovery.add_argument("--receipt", required=True)
    return result


def _report(receipt: Receipt) -> int:
    print(json.dumps({"status": receipt.status, "reason": receipt.reason,
                      "receipt": str(receipt.path), "invocation_id": receipt.invocation.invocation_id,
                      "proof": str(receipt.invocation.proof) if receipt.stage("project").state == "complete" else None,
                      "native_status": receipt.stage("native").result.get("status"),
                      "native_proof": receipt.stage("project").result,
                      "asset": receipt.stage("upload").result, "validation": receipt.stage("dispatch").result},
                     sort_keys=True))
    return {"PROMOTABLE": 0, "LOCAL_ONLY": 0, "FAIL": 1, "BLOCKED": 2, "INCOMPLETE": 3}[receipt.status]


def _locked(args: argparse.Namespace, argv: list[str]) -> int:
    receipt: Receipt | None = None
    if args.command == "resume":
        receipt = load_receipt(Path(args.receipt))
        checkout, request = receipt.invocation.checkout, receipt.invocation.request
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
        print(f"Native release receipt: {receipt.path}", file=sys.stderr, flush=True)
        _execute(receipt, recovery=args.command == "resume")
        return _report(receipt)
    finally:
        release_exclusive_file_lock(lock_fd)


def _execute(receipt: Receipt, *, recovery: bool) -> None:
    try:
        resume(receipt) if recovery else execute(receipt)
    except KeyboardInterrupt:
        receipt.report("INCOMPLETE", "invocation_interrupted_native_ownership_retained")
    except (ReleaseError, VerificationError, EvidenceError, PublicProofError,
            BoundedReadError, OSError, ValueError, RuntimeError) as error:
        native_claimed = receipt.stage("native").state != "pending"
        incomplete = isinstance(error, ReleaseError) and error.incomplete
        submission_intended = any(
            receipt.stage(name).state == "intended" for name in ("upload", "dispatch", "watch")
        )
        status = "INCOMPLETE" if incomplete or submission_intended or (
            native_claimed and receipt.stage("project").state != "complete"
        ) else "BLOCKED"
        code = (error.diagnostic if isinstance(error, ReleaseError)
                else error.code if isinstance(error, VerificationError)
                else "release_evidence_or_io_invalid")
        reason = (f"submission_intent_unresolved_resume_required:{code}"
                  if submission_intended else code)
        receipt.report(status, reason)


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
