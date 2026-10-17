"""Observe all managed task and inspection closure before cleanup authorization."""

from __future__ import annotations

import stat
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.evidence.records import _read_control_record, _state_root_identity
from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _task_identity,
    require,
)
from scripts.delegate_acceptance.inspections.authority import (
    _inspection_exit_authority,
    _inspection_managed_authority_started,
    _validate_unsubmitted_inspection_records,
)
from scripts.delegate_acceptance.inspections.binding import _validate_inspection_binding
from scripts.delegate_acceptance.lifetime.evidence import _validate_managed_ordinary_lifetime
from scripts.delegate_acceptance.workspaces.admission_evidence import _inspection_request_identity
from scripts.delegate_acceptance.workspaces.closure import _scan_managed_workspace_lifetime_closure
from pathlib import Path
from typing import Iterable


def _ordinary_lifetime_control_started(directory: Path) -> bool:
    """Return whether durable managed-lifetime evidence was published."""
    lifetime = directory / "lifetime"
    try:
        lifetime.lstat()
    except FileNotFoundError:
        return False
    except OSError as error:
        raise AcceptanceFailure("cannot inspect ordinary task lifetime records") from error
    require(lifetime.is_dir() and not lifetime.is_symlink(),
            "ordinary task lifetime path is not a regular directory")
    try:
        entries = list(lifetime.iterdir())
    except OSError as error:
        raise AcceptanceFailure("cannot inspect ordinary task lifetime records") from error
    for entry in entries:
        if entry.name == "owner.lock":
            require(entry.is_file() and not entry.is_symlink(),
                    "ordinary task lifetime owner lock is not a regular file")
            continue
        # intent.json is the first durable authorization record. Any other
        # entry must also enter full validation so malformed or partial
        # journals fail closed rather than being mistaken for an unused lock.
        return True
    return False


def _ordinary_supervisor_handoff_started(directory: Path) -> bool:
    """Return whether the supervisor durably accepted the staged task."""
    receipt = directory / "supervisor.ref.json"
    try:
        info = receipt.lstat()
    except FileNotFoundError:
        return False
    except OSError as error:
        raise AcceptanceFailure("cannot inspect ordinary supervisor receipt") from error
    require(stat.S_ISREG(info.st_mode) and not receipt.is_symlink(),
            "ordinary supervisor receipt is not a regular file")
    return True


def _scan_managed_ordinary_lifetime_closure(
        state: Path, root: str, task_ids: Iterable[str] = ()) -> dict[str, dict[str, object]]:
    """Scan ordinary state and reuse the canonical task lifetime validator."""
    requested = set(task_ids)
    require(all(isinstance(task, str) and _task_identity(task) for task in requested),
            "ordinary task lifetime task identity is invalid")
    tasks_parent = state / "tasks"
    try:
        entries = tuple(tasks_parent.iterdir())
    except FileNotFoundError:
        require(not requested, "ordinary task state is missing")
        return {}
    except OSError as error:
        raise AcceptanceFailure("cannot inspect ordinary task state") from error
    require(tasks_parent.is_dir() and not tasks_parent.is_symlink(),
            "ordinary task state is not a regular directory")

    candidates = set(requested)
    for entry in entries:
        require(_task_identity(entry.name),
                "ordinary task state contains an invalid task identity")
        require(entry.is_dir() and not entry.is_symlink(),
                f"ordinary task directory is invalid for {entry.name}")
        candidates.add(entry.name)

    closures: dict[str, dict[str, object]] = {}
    for task in sorted(candidates):
        directory = tasks_parent / task
        require(directory.is_dir() and not directory.is_symlink(),
                f"ordinary task directory is missing for {task}")
        meta_path = directory / "meta.json"
        meta = _read_control_record(meta_path, f"ordinary task metadata for {task}")
        ownership = meta.get("runner_ownership")
        require(ownership is None or ownership in {"managed", "custom"},
                f"ordinary task runner ownership is invalid for {task}")
        lifetime_started = _ordinary_lifetime_control_started(directory)
        handoff_started = _ordinary_supervisor_handoff_started(directory)
        if ownership == "managed" and not lifetime_started and not handoff_started:
            # A managed runner recorded in metadata is only a launch plan. The
            # immutable supervisor receipt is the handoff authority. Without
            # either that receipt or a lifetime journal, no worker was owned.
            continue
        if ownership != "managed" and not lifetime_started:
            continue

        task_path = directory / "task.json"
        require(task_path.is_file() and not task_path.is_symlink(),
                f"ordinary task record is missing for {task}")
        require(meta_path.is_file() and not meta_path.is_symlink(),
                f"ordinary task metadata is invalid for {task}")
        closure = _validate_managed_ordinary_lifetime(
            directory, root, task, digest(task_path), digest(meta_path))
        closures[task] = closure
    return closures


