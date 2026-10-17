"""Composed native acceptance behavior: diagnostics."""

from __future__ import annotations


import json
import stat
from pathlib import Path
from scripts.delegate_acceptance.evidence.files import read_json, write_json
from scripts.delegate_acceptance.evidence.validation import _task_identity, require
from scripts.delegate_acceptance.native.diagnostics import (
    _diagnostic_json_bytes,
    _diagnostic_output_diagnostics,
    _diagnostic_text_descriptor,
    _diagnostic_workspace,
)
from scripts.delegate_acceptance.native.constants import (
    AUTHENTICATION_STATUS_CODES,
    FAILURE_DIAGNOSTICS_BYTES,
    FAILURE_DIAGNOSTICS_MAX_OMISSIONS,
    FAILURE_DIAGNOSTICS_MAX_TASKS,
    FAILURE_DIAGNOSTICS_TEXT_BYTES,
    MAX_CONTROL_BYTES,
    MAX_RAW_BYTES,
)


def authentication_unavailable(directory: Path) -> bool:
    outcome_path = directory / "outcome.json"
    if not outcome_path.is_file() or outcome_path.is_symlink():
        return False
    try:
        outcome = read_json(outcome_path)
    except (OSError, ValueError, TypeError, RecursionError, RuntimeError):
        return False
    if not isinstance(outcome, dict) or outcome.get("verdict") != "rejected":
        return False
    if any(diagnostic.get("code") == "authentication-unavailable"
           for diagnostic in _diagnostic_output_diagnostics(outcome.get("diagnostics"))):
        return True
    for relative in ("raw/stdout", "raw/stderr", "publish.reject"):
        path = directory / relative
        if not path.is_file() or path.is_symlink():
            continue
        try:
            with path.open("rb") as stream:
                data = stream.read(MAX_RAW_BYTES).decode("utf-8", errors="replace")
        except OSError:
            continue
        for line in data.splitlines():
            try:
                event = json.loads(line)
            except (TypeError, ValueError, RecursionError):
                continue
            if not isinstance(event, dict):
                continue
            error = event.get("error")
            details = error.get("data") if isinstance(error, dict) else None
            status_code = details.get("statusCode") if isinstance(details, dict) else None
            if type(status_code) is int and status_code in AUTHENTICATION_STATUS_CODES:
                return True
    return False


def _failure_diagnostics(owner) -> dict[str, object]:
    acceptance = owner
    omitted: list[str] = []
    status: dict[str, object] = {
        "partial": False, "omitted": [], "omitted_count": 0, "truncated": [],
    }

    def record_omissions(values: list[str]) -> None:
        if not values:
            return
        status["partial"] = True
        status["omitted_count"] = int(status["omitted_count"]) + len(values)
        current = status["omitted"]
        require(isinstance(current, list), "failure diagnostic omission state is malformed")
        remaining = max(0, FAILURE_DIAGNOSTICS_MAX_OMISSIONS - len(current))
        current.extend(str(value)[:FAILURE_DIAGNOSTICS_TEXT_BYTES] for value in values[:remaining])

    failure, missing = acceptance._failure_diagnostic_record(omitted)
    if missing:
        record_omissions(omitted)

    task_specs = acceptance._failure_diagnostic_task_specs(omitted)
    tasks = []
    numeric_ids = getattr(acceptance, "numeric_ids", {})
    for name, task_id, state_root in task_specs:
        task = acceptance._diagnostic_task(state_root, task_id, omitted)
        task["name"] = name[:FAILURE_DIAGNOSTICS_TEXT_BYTES]
        numeric_id = numeric_ids.get(task_id)
        if type(numeric_id) is int and numeric_id >= 0:
            task["numeric_id"] = numeric_id
        tasks.append(task)

    workspace_path = getattr(acceptance, "workspace", None)
    if isinstance(workspace_path, Path):
        workspace = _diagnostic_workspace(workspace_path)
        workspace_omitted = workspace.pop("omitted", [])
        if isinstance(workspace_omitted, list):
            omitted.extend(str(value) for value in workspace_omitted)
        if workspace.pop("omitted_count", 0):
            status["partial"] = True
        if workspace.pop("omitted_truncated", False):
            status["truncated"].append("workspace.omitted")
        if workspace.pop("truncated", False):
            status["truncated"].append("workspace.entries")
            status["partial"] = True
    else:
        workspace = {"entries": {}}
        omitted.append("workspace")

    record_omissions(omitted)
    result: dict[str, object] = {
        "schema_version": 1,
        "provider": getattr(acceptance, "provider", None),
        "mode": getattr(acceptance, "mode", None),
        "scenario": getattr(acceptance, "scenario", None),
        "failure": failure,
        "task_ids": [task_id for _name, task_id, _state in task_specs],
        "tasks": tasks,
        "workspace": workspace,
        "status": status,
    }
    status["partial"] = bool(status["partial"] or status["omitted_count"] or status["truncated"])
    return acceptance._fit_failure_diagnostics(result)

