"""Composed native acceptance behavior: lifecycle."""

from __future__ import annotations


import hashlib
import json
import os
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from scripts.delegate_acceptance.directories.finalization import finalize_acceptance_artifacts
from scripts.delegate_acceptance.evidence.bounded_files import BoundedReadError, read_bounded_regular_file
from scripts.delegate_acceptance.evidence.files import digest, write_bytes, write_json
from scripts.delegate_acceptance.evidence.records import status_jobs
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.lifetime.closure import validate_managed_lifetime_closure
from scripts.delegate_acceptance.lifetime.identity import _lifetime_record_name
from scripts.delegate_acceptance.native_evidence.contract import MAX_INPUT_BYTES
from scripts.delegate_acceptance.native_evidence.lifetime_closure import (
    LIFETIME_CLOSURE_KIND,
    LIFETIME_CLOSURE_SCHEMA_VERSION,
    lifetime_closure_error,
)
from scripts.delegate_acceptance.native.task_ops import ModelDiscoveryCleanupOps
from scripts.delegate_acceptance.native.timeouts import TimeoutLeaseOwner
from scripts.delegate_acceptance.supervisor.configuration import pueue_command
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps


SUCCESS_CANDIDATE_NAME = "success-candidate.json"


@dataclass(frozen=True)
class SuccessCandidate:
    """The one private success candidate this owner commits before cleanup.

    It is recorded as unconfirmed before any byte is written and becomes
    confirmed only after the create-once write returned and a bounded readback
    matched the receipt. An unconfirmed candidate is sticky: every later
    finalization of this owner retains all owned state and raw captures.
    """
    sha256: str | None
    confirmed: bool


def _read_candidate(output: Path) -> bytes:
    """Read the committed candidate as one bounded regular file."""
    try:
        return read_bounded_regular_file(output / SUCCESS_CANDIDATE_NAME, MAX_INPUT_BYTES)
    except BoundedReadError as error:
        raise AcceptanceFailure("acceptance success candidate is unreadable: " + error.reason.value) from error


def commit_success_candidate(owner) -> bool:
    """Durably commit the complete success receipt before any authorized deletion."""
    acceptance = owner
    candidate = acceptance.success_candidate
    try:
        if candidate is not None:
            # Only this owner's confirmed bytes may be reused, after readback.
            require(candidate.confirmed, "acceptance success candidate was never confirmed")
            require(hashlib.sha256(_read_candidate(acceptance.output)).hexdigest() == candidate.sha256,
                    "acceptance success candidate changed")
            return True
        receipt = acceptance.success_receipt
        require(isinstance(receipt, dict), "acceptance success receipt is unavailable")
        acceptance.success_candidate = SuccessCandidate(sha256=None, confirmed=False)
        write_json(acceptance.output / SUCCESS_CANDIDATE_NAME, receipt)
        data = _read_candidate(acceptance.output)
        require(json.loads(data) == receipt, "acceptance success candidate does not match its receipt")
    except (AcceptanceFailure, OSError, ValueError) as error:
        # Reported, never retried: matching bytes after a failed write are not durability proof.
        print("FAIL acceptance-native: success candidate was not committed (" +
              type(error).__name__ + "); owned state and raw captures are retained",
              file=sys.stderr, flush=True)
        return False
    acceptance.success_candidate = SuccessCandidate(
        sha256=hashlib.sha256(data).hexdigest(), confirmed=True)
    return True


def _cleanup_timeout_leases(owner) -> bool:
    acceptance = owner
    timeout_state = getattr(acceptance, "timeout_state", None)
    if not isinstance(timeout_state, TimeoutLeaseOwner):
        return True
    return timeout_state.cleanup()