def _scan_managed_inspection_lifetime_closure(
        state: Path, root: str) -> dict[str, dict[str, object]]:
    """Scan inspection state and reuse the canonical inspection lifetime validator."""
    inspections_parent = state / "inspections"
    try:
        inspections_parent.lstat()
    except FileNotFoundError:
        return {}
    except OSError as error:
        raise AcceptanceFailure("cannot inspect managed inspection state") from error
    require(inspections_parent.is_dir() and not inspections_parent.is_symlink(),
            "managed inspection state is not a regular directory")

    closures: dict[str, dict[str, object]] = {}
    try:
        entries = tuple(inspections_parent.iterdir())
    except OSError as error:
        raise AcceptanceFailure("cannot inspect managed inspection state") from error
    for directory in entries:
        require(_task_identity(directory.name),
                "managed inspection state contains an invalid task identity")
        require(directory.is_dir() and not directory.is_symlink(),
                f"managed inspection directory is invalid for {directory.name}")
        directory_identity = directory.name
        request_path = directory / "request.json"
        request = _read_control_record(
            request_path, f"inspection request for {directory_identity}")
        identity = _inspection_request_identity(
            state, root, request, directory_identity,
            f"inspection request for {directory_identity}")
        task = identity["operation_id"]
        binding = request.get("binding")
        _validate_inspection_binding(binding, f"inspection request binding for {task}")
        created = _canonical_timestamp(
            request.get("created_at"), f"inspection request created_at for {task}")
        request_deadline = request.get("deadline")
        deadline = _canonical_timestamp(
            request_deadline, f"inspection request deadline for {task}")

        if not _inspection_managed_authority_started(directory):
            # A request and its task-local proof reference are intent-only. No
            # managed worker or scope exists until a lifecycle authority is
            # durably published.
            _validate_unsubmitted_inspection_records(directory, task)
            continue
        authority = _inspection_exit_authority(
            directory=directory, root=root, task=task, binding=binding, request_digest=digest(request_path),
            request_created=created,
            request_deadline=request_deadline, deadline=deadline)
        if authority["mode"] == "queue":
            # Historical and custom inspections retain their queue-owned
            # observation contract until a durable managed lifetime marker exists.
            continue
        require(authority["mode"] in {"guardian", "guardian-unlaunched"} and
                isinstance(authority.get("managed_lifetime"), dict),
                f"managed inspection lifetime evidence is absent for {task}")
        closures[task] = authority["managed_lifetime"]
    return closures


def validate_managed_lifetime_closure(
        state: Path, task_ids: Iterable[str] = ()) -> dict[str, object]:
    """Authorize cleanup from every managed lifetime under one state root.

    The state root is authoritative: task and inspection directories are
    scanned instead of relying on queue rows or dispatch response files.
    Historical and custom inspections without a durable managed lifetime
    marker remain under their existing queue-observation contract. Workspace
    operation journals are always included because they are independent of
    ordinary task directories.
    """
    state = Path(state)
    root, _ = _state_root_identity(state, "managed lifetime root record")
    workspace = _scan_managed_workspace_lifetime_closure(state, root)
    closure: dict[str, object] = {
        "root_id": root,
        "tasks": _scan_managed_ordinary_lifetime_closure(state, root, task_ids),
        "inspections": _scan_managed_inspection_lifetime_closure(state, root),
    }
    # Preserve the historical empty-root shape for ordinary/inspection callers;
    # every actual workspace operation is still scanned and returned here.
    if workspace:
        closure["workspace"] = workspace
    return closure
