"""The one-shot receipt and sanitized outcome of one maintainer candidate store command.

A command reserves a new receipt file before it reads any input or makes any
request, records at most one store append and writes the receipt once with
the actual outcome. The receipt is an observation of that one invocation: it
is never progress state, a retry journal or an approval, and nothing here
reads it back. The store branch stays the only state owner and
``candidate_store.append`` its only writer. The caller keeps the work
directory and the receipt.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
import json
import os
from pathlib import Path
import sys

from .assets import gh_environment
from .candidate_history import FindingRef
from .candidate_store_contract import AppendAmbiguous, AppendConfirmed, AppendOutcome, AppendRefused
from .candidate_store_replay import ReplayDenied
from .contract import VerificationError
from .filesystem import create_new_file, write_all
from .github import GitHubClient


# Exit codes; argparse owns 2 for a usage error.
EXIT_CONFIRMED, EXIT_FAILED, EXIT_AMBIGUOUS = 0, 1, 3


@dataclass(frozen=True)
class ReceiptIdentity:
    """The receipt kind, stage and receipt error identities one command owns."""

    kind: str
    stage: str
    unavailable_code: str
    write_failed_code: str


def refused(code: str, stage: str, findings: tuple[FindingRef, ...] = ()) -> dict[str, object]:
    return {"disposition": "refused", "code": code, "stage": stage,
            "findings": [{"code": item.code, "subject": item.subject} for item in findings]}


def refusal(error: VerificationError) -> dict[str, object]:
    """A refusal as raised; a ``ReplayDenied`` keeps its exact canonical findings."""
    return refused(error.code, error.stage, error.findings if isinstance(error, ReplayDenied) else ())


def disposition(outcome: AppendOutcome, *, stage: str) -> dict[str, object]:
    """The append outcome unchanged; an ambiguous outcome never reports its proposal as confirmed."""
    if isinstance(outcome, AppendConfirmed):
        return {"disposition": "confirmed", "parent": outcome.parent, "commit": outcome.commit,
                "kind": outcome.kind.value, "record_sha256s": list(outcome.record_sha256s)}
    if isinstance(outcome, AppendAmbiguous):
        return {"disposition": "ambiguous", "parent": outcome.parent, "proposed": outcome.proposed,
                "nonce": outcome.nonce, "record_sha256s": list(outcome.record_sha256s),
                "failure": outcome.failure}
    if isinstance(outcome, AppendRefused):
        return refused(outcome.reason, stage, outcome.findings)
    raise VerificationError("store_append_outcome_invalid", stage)


def report_unresolved(error: VerificationError) -> None:
    """The command's observation of a failed ref update; the outcome itself stays ambiguous."""
    print(f"store append unresolved: {error.code} ({error.stage})", file=sys.stderr)


def _write_receipt(descriptor: int, document: dict[str, object], *, failure_code: str) -> str | None:
    """Write and sync the reserved receipt once; return the failure code, never raise it."""
    data = (json.dumps(document, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n").encode()
    try:
        write_all(descriptor, data)
        os.fsync(descriptor)
    except OSError:
        return failure_code
    return None


def _close_receipt(descriptor: int) -> bool:
    """Close the reserved receipt once; a failed close is never retried and leaves the receipt unproven."""
    try:
        os.close(descriptor)
    except OSError:
        return False
    return True


def run_once(receipt: Path, identity: ReceiptIdentity, document: dict[str, object],
             record: Callable[[GitHubClient, dict[str, object]], dict[str, object]]) -> int:
    """Reserve ``receipt``, run ``record`` once and write what it actually observed.

    ``document`` holds the command's own receipt fields; ``record`` may fill
    them and returns the sanitized outcome. A failed write, sync or close is
    the command's one receipt fault; it never changes the outcome. Return 0
    confirmed; 1 refused, failed or a receipt fault after a confirmed append,
    which stays reported as confirmed; 3 ambiguous, with the work directory
    kept. An error ``record`` raises propagates unchanged, after the one close.
    """
    try:
        descriptor = create_new_file(receipt, identity.unavailable_code, stage=identity.stage)
    except VerificationError as error:
        print(json.dumps(refused(error.code, error.stage), sort_keys=True))
        return EXIT_FAILED
    # Stored gh credentials remain owned by gh; never read or export a token.
    client = GitHubClient({"commands": []}, gh_environment(auth_mode="local-config"))
    document = {"schema_version": 1, "kind": identity.kind, **document}
    try:
        outcome = record(client, document)
        document.update(outcome=outcome, commands=client.receipt["commands"])
        receipt_error = _write_receipt(descriptor, document, failure_code=identity.write_failed_code)
    except BaseException as error:
        if not _close_receipt(descriptor):
            error.add_note(f"{identity.write_failed_code}: the receipt close failed")
        raise
    if not _close_receipt(descriptor) and receipt_error is None:
        receipt_error = identity.write_failed_code
    print(json.dumps({**outcome, "receipt": str(receipt), "receipt_error": receipt_error}, sort_keys=True))
    if outcome["disposition"] == "ambiguous":
        return EXIT_AMBIGUOUS
    return EXIT_CONFIRMED if outcome["disposition"] == "confirmed" and receipt_error is None else EXIT_FAILED