def finalize(owner, success: bool, *, blocked: bool = False) -> bool:
    """Finalize the run, retaining genuine-failure state for diagnosis."""
    acceptance = owner
    failure_diagnostics_written = False
    if not success:
        # Diagnostics are independent evidence.  Persist them even when
        # supervisor shutdown cannot complete; cleanup must still run.
        failure_diagnostics_written = acceptance._write_failure_diagnostics()
    if not acceptance.closed:
        try:
            acceptance.safe_shutdown()
        except BaseException:
            pass
    leases_clean = True
    if acceptance.closed:
        leases_clean = acceptance._cleanup_timeout_leases()
    # Commit only where cleanup could delete: an unclosed supervisor retains everything.
    candidate_committed = commit_success_candidate(acceptance) if success and acceptance.closed else True
    candidate = acceptance.success_candidate
    # A candidate that was attempted but never confirmed keeps every source.
    retain_all = not candidate_committed or (candidate is not None and not candidate.confirmed)
    success = success and candidate_committed
    # Genuine failures keep the authoritative inventory, not a selection: an
    # owned child is deleted even under a retained parent.
    retain_sources = retain_all or (not success and not blocked)
    retained_owned_directories = acceptance.owned_directories if retain_sources else ()
    artifacts_clean = finalize_acceptance_artifacts(
        acceptance.output,
        acceptance.closed,
        acceptance.owned_directories,
        success,
        remove_failed_process_captures=failure_diagnostics_written and not retain_sources,
        raw_process_captures_token=acceptance.processes.capture_root,
        retained_owned_directories=retained_owned_directories,
    )
    return leases_clean and candidate_committed and artifacts_clean

def publish_success(owner, *, expected_gate: str = "acceptance-native") -> None:
    """Publish exactly the confirmed candidate bytes after cleanup succeeded.

    The caller names its own gate; the candidate never chooses its expectation.
    """
    acceptance = owner
    require(isinstance(expected_gate, str) and bool(expected_gate),
            "acceptance success gate expectation is invalid")
    candidate = acceptance.success_candidate
    require(candidate is not None and candidate.confirmed,
            "acceptance success candidate was not committed")
    data = _read_candidate(acceptance.output)
    require(hashlib.sha256(data).hexdigest() == candidate.sha256,
            "acceptance success candidate changed")
    receipt = json.loads(data)
    require(isinstance(receipt, dict) and receipt.get("gate") == expected_gate and
            receipt.get("status") == "passed",
            "acceptance success candidate is not a passed native receipt")
    write_bytes(acceptance.output / "success.json", data)

def _record_sha256(directory: Path, owner_kind: str, record: str) -> str | None:
    path = directory / _lifetime_record_name(owner_kind, record)
    return digest(path) if path.is_file() and not path.is_symlink() else None


def _lifetime_facts(directory: Path, owner_kind: str, record: object) -> dict[str, object]:
    """Keep sanitized closure facts only: no PID, boot, environment, or argv."""
    require(isinstance(record, dict), "managed lifetime closure record is malformed")
    termination, wait = record.get("termination"), record.get("worker_wait")
    termination = termination if isinstance(termination, dict) else {}
    wait = wait if isinstance(wait, dict) else {}
    return {
        "cause": termination.get("cause"), "scope_empty": termination.get("scope_empty"),
        "launch_count": termination.get("launch_count"), "wait_result": wait.get("result"),
        "recovered": wait.get("result") == "absent",
        "registration_sha256": _record_sha256(directory, owner_kind, "registration"),
        "termination_sha256": _record_sha256(directory, owner_kind, "termination"),
        "wait_sha256": _record_sha256(directory, owner_kind, "worker-wait"),
    }


def _root_closure(state: Path, task_ids: set[str]) -> dict[str, object]:
    """Project one state root's canonical managed-lifetime closure."""
    closure = validate_managed_lifetime_closure(state, task_ids)
    tasks, inspections = closure.get("tasks"), closure.get("inspections")
    require(isinstance(tasks, dict) and isinstance(inspections, dict),
            "managed lifetime closure is malformed")
    parent = state / "inspections"
    journals = {entry.name for entry in parent.iterdir()} if parent.is_dir() else set()
    # The scanner skips queue-owned and unsubmitted inspections; a managed
    # success must prove the lifetime of every inspection it journaled.
    require(set(inspections) == journals, "managed inspection lifetime coverage is not exact")
    return {
        "root_id": closure.get("root_id"), "launch": "admitted",
        "tasks": {task: {**_lifetime_facts(state / "tasks" / task / "lifetime", "task", record),
                         "spec_sha256": digest(state / "tasks" / task / "task.json"),
                         "meta_sha256": digest(state / "tasks" / task / "meta.json")}
                  for task, record in sorted(tasks.items())},
        "inspections": {operation: {**_lifetime_facts(parent / operation, "inspection", record),
                                    "request_sha256": digest(parent / operation / "request.json")}
                        for operation, record in sorted(inspections.items())},
    }


