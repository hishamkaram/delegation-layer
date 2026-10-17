"""Observe workspace helper records through the canonical lifetime validator."""

from __future__ import annotations

import stat
from datetime import datetime
from scripts.delegate_acceptance.evidence.limits import MAX_PROVIDER_BYTES
from scripts.delegate_acceptance.evidence.records import _read_canonical_control_record
from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _task_identity,
    require,
)
from scripts.delegate_acceptance.lifetime.evidence import _validate_managed_lifetime
from scripts.delegate_acceptance.lifetime.identity import (
    _WORKSPACE_LIFETIME_RECORDS,
    _lifetime_launch_sequence,
)
from scripts.delegate_acceptance.lifetime.unlaunched import (
    _auxiliary_lifetime_unlaunched,
    _operation_lifetime_owner_live,
)
from scripts.delegate_acceptance.workspaces.specification import (
    _canonical_operation_guard_bytes,
    _canonical_operation_receipt_bytes,
    _canonical_operation_request_bytes,
    _canonical_operation_result_bytes,
)
from pathlib import Path


_WORKSPACE_OPERATION_GUARDS = {
    "submission.json", "receipt.json", "result.json", "start.json", "stop.json",
}


_WORKSPACE_OPERATION_LOCKS = {".operation.lock", ".lifetime-owner.lock"}


def _workspace_operation_entries(directory: Path, operation: str) -> set[str]:
    """Validate the bounded top-level namespace of one operation journal."""
    try:
        entries = tuple(directory.iterdir())
    except OSError as error:
        raise AcceptanceFailure(
            f"cannot inspect workspace operation records for {operation}") from error
    names: set[str] = set()
    for entry in entries:
        name = entry.name
        require(name not in names, f"duplicate workspace operation record {operation}/{name}")
        names.add(name)
        try:
            metadata = entry.lstat()
        except OSError as error:
            raise AcceptanceFailure(
                f"cannot inspect workspace operation record {operation}/{name}") from error
        require(not stat.S_ISLNK(metadata.st_mode),
                f"workspace operation record is a symlink: {operation}/{name}")
        if name == "artifacts":
            require(stat.S_ISDIR(metadata.st_mode),
                    f"workspace operation artifacts are not a directory: {operation}")
            try:
                artifacts = tuple(entry.rglob("*"))
            except OSError as error:
                raise AcceptanceFailure(
                    f"cannot inspect workspace operation artifacts for {operation}") from error
            for artifact in artifacts:
                artifact_metadata = artifact.lstat()
                require(not stat.S_ISLNK(artifact_metadata.st_mode) and
                        stat.S_ISREG(artifact_metadata.st_mode),
                        f"workspace operation artifact is not a regular file: {operation}")
                require(artifact_metadata.st_size <= MAX_PROVIDER_BYTES,
                        f"workspace operation artifact exceeds the acceptance bound: {operation}")
            continue
        allowed = (name == "request.json" or name in _WORKSPACE_OPERATION_GUARDS or
                   name in _WORKSPACE_OPERATION_LOCKS or
                   name in _WORKSPACE_LIFETIME_RECORDS or
                   _lifetime_launch_sequence("workspace", name) is not None)
        require(allowed, f"workspace operation has an unexpected record: {operation}/{name}")
        require(stat.S_ISREG(metadata.st_mode),
                f"workspace operation record is not regular: {operation}/{name}")
    return names


def _validate_workspace_operation_records(
        directory: Path, root: str, operation: str, request: dict[str, object],
        request_digest: str, names: set[str],
        ) -> None:
    """Validate every create-once guard that can authorize an auxiliary worker."""
    for name in ("submission.json", "start.json", "stop.json"):
        if name in names:
            _read_canonical_control_record(
                directory / name,
                f"workspace operation {name[:-5]} guard for {operation}",
                lambda value, record=name: _canonical_operation_guard_bytes(
                    value, request_digest,
                    f"workspace operation {record[:-5]} guard for {operation}"),
            )
    if "receipt.json" in names:
        _read_canonical_control_record(
            directory / "receipt.json",
            f"workspace operation receipt for {operation}",
            lambda value: _canonical_operation_receipt_bytes(
                value, request_digest, f"workspace operation receipt for {operation}"),
        )
    if "result.json" in names:
        _read_canonical_control_record(
            directory / "result.json",
            f"workspace operation result for {operation}",
            lambda value: _canonical_operation_result_bytes(
                value, operation, request_digest, str(request["kind"]),
                f"workspace operation result for {operation}"),
        )


