"""Composed native acceptance behavior: lifecycle."""

from __future__ import annotations


import time
from scripts.delegate_acceptance.directories.finalization import finalize_acceptance_artifacts
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.evidence.records import status_jobs
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.native.task_ops import ModelDiscoveryCleanupOps
from scripts.delegate_acceptance.native.timeouts import TimeoutLeaseOwner
from scripts.delegate_acceptance.supervisor.configuration import pueue_command
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps


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
    retained_owned_directories = ()
    if not success and not blocked:
        retained_targets = (acceptance.base, acceptance.state, acceptance.workspace)
        retained_owned_directories = tuple(
            entry for entry in acceptance.owned_directories
            if any(entry.directory == target or entry.directory in target.parents
                   for target in retained_targets)
        )
    artifacts_clean = finalize_acceptance_artifacts(
        acceptance.output,
        acceptance.closed,
        acceptance.owned_directories,
        success,
        remove_failed_process_captures=failure_diagnostics_written,
        raw_process_captures_token=acceptance.processes.capture_root,
        retained_owned_directories=retained_owned_directories,
    )
    return leases_clean and artifacts_clean

def publish_success(owner) -> None:
    acceptance = owner
    receipt = getattr(acceptance, "success_receipt", None)
    require(isinstance(receipt, dict), "acceptance success receipt is unavailable")
    write_json(acceptance.output / "success.json", receipt)

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