def _known_no_launch(acceptance) -> bool:
    """Return whether the ordinary root provably admitted nothing at all."""
    return (not acceptance.tasks and not acceptance.ops.dispatch_attempts and
            not acceptance.ops.inspection_attempts and
            not os.path.lexists(acceptance.state / "tasks") and
            not os.path.lexists(acceptance.state / "inspections"))


def record_lifetime_closure(owner) -> None:
    """Prove every managed lifetime closed before the private daemon stops."""
    acceptance = owner
    if not acceptance.ops.managed_runner:
        # Internal custom-runner fixtures own no managed lifetime. Their
        # receipt records no proof, so release evidence stays unresolved.
        acceptance.lifetime_closure = None
        return
    task_ids = set(acceptance.tasks.values())
    discovery_required = acceptance.model_discovery_summary is not None
    if not discovery_required:
        require(not os.path.lexists(acceptance.models_state / "inspections"),
                "model discovery state exists without a discovery result")
    proof: dict[str, object] = {
        "schema_version": LIFETIME_CLOSURE_SCHEMA_VERSION, "kind": LIFETIME_CLOSURE_KIND,
        "ordinary": ({"root_id": None, "launch": "none", "tasks": {}, "inspections": {}}
                     if _known_no_launch(acceptance) else _root_closure(acceptance.state, task_ids)),
        "model_discovery": (_root_closure(acceptance.models_state, set())
                            if discovery_required else None),
    }
    # Only the timeout source is expected to close by its own budget.
    budget = [acceptance.tasks["timeout-source"]] if "timeout-source" in acceptance.tasks else []
    error = lifetime_closure_error(proof, task_ids=sorted(task_ids), budget_task_ids=budget,
                                   model_discovery_required=discovery_required)
    require(error is None, f"managed lifetime closure is not proven: {error}")
    discovery = proof["model_discovery"]
    require(discovery is None or (isinstance(discovery, dict) and isinstance(discovery.get("inspections"), dict) and
                                  set(discovery["inspections"]) == {acceptance.model_discovery_task_id}),
            "model discovery lifetime closure does not cover its inspection")
    acceptance.lifetime_closure = proof


def lifecycle_receipt(owner) -> dict[str, object]:
    """Return the one success-receipt fragment for shutdown and closure proof."""
    acceptance = owner
    return {"natural_daemon_shutdown": acceptance.closed,
            "lifetime_closure": acceptance.lifetime_closure}


def shutdown(owner) -> None:
    acceptance = owner
    require(acceptance.daemon is not None, "private daemon was not started")
    deadline = time.monotonic() + 30
    terminal = False
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        process = acceptance.direct("final-status", pueue_command(acceptance.pueue, acceptance.config, "status", "--json"),
                              timeout=min(20, remaining))
        status = status_jobs(process)
        if acceptance.terminal_private_queue(status):
            terminal = True
            break
        delay = min(0.2, max(0.0, deadline - time.monotonic()))
        if delay <= 0:
            break
        time.sleep(delay)
    if not terminal:
        raise AcceptanceFailure("private queue did not reach a terminal state")
    record_lifetime_closure(acceptance)
    acceptance.direct("private-shutdown", pueue_command(acceptance.pueue, acceptance.config, "shutdown"), timeout=30)
    acceptance.daemon.wait(30, expected=0)
    acceptance.closed = True

def safe_shutdown(owner) -> bool:
    acceptance = owner
    if acceptance.closed:
        return True
    if acceptance.daemon is None:
        processes = getattr(acceptance, "processes", None)
        if processes is None:
            acceptance.closed = True
            return True
        try:
            if not processes.finished_naturally():
                return False
        except BaseException:
            return False
        acceptance.closed = True
        return True
    try:
        if acceptance.root_id is not None and acceptance.ops.root_id is not None:
            require(acceptance.root_id == acceptance.ops.root_id,
                    "failure cleanup root identity changed")
        elif acceptance.root_id is not None:
            acceptance.ops.root_id = acceptance.root_id
        acceptance._sync_ops_owned_tasks()
        cleanup_ops: NativeTaskOps = acceptance.ops
        if getattr(acceptance, "discover_models_only", False):
            cleanup_ops = ModelDiscoveryCleanupOps(acceptance)
        cleanup_ops.bind_supervisor(acceptance.config, acceptance.daemon)
        retained = cleanup_ops.retain_failure_ownership()
    except BaseException:
        return False
    if retained:
        acceptance.closed = True
    return retained