def _failure_diagnostic_record(
        owner, omitted: list[str]) -> tuple[dict[str, object], bool]:
    acceptance = owner
    failure: dict[str, object] = {}
    failure_path = acceptance.output / "failure.json"
    try:
        raw_failure = read_json(failure_path, bound=MAX_CONTROL_BYTES)
    except (OSError, ValueError, TypeError, RecursionError, RuntimeError):
        raw_failure = None
        omitted.append("failure.json")
    if isinstance(raw_failure, dict):
        for key in ("gate", "status", "provider", "mode", "scenario", "task_budget"):
            value = raw_failure.get(key)
            if isinstance(value, str) and len(value) <= FAILURE_DIAGNOSTICS_TEXT_BYTES and \
                    not any(ord(character) < 32 for character in value):
                failure[key] = value
        for key in ("requested_model", "requested_effort"):
            value = raw_failure.get(key)
            if value is None or (isinstance(value, str) and
                                 len(value) <= FAILURE_DIAGNOSTICS_TEXT_BYTES and
                                 not any(ord(character) < 32 for character in value)):
                failure[key] = value
        for key in ("discover_models", "no_retry", "credentials_in_receipt"):
            value = raw_failure.get(key)
            if type(value) is bool:
                failure[key] = value
        if "error" in raw_failure:
            failure["error"] = _diagnostic_text_descriptor(raw_failure.get("error"))
    else:
        return failure, True
    return failure, False

def _failure_diagnostic_task_specs(
        owner, omitted: list[str]) -> list[tuple[str, str, Path]]:
    acceptance = owner
    task_specs: list[tuple[str, str, Path]] = []
    seen: set[str] = set()
    for name, task_id in sorted(getattr(acceptance, "tasks", {}).items()):
        if not isinstance(name, str) or not _task_identity(task_id) or task_id in seen:
            omitted.append("admitted-task")
            continue
        seen.add(task_id)
        task_specs.append((name, task_id, acceptance.state))
    model_task = getattr(acceptance, "model_discovery_task_id", None)
    models_state = getattr(acceptance, "models_state", None)
    if _task_identity(model_task) and model_task not in seen and isinstance(models_state, Path):
        task_specs.append(("model-discovery", model_task, models_state))
        seen.add(model_task)
    if len(task_specs) > FAILURE_DIAGNOSTICS_MAX_TASKS:
        omitted.append("admitted-task-list")
        task_specs = task_specs[:FAILURE_DIAGNOSTICS_MAX_TASKS]
    return task_specs

def _write_failure_diagnostics(owner) -> bool:
    acceptance = owner
    path = acceptance.output / "failure-diagnostics.json"
    try:
        try:
            metadata = path.lstat()
        except FileNotFoundError:
            metadata = None
        if metadata is not None:
            require(stat.S_ISREG(metadata.st_mode) and not stat.S_ISLNK(metadata.st_mode),
                    "failure diagnostic path is unsafe")
            data = path.read_bytes()
            require(len(data) <= FAILURE_DIAGNOSTICS_BYTES,
                    "existing failure diagnostic exceeds its bound")
            value = json.loads(data)
            require(isinstance(value, dict) and value.get("schema_version") == 1,
                    "existing failure diagnostic is malformed")
            return True
        value = acceptance._failure_diagnostics()
        data = _diagnostic_json_bytes(value)
        require(len(data) <= FAILURE_DIAGNOSTICS_BYTES,
                "failure diagnostic exceeds its bound")
        write_json(path, value, replace=True)
        require(path.is_file() and not path.is_symlink() and
                path.stat().st_size <= FAILURE_DIAGNOSTICS_BYTES,
                "persisted failure diagnostic exceeds its bound")
        return True
    except BaseException:
        staging = path.with_name(path.name + ".staging")
        try:
            if staging.is_file() and not staging.is_symlink():
                staging.unlink()
        except OSError:
            pass
        return False
