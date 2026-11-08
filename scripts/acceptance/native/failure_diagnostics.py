"""Composed native acceptance behavior: diagnostics."""

from __future__ import annotations


import json
import stat
from pathlib import Path
from scripts.acceptance.evidence.files import read_json
from scripts.acceptance.evidence.files import write_json
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.validation import _task_identity
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.native.diagnostics import _diagnostic_inspection
from scripts.acceptance.native.diagnostics import _diagnostic_json_bytes
from scripts.acceptance.native.diagnostics import _diagnostic_output_diagnostics
from scripts.acceptance.native.diagnostics import _diagnostic_text_descriptor
from scripts.acceptance.native.diagnostics import _diagnostic_workspace
from scripts.acceptance.native.constants import AUTHENTICATION_STATUS_CODES
from scripts.acceptance.native.constants import FAILURE_DIAGNOSTICS_BYTES
from scripts.acceptance.native.constants import FAILURE_DIAGNOSTICS_MAX_OMISSIONS
from scripts.acceptance.native.constants import FAILURE_DIAGNOSTICS_MAX_TASKS
from scripts.acceptance.native.constants import FAILURE_DIAGNOSTICS_TEXT_BYTES
from scripts.acceptance.native.constants import MAX_CONTROL_BYTES
from scripts.acceptance.native.constants import MAX_RAW_BYTES


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
        count = status["omitted_count"]
        if not isinstance(count, int):
            raise AcceptanceFailure("failure diagnostic omission count is malformed")
        status["omitted_count"] = count + len(values)
        current = status["omitted"]
        if not (isinstance(current, list)):
            raise AcceptanceFailure("failure diagnostic omission state is malformed")
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
    model_inspection = _model_discovery_inspection_diagnostic(acceptance, omitted)
    diagnosed_ids = [task_id for _name, task_id, _state in task_specs]
    if model_inspection is not None and model_inspection["journal_task_id"] is not None:
        diagnosed_ids.append(model_inspection["journal_task_id"])

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
        "task_ids": diagnosed_ids,
        "tasks": tasks,
        "workspace": workspace,
        "status": status,
    }
    if model_inspection is not None:
        result["model_discovery_inspection"] = model_inspection
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
    if len(task_specs) > FAILURE_DIAGNOSTICS_MAX_TASKS:
        omitted.append("admitted-task-list")
        task_specs = task_specs[:FAILURE_DIAGNOSTICS_MAX_TASKS]
    return task_specs

def _model_inspection_context(response: object) -> dict[str, object] | None:
    """Return the typed inspection context of a response validated on receipt."""
    failure = response.get("failure") if isinstance(response, dict) else None
    inspection = failure.get("inspection") if isinstance(failure, dict) else None
    if not isinstance(inspection, dict):
        return None
    return {key: inspection[key] for key in ("class", "deadline_state", "journal_task_id")
            if isinstance(inspection.get(key), str) and
            len(inspection[key]) <= FAILURE_DIAGNOSTICS_TEXT_BYTES and
            not any(ord(character) < 32 for character in inspection[key])}

def _model_discovery_inspection_diagnostic(
        owner, omitted: list[str]) -> dict[str, object] | None:
    """Project the model inspection through its own journal, never as an ordinary task."""
    acceptance = owner
    models_state = getattr(acceptance, "models_state", None)
    context = _model_inspection_context(getattr(acceptance, "model_discovery", None))
    task_id = getattr(acceptance, "model_discovery_task_id", None)
    if not _task_identity(task_id):
        task_id = context.get("journal_task_id") if context is not None else None
    if not isinstance(models_state, Path) or (context is None and not _task_identity(task_id)):
        return None
    if _task_identity(task_id):
        inspection = _diagnostic_inspection(models_state, task_id, omitted)
    else:
        inspection = {"journal_task_id": None, "journal": "unknown", "files": {}}
    if context is not None:
        inspection["context"] = context
    return inspection

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
            if not (isinstance(value, dict) and value.get("schema_version") == 1):
                raise AcceptanceFailure("existing failure diagnostic is malformed")
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
