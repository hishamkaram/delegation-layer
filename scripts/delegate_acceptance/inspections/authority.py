"""Observe the canonical inspection exit authority without queue substitution."""

from __future__ import annotations

from datetime import datetime
from scripts.delegate_acceptance.evidence.records import _read_control_record
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.lifetime.evidence import _validate_managed_inspection_lifetime
from scripts.delegate_acceptance.lifetime.unlaunched import _auxiliary_lifetime_unlaunched
from pathlib import Path


def _inspection_lifetime_control_started(directory: Path) -> bool:
    """Detect any managed lifetime journal before choosing historical fallback."""
    try:
        for entry in directory.iterdir():
            if entry.name.startswith("lifetime-") and entry.name.endswith(".json"):
                return True
    except OSError as error:
        raise AcceptanceFailure("cannot inspect managed inspection lifetime records") from error
    return False


def _inspection_managed_authority_started(directory: Path) -> bool:
    """Return whether the inspection published a lifecycle authority.

    The immutable request is admission intent, and a proof reference is a
    task-local pointer to already-materialized facts. Neither creates a local
    worker or a managed scope. Submission, receipt, start, result, completion,
    observation, upgrade, or lifetime records do establish that the operation
    advanced into a lifecycle that must have a closure proof.
    """
    lifecycle_records = (
        "submission.json",
        "receipt.json",
        "start.json",
        "result.json",
        "completion.json",
        "worker-observation.json",
        "worker-upgrade.json",
    )
    for name in lifecycle_records:
        try:
            (directory / name).lstat()
        except FileNotFoundError:
            continue
        except OSError as error:
            raise AcceptanceFailure(
                "cannot inspect managed inspection lifecycle records") from error
        return True
    return _inspection_lifetime_control_started(directory)


def _validate_unsubmitted_inspection_records(directory: Path, task: str) -> None:
    """Allow only intent and an optional opaque proof reference before submit."""
    allowed = {"request.json", "proof-reference.json"}
    try:
        entries = tuple(directory.iterdir())
    except OSError as error:
        raise AcceptanceFailure(
            "cannot inspect unsubmitted managed inspection records") from error
    unexpected = sorted(entry.name for entry in entries if entry.name not in allowed)
    require(not unexpected,
            f"unsubmitted inspection has unexpected records for {task}: " +
            ", ".join(unexpected))
    proof_reference = directory / "proof-reference.json"
    if proof_reference.exists() or proof_reference.is_symlink():
        _read_control_record(proof_reference, f"inspection proof reference for {task}")


def _inspection_exit_authority(directory: Path, root: str, task: str,
                               binding: dict[str, object], request_digest: str,
                               request_created: tuple[datetime, int],
                               request_deadline: str,
                               deadline: tuple[datetime, int]) -> dict[str, object]:
    """Choose the sole durable exit-proof policy for one inspection journal.

    Fresh inspections prove the child Wait and independently empty scope
    through the guardian. Markerless journals retain the historical queue
    observation only when no managed lifetime record was ever started. Once any
    lifetime record exists, incomplete evidence is a managed evidence fault and
    cannot be replaced by a terminal queue row.
    """
    lifetime_started = _inspection_lifetime_control_started(directory)
    managed = (binding.get("worker_exit_proof") == "guardian-wait-v1" or lifetime_started)
    if not managed:
        return {"mode": "queue", "managed_lifetime": None}
    if not lifetime_started:
        return {"mode": "guardian-unstarted", "managed_lifetime": None}
    if (directory / "lifetime-unlaunched.json").exists():
        names = {entry.name for entry in directory.iterdir()}
        fence = _auxiliary_lifetime_unlaunched(
            directory, root, task, request_digest, request_deadline, names, "inspection")
        return {"mode": "guardian-unlaunched", "managed_lifetime": fence}
    lifetime = _validate_managed_inspection_lifetime(
        directory, root, task, request_digest, request_created, request_deadline, deadline)
    return {"mode": "guardian", "managed_lifetime": lifetime}
