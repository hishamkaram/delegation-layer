"""Observe workspace helper records through the canonical lifetime validator."""

from __future__ import annotations

import stat
from datetime import datetime
from scripts.acceptance.evidence.files import digest
from scripts.acceptance.evidence.limits import MAX_PROVIDER_BYTES
from scripts.acceptance.evidence.records import _read_canonical_control_record
from scripts.acceptance.evidence.timestamps import _canonical_timestamp
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.validation import _task_identity
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.lifetime.evidence import _validate_managed_lifetime
from scripts.acceptance.lifetime.identity import _WORKSPACE_LIFETIME_RECORDS
from scripts.acceptance.lifetime.identity import _lifetime_launch_sequence
from scripts.acceptance.lifetime.unlaunched import _auxiliary_lifetime_unlaunched
from scripts.acceptance.lifetime.unlaunched import _operation_lifetime_owner_live
from scripts.acceptance.workspaces.checkpoint_records import CHECKPOINT_CANDIDATE_RECORD
from scripts.acceptance.workspaces.checkpoint_records import CHECKPOINT_CREATION_RECORDS
from scripts.acceptance.workspaces.checkpoint_records import CHECKPOINT_RECEIPT_RECORD
from scripts.acceptance.workspaces.checkpoint_records import ClosureProof
from scripts.acceptance.workspaces.checkpoint_records import canonical_checkpoint_candidate_bytes
from scripts.acceptance.workspaces.checkpoint_records import canonical_checkpoint_receipt_bytes
from scripts.acceptance.workspaces.checkpoint_records import canonical_checkpoint_removal_receipt_bytes
from scripts.acceptance.workspaces.checkpoint_records import canonical_checkpoint_removal_reference_bytes
from scripts.acceptance.workspaces.checkpoint_records import checkpoint_removal_name
from scripts.acceptance.workspaces.checkpoint_records import checkpoint_removal_record
from scripts.acceptance.workspaces.checkpoint_records import require_receipt_closure
from scripts.acceptance.workspaces.checkpoint_records import resolved_closure_kind
from scripts.acceptance.workspaces.specification import _canonical_operation_guard_bytes
from scripts.acceptance.workspaces.specification import _canonical_operation_receipt_bytes
from scripts.acceptance.workspaces.specification import _canonical_operation_request_bytes
from scripts.acceptance.workspaces.specification import _canonical_operation_result_bytes
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
            # Artifacts are flat regular files. iterdir reports an unreadable
            # directory, unlike rglob, which silently scans it as empty.
            try:
                artifacts = tuple(entry.iterdir())
            except OSError as error:
                raise AcceptanceFailure(
                    f"cannot inspect workspace operation artifacts for {operation}") from error
            for artifact in artifacts:
                try:
                    artifact_metadata = artifact.lstat()
                except OSError as error:
                    raise AcceptanceFailure(
                        "cannot inspect workspace operation artifact "
                        f"{operation}/artifacts/{artifact.name}") from error
                require(not stat.S_ISLNK(artifact_metadata.st_mode) and
                        stat.S_ISREG(artifact_metadata.st_mode),
                        f"workspace operation artifact is not a regular file: {operation}")
                require(artifact_metadata.st_size <= MAX_PROVIDER_BYTES,
                        f"workspace operation artifact exceeds the acceptance bound: {operation}")
            continue
        allowed = (name == "request.json" or name in _WORKSPACE_OPERATION_GUARDS or
                   name in _WORKSPACE_OPERATION_LOCKS or
                   name in _WORKSPACE_LIFETIME_RECORDS or
                   _lifetime_launch_sequence("workspace", name) is not None or
                   _checkpoint_record(name))
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
    if not (isinstance(wait, dict) or (
        isinstance(termination, dict) and termination.get("cause") == "previous-boot")):
        raise AcceptanceFailure(f"workspace operation WorkerWait proof is missing: {operation}")
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
    names, request, request_digest = _read_workspace_operation(directory, root, operation)
    closure = _workspace_operation_closure(
        directory, root, operation, request, request_digest, names)
    if request["kind"] == "checkpoint":
        return {**closure, "checkpoint": _checkpoint_creation_evidence(
            directory, root, request, request_digest, names, closure)}
    require(not any(_checkpoint_record(name) for name in names),
            f"workspace operation has checkpoint records for kind {request['kind']}: {operation}")
    if request["kind"] == "checkpoint-remove":
        return {**closure, "checkpoint_removal": _checkpoint_removal_registration(
            directory, root, request, request_digest, names, closure)}
    return closure


