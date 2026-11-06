"""Observe workspace reservations and relate historical admission attempts."""

from __future__ import annotations

import stat
from copy import deepcopy
from scripts.delegate_acceptance.evidence.records import _read_canonical_control_record
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _exact_keys,
    _hex_digest,
    _task_identity,
    require,
)
from scripts.delegate_acceptance.tasks.specification import (
    _canonical_workspace_binding,
    _validate_task_record,
)
from scripts.delegate_acceptance.workspaces.admission_identity import (
    _admission_attempt_operation_id,
)
from scripts.delegate_acceptance.workspaces.data import InspectionIdentity, WorkspaceAdmissionAttempt
from scripts.delegate_acceptance.workspaces.specification import (
    _canonical_workspace_closure_bytes,
    _canonical_workspace_reservation_bytes,
    _canonical_workspace_resource_request_bytes,
)
from pathlib import Path


def _workspace_admission_attempt(
        state: Path, root: str, task_record: object, label: str,
        ) -> WorkspaceAdmissionAttempt:
    """Resolve one TaskRecord workspace binding through its durable reservation."""
    if not isinstance(task_record, dict):
        raise AcceptanceFailure(f"{label} task is not an object")
    task = task_record.get("task_id")
    if not _task_identity(task):
        raise AcceptanceFailure(f"{label} task_id is invalid")
    _validate_task_record(task_record, root, task, label)
    binding = _canonical_workspace_binding(task_record.get("workspace"),
                                           f"{label} workspace")
    workspace = binding["id"]
    sequence = binding["reservation"]
    request_sha256 = binding["request_sha256"]
    resource_directory = state / "workspaces" / workspace
    require(resource_directory.is_dir() and not resource_directory.is_symlink(),
            f"{label} workspace resource is missing")
    resource_request, resource_digest = _read_canonical_control_record(
        resource_directory / "request.json",
        f"{label} workspace resource request",
        lambda value: _canonical_workspace_resource_request_bytes(
            value, root, workspace, f"{label} workspace resource request"),
    )
    reservation_path = resource_directory / f"use-{sequence:020d}.json"
    reservation, reservation_digest = _read_canonical_control_record(
        reservation_path,
        f"{label} workspace reservation",
        lambda value: _canonical_workspace_reservation_bytes(
            value, workspace, sequence, task, request_sha256,
            f"{label} workspace reservation"),
    )
    closure_path = resource_directory / f"use-{sequence:020d}-closed.json"
    try:
        closure_metadata = closure_path.lstat()
    except FileNotFoundError:
        closure = None
        closure_digest = None
    except OSError as error:
        raise AcceptanceFailure(f"cannot inspect {label} workspace closure") from error
    else:
        require(stat.S_ISREG(closure_metadata.st_mode) and not closure_path.is_symlink(),
                f"{label} workspace closure is not a regular record")
        closure, closure_digest = _read_canonical_control_record(
            closure_path,
            f"{label} workspace reservation closure",
            lambda value: _canonical_workspace_closure_bytes(
                value, workspace, sequence,
                f"{label} workspace reservation closure"),
        )
    operation_id = _admission_attempt_operation_id(
        root, workspace, task, request_sha256, sequence, label)
    return {
        "root_id": root,
        "workspace_id": workspace,
        "task_id": task,
        "request_sha256": request_sha256,
        "sequence": sequence,
        "operation_id": operation_id,
        "task_record": deepcopy(task_record),
        "task_digest": _validate_task_record(task_record, root, task, label),
        "binding": binding,
        "resource_request": resource_request,
        "resource_request_sha256": resource_digest,
        "reservation": reservation,
        "reservation_sha256": reservation_digest,
        "closure": closure,
        "closure_sha256": closure_digest,
    }


def _inspection_request_identity(
        state: Path, root: str, request: object, directory_name: str,
        label: str,
        ) -> InspectionIdentity:
    """Validate logical/effective inspection identity and resolve workspace attempts."""
    if not isinstance(request, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(request, {
        "schema_version", "root_id", "task_id", "task", "task_sha256", "binding",
        "group", "label", "created_at", "deadline",
    }, {"operation_id"}, label=label)
    require(type(request.get("schema_version")) is int and request["schema_version"] == 1 and
            request.get("root_id") == root, f"{label} identity is invalid")
    logical = request.get("task_id")
    if not _task_identity(logical):
        raise AcceptanceFailure(f"{label} task_id is invalid")
    embedded_task = request.get("task")
    if not isinstance(embedded_task, dict):
        raise AcceptanceFailure(f"{label} task is not a task record")
    embedded_digest = _validate_task_record(embedded_task, root, logical,
                                             f"{label} task")
    require(_hex_digest(request.get("task_sha256")) and
            request["task_sha256"] == embedded_digest,
            f"{label} task digest is invalid")
    operation_id = request.get("operation_id")
    if operation_id is not None:
        if not _task_identity(operation_id):
            raise AcceptanceFailure(f"{label} operation_id is invalid")
    attempt = None
    if "workspace" in embedded_task:
        attempt = _workspace_admission_attempt(
            state, root, embedded_task, f"{label} workspace admission")
        require(operation_id is not None and operation_id == attempt["operation_id"],
                f"{label} operation_id does not match workspace admission attempt")
    effective = operation_id if operation_id is not None else logical
    require(effective == directory_name,
            f"{label} directory does not match effective inspection identity")
    return {
        "task_id": logical,
        "operation_id": effective,
        "task": embedded_task,
        "task_sha256": embedded_digest,
        "attempt": attempt,
    }


def _validate_historical_workspace_attempt(
        state: Path, attempt: object, current_task: object, root: str,
        label: str) -> WorkspaceAdmissionAttempt:
    """Allow only an exact later task after a positively aborted old attempt."""
    if not isinstance(attempt, dict):
        raise AcceptanceFailure(f"{label} old admission attempt is invalid")
    closure = attempt.get("closure")
    require(isinstance(closure, dict) and
            closure.get("disposition") == "aborted-not-launched",
            f"{label} old admission lacks an aborted-not-launched closure")
    if not isinstance(current_task, dict):
        raise AcceptanceFailure(f"{label} current task is invalid")
    task = current_task.get("task_id")
    if not _task_identity(task):
        raise AcceptanceFailure(f"{label} current task task_id is invalid")
    _validate_task_record(current_task, root, task, f"{label} current task")
    current_attempt = _workspace_admission_attempt(
        state, root, current_task, f"{label} current admission")
    sequence = attempt.get("sequence")
    if type(sequence) is not int:
        raise AcceptanceFailure(f"{label} old admission attempt sequence is invalid")
    require(current_attempt["workspace_id"] == attempt.get("workspace_id") and
            current_attempt["sequence"] > sequence and
            current_attempt["operation_id"] != attempt.get("operation_id"),
            f"{label} current admission is not a later reservation")
    old_task = attempt.get("task_record")
    if not isinstance(old_task, dict):
        raise AcceptanceFailure(f"{label} old admission task is missing")
    normalized_old = deepcopy(old_task)
    old_workspace = normalized_old.get("workspace")
    current_workspace = current_task.get("workspace")
    if not isinstance(old_workspace, dict) or not isinstance(current_workspace, dict):
        raise AcceptanceFailure(f"{label} workspace binding is missing")
    normalized_workspace = deepcopy(old_workspace)
    normalized_workspace["reservation"] = current_workspace.get("reservation")
    normalized_old["workspace"] = normalized_workspace
    require(normalized_old == current_task,
            f"{label} ordinary task differs from aborted admission beyond reservation")
    return current_attempt
