"""Settle both owned build trees and write the terminal receipt exactly once.

Owner: this module, for the end of one build. It holds each tree's
DirectoryOwner pins until `settle` consumes them, one owner at a time, so a
failure settling one tree never leaves the other's pins live. A tree is removed
only when every child that could touch it is proven closed. When closure is
unproven (an unresolved ownership code or any unexpected exception) both trees
are retained as evidence. The receipt records what actually happened.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.release_verification.candidate_build_plan import STAGE
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.contract import set_failure
from scripts.release_verification.filesystem import write_receipt
from scripts.release_verification.published_contract import UNRESOLVED_OWNERSHIP_CODES


UNEXPECTED_ERROR_CODE = "build_unexpected_error"
RECEIPT_WRITE_FAILED_CODE = "build_receipt_write_failed"


@dataclass(frozen=True)
class BuildOwners:
    """The scratch tree (clone, extractions, npm output) and the candidate payload tree."""

    work: DirectoryOwner
    output: DirectoryOwner


def closure_unproven(failure: BaseException) -> bool:
    """A child's closure is unproven for a canonical code and for any unexpected exception."""
    return not isinstance(failure, VerificationError) or failure.code in UNRESOLVED_OWNERSHIP_CODES


def _apply(receipt: dict, label: str, operation: Callable[[], bool], disposition: str) -> bool:
    """Run one owner operation; record its actual disposition and any failure."""
    cleanup = receipt["cleanup"]
    try:
        operation()
    except (OSError, RuntimeError) as error:
        cleanup["dispositions"][label] = "failed"
        cleanup["errors"].append(f"{label} cleanup failed: {error}")
        return False
    cleanup["dispositions"][label] = disposition
    return True


def _record_outcome(receipt: dict, failure: BaseException | None, cleanup_failed: bool) -> None:
    if isinstance(failure, VerificationError):
        set_failure(receipt, failure.code, failure.stage)
    elif failure is not None:
        set_failure(receipt, UNEXPECTED_ERROR_CODE, STAGE)
        receipt["failure"]["exception"] = type(failure).__name__
    elif cleanup_failed:
        set_failure(receipt, "build_cleanup_failed", STAGE)
    else:
        receipt.update({"verdict": "pass", "stage": "complete"})


def _write_terminal_receipt(receipt: dict, receipt_path: Path) -> VerificationError | None:
    """Write the receipt once; a refusal keeps its code and a raw I/O failure becomes ours."""
    try:
        write_receipt(receipt_path, receipt)
    except VerificationError as error:
        return error
    except OSError:
        return VerificationError(RECEIPT_WRITE_FAILED_CODE, STAGE)
    return None


def settle(owners: BuildOwners, receipt: dict, receipt_path: Path, *,
           failure: BaseException | None) -> VerificationError | None:
    """Consume both owners' pins and write the terminal receipt.

    With a ``failure`` the caller re-raises it; cleanup and receipt problems are
    attached as notes. Without one, a cleanup or receipt problem is returned as
    the error to raise, so a successful result is never published half-final.
    """
    cleanup = receipt["cleanup"]
    cleanup.update(dispositions={}, errors=[])
    retain = failure is not None and closure_unproven(failure)
    if failure is None:
        work_settled = _apply(receipt, "scratch", owners.work.rollback, "removed")
        # A scratch that cannot be removed fails the build, so its payload is removed too.
        output_settled = (_apply(receipt, "payload", owners.output.release, "retained") if work_settled
                          else _apply(receipt, "payload", owners.output.rollback, "removed"))
        cleanup_failed = not (work_settled and output_settled)
    else:
        disposition = "retained" if retain else "removed"
        _apply(receipt, "payload", owners.output.release if retain else owners.output.rollback, disposition)
        _apply(receipt, "scratch", owners.work.release if retain else owners.work.rollback, disposition)
        cleanup_failed = bool(cleanup["errors"])
    cleanup["status"] = "failed" if cleanup["errors"] else ("retained" if retain else "settled")
    _record_outcome(receipt, failure, cleanup_failed)
    receipt_error = _write_terminal_receipt(receipt, receipt_path)
    problems = [*cleanup["errors"]]
    if receipt_error is not None:
        problems.append(f"build receipt was not written: {receipt_error.code}")
    if failure is not None:
        for problem in problems:
            failure.add_note(problem)
        return None
    if problems:
        result = VerificationError("build_cleanup_failed" if cleanup_failed else "build_receipt_unwritable",
                                   STAGE)
        for problem in problems:
            result.add_note(problem)
        return result
    return None