def _read_workspace_operation(
        directory: Path, root: str, operation: str,
        ) -> tuple[set[str], dict[str, object], str]:
    """Read one operation's bounded namespace and its exact canonical request."""
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
    return names, request, request_digest


def _workspace_operation_closure(
        directory: Path, root: str, operation: str, request: dict[str, object],
        request_digest: str, names: set[str],
        ) -> dict[str, object]:
    """Classify the generic lifetime closure shared by every operation kind."""
    _validate_workspace_operation_records(
        directory, root, operation, request, request_digest, names)
    deadline_text = request["deadline"]
    deadline = _canonical_timestamp(
        deadline_text, f"workspace operation deadline for {operation}")
    if not isinstance(deadline_text, str):
        raise AcceptanceFailure(f"workspace operation deadline for {operation} is not a timestamp")
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
            directory, root, operation, request_digest, deadline_text, names)
    if has_native_authority or has_guard or has_artifacts:
        return _workspace_operation_native_closure(
            directory, root, operation, request_digest, deadline,
            deadline_text, names)
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


def _checkpoint_record(name: str) -> bool:
    """Recognize checkpoint names; a malformed removal-family name is refused."""
    return name in CHECKPOINT_CREATION_RECORDS or checkpoint_removal_record(name) is not None


def _operation_closure_proof(directory: Path, closure: dict[str, object]) -> ClosureProof:
    """Project the validated lifetime records as guardian.ReadAuxiliaryClosureProof does."""
    mode = closure["mode"]
    require(mode != "unsubmitted",
            f"workspace operation has a checkpoint receipt without closure: {directory.name}")
    if mode == "unlaunched":
        return ClosureProof(kind="", unlaunched_sha256=str(closure["unlaunched_sha256"]))
    lifetime = closure["managed_lifetime"]
    if not isinstance(lifetime, dict):
        raise AcceptanceFailure(f"workspace operation has no managed lifetime: {directory.name}")
    termination = lifetime["termination"]
    wait = lifetime["worker_wait"]
    return ClosureProof(
        kind=resolved_closure_kind(
            registered=lifetime["registration"] is not None, worker_wait=wait),
        termination_sha256="" if termination is None else digest(
            directory / "lifetime-termination.json"),
        termination_cause="" if termination is None else str(termination["cause"]),
        worker_wait_sha256="" if wait is None else digest(
            directory / "lifetime-worker-wait.json"))


def _checkpoint_creation_evidence(
        directory: Path, root: str, request: dict[str, object], request_digest: str,
        names: set[str], closure: dict[str, object],
        ) -> dict[str, object]:
    """Validate one checkpoint journal's candidate, receipt and removal attempts."""
    operation = directory.name
    candidate = None
    if CHECKPOINT_CANDIDATE_RECORD in names:
        label = f"checkpoint candidate for {operation}"
        candidate, _ = _read_canonical_control_record(
            directory / CHECKPOINT_CANDIDATE_RECORD, label,
            lambda value: canonical_checkpoint_candidate_bytes(
                value, request, request_digest, label))
    status = None
    if CHECKPOINT_RECEIPT_RECORD in names:
        label = f"checkpoint receipt for {operation}"
        receipt, _ = _read_canonical_control_record(
            directory / CHECKPOINT_RECEIPT_RECORD, label,
            lambda value: canonical_checkpoint_receipt_bytes(
                value, request, request_digest, label))
        require("candidate" not in receipt or receipt["candidate"] == candidate,
                f"{label} candidate differs from the stored candidate")
        status = receipt["status"]
        require_receipt_closure(
            receipt, _operation_closure_proof(directory, closure),
            success=status == "checkpointed", label=label)
    removals = [
        _checkpoint_removal_outcome(
            directory, root, request_digest, attempt, names,
            candidate_present=candidate is not None)
        for attempt in _checkpoint_removal_attempts(directory, names)
    ]
    # The coordinator registers a new attempt only after the head holds an
    # unavailable receipt, so every attempt before the head must keep one.
    require(all(removal["status"] == "unavailable" for removal in removals[:-1]),
            f"checkpoint removal history has a prior attempt without an unavailable receipt: {operation}")
    return {"candidate": candidate is not None, "status": status, "removals": removals}


