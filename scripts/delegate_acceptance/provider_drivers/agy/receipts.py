"""Failure evidence and cleanup for the Antigravity acceptance owner."""

from __future__ import annotations

from pathlib import Path
import sys
from typing import TYPE_CHECKING, Protocol

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.directories.operations import OwnedDirectory
from scripts.delegate_acceptance.processes.observation import failure_cleanup_deadline
from scripts.delegate_acceptance.directories.operations import rollback_owned_directories
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.provider_drivers.agy.runtime import BlockedFailure
from scripts.delegate_acceptance.provider_drivers.agy.constants import (
    FINALIZATION_SECONDS,
    WATCH_SECONDS,
)

if TYPE_CHECKING:
    from scripts.delegate_acceptance.provider_drivers.agy.prepared import Prepared


class FailureFinalizer(Protocol):
    """The cleanup operations required from the canonical run owner."""

    def failure_shutdown(self) -> bool: ...

    def finalize(self, success: bool) -> bool: ...


def success_receipt(
        prepared: Prepared, daemon_done: bool, final_queue: dict[str, object] | None,
        task_numbers: dict[str, int], labels: dict[str, str],
        task_results: dict[str, dict[str, object]],
        nonprovider_tasks: dict[str, dict[str, object]], pueue_base: Path | None) -> dict[str, object]:
    require(daemon_done, "private supervisor was not positively shut down")
    require(final_queue is not None, "final queue observation is absent")
    value: dict[str, object] = {
        "tasks": [], "labels": [], "provider_exit": {},
        "events": {"A": 0, "S": 0, "E": 0, "seal": 0},
    }
    for name, task in prepared.ids.items():
        value["tasks"].append(task)
        value["labels"].append({
            "task_id": task, "numeric_task_id": task_numbers[task],
            "label": labels[task], "state": "Done",
        })
        value["provider_exit"][name] = task_results[name]["provider_exit"]
        events = task_results[name]["events"]
        require(events == {"A": 1, "S": 1, "E": 1, "seal": 1},
                f"{name} one-use event counts are not exact: {events}")
        for event, count in events.items():
            value["events"][event] += count
    require(len(value["tasks"]) == 3 and len(set(value["tasks"])) == 3,
            "native gate did not run exactly three task turns")
    value["events"]["A"] += len(nonprovider_tasks)
    return {
        "gate": "acceptance-agy",
        "status": "passed",
        "prepared": str(prepared.source) if prepared.source else None,
        "workspace": str(prepared.workspace),
        "state": str(prepared.state),
        "private_pueue_base": str(pueue_base),
        "tasks": value["tasks"],
        "queue_labels": value["labels"],
        "provider_exit": value["provider_exit"],
        "launch_counts": value["events"],
        "nonprovider_tasks": nonprovider_tasks,
        "watch_seconds": WATCH_SECONDS,
        "raw_evidence_retained": False,
        "supervisor_status_environment_redacted": True,
    }

def _observe_retained_resource(
        value: object, label: str, retained: list[str], unresolved: list[str]) -> bool:
    """Record one resource only when it exists at terminal-receipt time."""
    try:
        path = Path(value)
    except (TypeError, ValueError):
        unresolved.append(f"{label} retention path is invalid")
        return False
    try:
        path.lstat()
    except FileNotFoundError:
        return False
    except OSError as error:
        unresolved.append(f"{label} retention could not be observed: {error}")
        return False
    encoded = str(path)
    if encoded not in retained:
        retained.append(encoded)
    return True

def _observe_owned_directories(
        entries: object, retained: list[str], unresolved: list[str]) -> None:
    if not isinstance(entries, (tuple, list)):
        return
    for index, entry in enumerate(entries):
        directory = getattr(entry, "directory", None)
        if directory is None:
            unresolved.append(f"owned resource {index} has no directory identity")
            continue
        _observe_retained_resource(
            directory, f"owned resource {index}", retained, unresolved)

def _record_run_failure_details(details: dict[str, object], run: FailureFinalizer,
                                prepared: Prepared | None,
                                retained_resources: list[str],
                                unresolved_facts: list[str]) -> Prepared | None:
    prepared = getattr(run, "prepared", prepared)
    state = getattr(prepared, "state", None)
    if state is not None:
        details["state"] = str(state)
    if prepared is not None:
        for name in ("state", "scratch", "workspace"):
            path = getattr(prepared, name, None)
            if path is not None:
                _observe_retained_resource(
                    path, f"prepared {name}", retained_resources, unresolved_facts)
        _observe_owned_directories(
            getattr(prepared, "owned_directories", ()),
            retained_resources, unresolved_facts)
    pueue_base = getattr(run, "pueue_base", None)
    details["private_pueue_base"] = str(pueue_base) if pueue_base else None
    if pueue_base:
        _observe_retained_resource(
            pueue_base, "private supervisor", retained_resources, unresolved_facts)
    _observe_owned_directories(
        getattr(run, "pueue_owned_directories", ()),
        retained_resources, unresolved_facts)
    _observe_owned_directories(
        getattr(run, "process_owned_directories", ()),
        retained_resources, unresolved_facts)
    details["admitted_tasks"] = getattr(run, "processed_turns", 0)
    _record_process_failure_details(details, run, retained_resources, unresolved_facts)
    daemon = getattr(run, "daemon", None)
    details["daemon_pid"] = daemon.pid if daemon is not None else None
    closure = getattr(run, "lifetime_closure", None)
    if closure is not None:
        details["lifetime_closure"] = {"status": "closed", "evidence": closure}
    else:
        unresolved_facts.append("private lifetime closure was not committed")
    if not getattr(run, "daemon_done", False):
        unresolved_facts.append("private supervisor shutdown was not proven")
    return prepared