def _workspace_operation_unlaunched(
        directory: Path, root: str, operation: str, request_digest: str,
        deadline: str, names: set[str],
        ) -> dict[str, object]:
    return _auxiliary_lifetime_unlaunched(
        directory, root, operation, request_digest, deadline, names, "workspace")


def _workspace_operation_native_closure(
        directory: Path, root: str, operation: str, request_digest: str,
        deadline: tuple[datetime, int], deadline_text: str, names: set[str],
        ) -> dict[str, object]:
    """Reuse the shared managed lifetime validator for a workspace helper."""
    require("lifetime-unlaunched.json" not in names,
            f"workspace operation has both native and unlaunched proof: {operation}")
    native_records = (_WORKSPACE_LIFETIME_RECORDS - {"lifetime-unlaunched.json"}) | {
        name for name in names if _lifetime_launch_sequence("workspace", name) is not None
    }
    require(names & native_records,
            f"workspace operation has no native lifetime authority: {operation}")
    require(not _operation_lifetime_owner_live(
        directory, f"workspace operation {operation}"),
        f"workspace operation lifetime owner is still active: {operation}")
    closure = _validate_managed_lifetime(
        directory=directory, root=root, task=operation, owner_kind="workspace", owner_digest=request_digest,
        expected_deadline=deadline_text, window_deadline=deadline)
    wait = closure.get("worker_wait")
    termination = closure.get("termination")
    require(isinstance(wait, dict) or (
        isinstance(termination, dict) and termination.get("cause") == "previous-boot"),
            f"workspace operation WorkerWait proof is missing: {operation}")
    return {
        "mode": "recovered" if isinstance(wait, dict) and wait.get("result") == "absent" else "terminated",
        "request_sha256": request_digest,
        "managed_lifetime": closure,
    }


def _scan_managed_workspace_lifetime_closure(
        state: Path, root: str) -> dict[str, dict[str, object]]:
    """Scan every workspace operation, including roots with no ordinary tasks."""
    operations_parent = state / "workspace-operations"
    try:
        operations_parent.lstat()
    except FileNotFoundError:
        return {}
    except OSError as error:
        raise AcceptanceFailure("cannot inspect managed workspace operations") from error
    require(operations_parent.is_dir() and not operations_parent.is_symlink(),
            "managed workspace operations are not a regular directory")
    try:
        entries = tuple(operations_parent.iterdir())
    except OSError as error:
        raise AcceptanceFailure("cannot enumerate managed workspace operations") from error

    return {directory.name: validate_managed_workspace_operation_closure(directory, root)
            for directory in entries}


def validate_managed_workspace_operation_closure(
        directory: Path, root: str) -> dict[str, object]:
    """Validate one exact operation with the same authority as root cleanup."""
    operation = directory.name
    require(_task_identity(operation),
            "managed workspace operation has an invalid operation identity")
    require(directory.is_dir() and not directory.is_symlink(),
            f"managed workspace operation is not a regular directory: {operation}")
    names = _workspace_operation_entries(directory, operation)
    require("request.json" in names,
            f"workspace operation request is missing: {operation}")
    request, request_digest = _read_canonical_control_record(
        directory / "request.json",
        f"workspace operation request for {operation}",
        lambda value: _canonical_operation_request_bytes(
            value, root, operation, f"workspace operation request for {operation}"),
    )
    _validate_workspace_operation_records(
        directory, root, operation, request, request_digest, names)
    deadline = _canonical_timestamp(
        request["deadline"], f"workspace operation deadline for {operation}")
    native_records = (_WORKSPACE_LIFETIME_RECORDS - {"lifetime-unlaunched.json"}) | {
        name for name in names if _lifetime_launch_sequence("workspace", name) is not None
    }
    has_native_authority = bool(names & native_records)
    has_unlaunched = "lifetime-unlaunched.json" in names
    has_guard = bool(names & _WORKSPACE_OPERATION_GUARDS)
    has_artifacts = "artifacts" in names
    if has_unlaunched:
        require(not has_native_authority,
                f"workspace operation has contradictory unlaunched/native facts: {operation}")
        return _workspace_operation_unlaunched(
            directory, root, operation, request_digest, request["deadline"], names)
    if has_native_authority or has_guard or has_artifacts:
        return _workspace_operation_native_closure(
            directory, root, operation, request_digest, deadline,
            request["deadline"], names)
    # Before the create-once guard there is no native launch authority. The
    # operation owner lock is the only live fact that can still retain the
    # journal; an absent live lease makes this an exact no-side-effect case.
    require(not _operation_lifetime_owner_live(
        directory, f"workspace operation {operation}"),
        f"unsubmitted workspace operation lifetime owner is still active: {operation}")
    return {
        "mode": "unsubmitted",
        "request_sha256": request_digest,
    }