def _checkpoint_removal_attempts(
        directory: Path, names: set[str]) -> list[dict[str, object]]:
    """Read registered attempts 1..N as Go's readCheckpointRemovalAttemptsTx does."""
    checkpoint = directory.name
    families: dict[str, set[int]] = {"attempt": set(), "receipt": set()}
    for name in names:
        record = checkpoint_removal_record(name)
        if record is not None:
            families[record[0]].add(record[1])
    head = len(families["attempt"])
    require(all(sequence <= head for sequence in families["attempt"]),
            f"checkpoint removal attempts are not contiguous: {checkpoint}")
    require(all(sequence <= head for sequence in families["receipt"]),
            f"checkpoint removal receipt has no registered attempt: {checkpoint}")
    attempts: list[dict[str, object]] = []
    for sequence in range(1, head + 1):
        label = f"checkpoint removal attempt {sequence} for {checkpoint}"
        attempt, _ = _read_canonical_control_record(
            directory / checkpoint_removal_name("attempt", sequence), label,
            lambda value, number=sequence, record=label:
                canonical_checkpoint_removal_reference_bytes(value, checkpoint, number, record))
        require(all(attempt["operation_id"] != prior["operation_id"] for prior in attempts),
                f"{label} repeats a registered removal operation")
        attempts.append(attempt)
    return attempts


def _checkpoint_removal_outcome(
        directory: Path, root: str, request_digest: str, attempt: dict[str, object],
        names: set[str], *, candidate_present: bool,
        ) -> dict[str, object]:
    """Bind one registered attempt to its removal request, closure and receipt."""
    removal = str(attempt["operation_id"])
    removal_directory = directory.parent / removal
    removal_names, removal_request, removal_digest = _read_workspace_operation(
        removal_directory, root, removal)
    require(removal_request["kind"] == "checkpoint-remove" and
            removal_request["subject"] == {
                "kind": "operation", "id": directory.name, "sha256": request_digest} and
            removal_digest == attempt["request_sha256"],
            f"checkpoint removal attempt does not bind its removal request: {removal}")
    sequence = attempt["sequence"]
    if type(sequence) is not int:
        raise AcceptanceFailure("checkpoint removal attempt sequence is invalid")
    name = checkpoint_removal_name("receipt", sequence)
    status = None
    if name in names:
        label = f"checkpoint removal receipt {attempt['sequence']} for {directory.name}"
        receipt, _ = _read_canonical_control_record(
            directory / name, label,
            lambda value: canonical_checkpoint_removal_receipt_bytes(value, attempt, label))
        status = receipt["status"]
        require(status != "removed" or candidate_present,
                f"{label} removed a checkpoint without a candidate")
        removal_closure = _workspace_operation_closure(
            removal_directory, root, removal, removal_request, removal_digest, removal_names)
        require_receipt_closure(
            receipt, _operation_closure_proof(removal_directory, removal_closure),
            success=status in {"removed", "nothing_to_remove"}, label=label)
    return {"sequence": attempt["sequence"], "operation_id": removal, "status": status}


def _checkpoint_removal_registration(
        directory: Path, root: str, request: dict[str, object], request_digest: str,
        names: set[str], closure: dict[str, object],
        ) -> dict[str, object]:
    """Bind a removal journal to its checkpoint's registered attempt.

    An unregistered removal request must never be submitted, so a guard or any
    native lifetime fact requires its exact attempt in the original journal.
    """
    operation = directory.name
    subject = request["subject"]
    if not isinstance(subject, dict):
        raise AcceptanceFailure(f"checkpoint removal request subject is invalid: {operation}")
    checkpoint = str(subject["id"])
    creation = directory.parent / checkpoint
    submitted = bool(names & _WORKSPACE_OPERATION_GUARDS) or "managed_lifetime" in closure
    if not submitted and not creation.exists() and not creation.is_symlink():
        return {"checkpoint_id": checkpoint, "sequence": None}
    creation_names, creation_request, creation_digest = _read_workspace_operation(
        creation, root, checkpoint)
    require(creation_request["kind"] == "checkpoint" and subject["sha256"] == creation_digest,
            f"checkpoint removal names a foreign checkpoint request: {operation}")
    registered = [attempt for attempt in _checkpoint_removal_attempts(creation, creation_names)
                  if attempt["operation_id"] == operation]
    require(not registered or registered[0]["request_sha256"] == request_digest,
            f"checkpoint removal attempt does not bind this request: {operation}")
    require(registered or not submitted,
            f"unregistered checkpoint removal was submitted: {operation}")
    return {"checkpoint_id": checkpoint,
            "sequence": registered[0]["sequence"] if registered else None}