def _record_process_failure_details(details: dict[str, object], run: FailureFinalizer,
                                   retained_resources: list[str],
                                   unresolved_facts: list[str]) -> None:
    processes = getattr(run, "processes", None)
    if processes is None:
        return
    try:
        pending = processes.pending()
    except BaseException as pending_error:
        pending = []
        unresolved_facts.append(
            "owned process observation failed: " + str(pending_error))
    details["owned_pending"] = pending
    process_directory = getattr(processes, "directory", None)
    if process_directory is None:
        process_directory = getattr(
            getattr(processes, "capture_root", None), "directory", None)
    process_retained = False
    if process_directory is not None:
        process_retained = _observe_retained_resource(
            process_directory, "owned process capture",
            retained_resources, unresolved_facts)
    if pending and not process_retained:
        unresolved_facts.append(
            "owned processes remain pending without observable capture storage")

def failure_receipt(output: Path | None, error: BaseException, run: FailureFinalizer | None,
                    stage: str = "unknown", cleanup: dict[str, object] | None = None,
                    prepared: Prepared | None = None) -> None:
    retained_resources: list[str] = []
    unresolved_facts: list[str] = []
    cleanup_result = cleanup or {"status": "not_started"}
    details: dict[str, object] = {
        "gate": "acceptance-agy",
        "status": "BLOCKED" if isinstance(error, BlockedFailure) else "failed",
        "error": str(error),
        "stage": stage,
        "no_retry": True,
        "signals_sent": 0,
        "lifetime_closure": {"status": "unresolved"},
        "cleanup": cleanup_result,
        "retained_resources": retained_resources,
        "unresolved_facts": unresolved_facts,
    }
    cleanup_status = cleanup_result.get("status")
    if cleanup_status == "failed":
        unresolved_facts.append("acceptance cleanup failed")
    elif cleanup_status != "completed":
        unresolved_facts.append("acceptance cleanup has not completed")
    if run is not None:
        prepared = _record_run_failure_details(
            details, run, prepared, retained_resources, unresolved_facts)
    elif prepared is not None:
        for name in ("state", "scratch", "workspace"):
            path = getattr(prepared, name, None)
            if path is not None:
                _observe_retained_resource(
                    path, f"prepared {name}", retained_resources, unresolved_facts)
        _observe_owned_directories(
            getattr(prepared, "owned_directories", ()),
            retained_resources, unresolved_facts)
        unresolved_facts.append("acceptance run was not constructed")
    else:
        unresolved_facts.append("acceptance run was not constructed")
    if output is not None:
        output.mkdir(mode=0o700, parents=True, exist_ok=True)
        write_json(output / "failure.json", details, replace=True)

def best_effort_failure_receipt(output: Path | None, error: BaseException,
                                run: FailureFinalizer | None, stage: str = "unknown",
                                cleanup: dict[str, object] | None = None,
                                prepared: Prepared | None = None) -> None:
    try:
        failure_receipt(output, error, run, stage, cleanup, prepared)
    except BaseException as diagnostic_error:
        print(f"WARNING acceptance-agy diagnostics were not written: {diagnostic_error}",
              file=sys.stderr, flush=True)

def remove_passed_receipt(output: Path | None) -> BaseException | None:
    if output is None:
        return None
    try:
        (output / "success.json").unlink(missing_ok=True)
    except BaseException as error:
        return error
    return None

def failure_cleanup(run: FailureFinalizer | None, output: Path | None,
                    standalone_directories: tuple[OwnedDirectory, ...] = ()) -> list[BaseException]:
    errors: list[BaseException] = []
    shutdown_completed = run is None
    finalized = run is None
    if run is None:
        try:
            rollback_owned_directories(
                standalone_directories,
                deadline=failure_cleanup_deadline(FINALIZATION_SECONDS))
        except BaseException as error:
            errors.append(error)
            finalized = False
        if output is not None and output.is_dir():
            try:
                write_json(output / "failure-cleanup.json", {
                    "status": "completed" if not errors else "failed",
                    "natural_daemon_shutdown": True,
                    "artifact_finalization_completed": finalized,
                    "passed_receipt_absent": not (output / "success.json").exists(),
                    "signals_sent": 0,
                }, replace=True)
            except BaseException as error:
                errors.append(error)
        return errors
    try:
        if not run.failure_shutdown():
            errors.append(AcceptanceFailure("private supervisor failure shutdown was not proven"))
        else:
            shutdown_completed = True
    except BaseException as error:
        errors.append(error)
    try:
        if not run.finalize(False):
            errors.append(AcceptanceFailure("acceptance artifact finalization was not completed"))
        else:
            finalized = True
    except BaseException as error:
        errors.append(error)
    if output is not None:
        try:
            write_json(output / "failure-cleanup.json", {
                "status": "completed" if not errors else "failed",
                "natural_daemon_shutdown": shutdown_completed,
                "artifact_finalization_completed": finalized,
                "passed_receipt_absent": not (output / "success.json").exists(),
                "signals_sent": 0,
            }, replace=True)
        except BaseException as error:
            errors.append(error)
    return errors
