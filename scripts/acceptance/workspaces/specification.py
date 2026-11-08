"""Pure workspace request, reservation, closure, and operation encoders."""

from __future__ import annotations

from scripts.acceptance.evidence.encoding import canonical_go_json
from scripts.acceptance.evidence.timestamps import _canonical_timestamp
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.validation import _exact_keys
from scripts.acceptance.evidence.validation import _hex_digest
from scripts.acceptance.evidence.validation import _task_identity
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.paths.policy import _raw_clean_absolute_path
from scripts.acceptance.supervisor.binding import _canonical_supervisor_ref
from scripts.acceptance.tasks.specification import _MAX_INT64
from scripts.acceptance.tasks.specification import _MAX_UINT64
from pathlib import Path


_OPERATION_REQUEST_REQUIRED_KEYS = {
    "schema_version", "root_id", "operation_id", "workspace_id", "kind", "subject",
    "deadline", "worker_executable", "supervisor",
}


_OPERATION_REQUEST_OPTIONAL_KEYS = {
    "arguments", "discard", "checkpoint", "worker_sha256", "executable_identity",
}


_OPERATION_SUBJECT_KEYS = {"kind", "id", "sha256"}


_OPERATION_KINDS = {
    "provision", "capture", "verify", "remove", "forget", "checkpoint", "checkpoint-remove",
}


_CHECKPOINT_SELECTION_OPTIONAL_KEYS = {"cwd", "cwd_identity"}


_DIRECTORY_IDENTITY_KEYS = {"device", "inode"}


_RESOURCE_REQUEST_KEYS = {
    "schema_version", "root_id", "workspace_id", "task_id", "request_sha256",
    "source_cwd", "base_ref", "container", "budget_nanos",
}


_WORKSPACE_SUBJECT_KEYS = {"kind", "id", "sha256"}


_WORKSPACE_RESERVATION_KEYS = {"schema_version", "workspace_id", "sequence", "subject"}


_WORKSPACE_CLOSURE_KEYS = {
    "schema_version", "workspace_id", "sequence", "disposition",
}


_WORKSPACE_CLOSURE_OPTIONAL_KEYS = {"capture_id"}


def _canonical_workspace_subject(
        value: object, label: str, *, expected_kind: str | None = None,
        expected_id: str | None = None, expected_sha256: str | None = None,
        ) -> dict[str, str]:
    """Validate one workspace Subject and return its Go field order."""
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(value, _WORKSPACE_SUBJECT_KEYS, label=label)
    kind = value.get("kind")
    subject_id = value.get("id")
    request_sha256 = value.get("sha256")
    if not isinstance(kind, str) or kind not in {"task", "operation", "admission"}:
        raise AcceptanceFailure(f"{label} kind is invalid")
    if not _task_identity(subject_id):
        raise AcceptanceFailure(f"{label} id is invalid")
    if not _hex_digest(request_sha256):
        raise AcceptanceFailure(f"{label} sha256 is invalid")
    if expected_kind is not None:
        require(kind == expected_kind, f"{label} kind does not match its owner")
    if expected_id is not None:
        require(subject_id == expected_id, f"{label} id does not match its owner")
    if expected_sha256 is not None:
        require(request_sha256 == expected_sha256,
                f"{label} sha256 does not match its owner")
    return {"kind": kind, "id": subject_id, "sha256": request_sha256}


def _canonical_workspace_resource_request_bytes(
        value: object, root: str, workspace: str, label: str) -> bytes:
    """Validate and marshal workspace.ResourceRequest exactly as Go does."""
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(value, _RESOURCE_REQUEST_KEYS, label=label)
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("root_id") == root and value.get("workspace_id") == workspace,
            f"{label} identity is invalid")
    require(_task_identity(value.get("task_id")), f"{label} task_id is invalid")
    require(_hex_digest(value.get("request_sha256")), f"{label} request_sha256 is invalid")
    for key in ("source_cwd", "container"):
        path = value.get(key)
        require(_raw_clean_absolute_path(path) and "\x00" not in path,
                f"{label} {key} is invalid")
    base_ref = value.get("base_ref")
    require(isinstance(base_ref, str) and "\x00" not in base_ref and
            "\r" not in base_ref and "\n" not in base_ref,
            f"{label} base_ref is invalid")
    budget = value.get("budget_nanos")
    require(type(budget) is int and 0 < budget <= _MAX_INT64,
            f"{label} budget_nanos is invalid")
    return canonical_go_json({
        "schema_version": value["schema_version"],
        "root_id": value["root_id"],
        "workspace_id": value["workspace_id"],
        "task_id": value["task_id"],
        "request_sha256": value["request_sha256"],
        "source_cwd": value["source_cwd"],
        "base_ref": value["base_ref"],
        "container": value["container"],
        "budget_nanos": value["budget_nanos"],
    })


def _canonical_workspace_reservation_bytes(
        value: object, workspace: str, sequence: int, task: str,
        request_sha256: str, label: str) -> bytes:
    """Validate one immutable admission reservation and marshal its bytes."""
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(value, _WORKSPACE_RESERVATION_KEYS, label=label)
    stored_sequence = value.get("sequence")
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("workspace_id") == workspace and stored_sequence == sequence and
            type(stored_sequence) is int and 0 < stored_sequence <= _MAX_UINT64,
            f"{label} sequence is invalid")
    require(type(sequence) is int and 0 < sequence <= _MAX_UINT64,
            f"{label} expected sequence is invalid")
    subject = _canonical_workspace_subject(
        value.get("subject"), f"{label} subject", expected_kind="admission",
        expected_id=task, expected_sha256=request_sha256)
    return canonical_go_json({
        "schema_version": value["schema_version"],
        "workspace_id": value["workspace_id"],
        "sequence": stored_sequence,
        "subject": subject,
    })


def _canonical_workspace_closure_bytes(
        value: object, workspace: str, sequence: int, label: str) -> bytes:
    """Validate one immutable workspace reservation closure."""
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(value, _WORKSPACE_CLOSURE_KEYS, _WORKSPACE_CLOSURE_OPTIONAL_KEYS, label)
    stored_sequence = value.get("sequence")
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("workspace_id") == workspace and stored_sequence == sequence and
            type(stored_sequence) is int and 0 < stored_sequence <= _MAX_UINT64,
            f"{label} sequence is invalid")
    require(type(sequence) is int and 0 < sequence <= _MAX_UINT64,
            f"{label} expected sequence is invalid")
    disposition = value.get("disposition")
    require(isinstance(disposition, str) and disposition in {
                "captured", "unavailable", "aborted-not-launched", "operation-completed"},
            f"{label} disposition is invalid")
    capture_id = value.get("capture_id")
    if "capture_id" in value:
        require(_task_identity(capture_id), f"{label} capture_id is invalid")
    canonical: dict[str, object] = {
        "schema_version": value["schema_version"],
        "workspace_id": value["workspace_id"],
        "sequence": stored_sequence,
        "disposition": disposition,
    }
    if capture_id is not None:
        canonical["capture_id"] = capture_id
    return canonical_go_json(canonical)


def _canonical_operation_subject(value: object, label: str) -> dict[str, str]:
    """Validate and order a workspace.Subject record."""
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(value, _OPERATION_SUBJECT_KEYS, label=label)
    kind = value.get("kind")
    subject_id = value.get("id")
    subject_sha256 = value.get("sha256")
    if not isinstance(kind, str) or kind not in {"task", "operation", "admission"}:
        raise AcceptanceFailure(f"{label} kind is invalid")
    if not _task_identity(subject_id):
        raise AcceptanceFailure(f"{label} id is invalid")
    if not _hex_digest(subject_sha256):
        raise AcceptanceFailure(f"{label} sha256 is invalid")
    return {"kind": kind, "id": subject_id, "sha256": subject_sha256}


def _canonical_operation_request_bytes(
        value: object, root: str, operation: str, label: str = "workspace operation request",
        ) -> bytes:
    """Validate one exact workspace.OperationRequest and marshal its Go bytes."""
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(value, _OPERATION_REQUEST_REQUIRED_KEYS, _OPERATION_REQUEST_OPTIONAL_KEYS, label)
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1,
            f"{label} schema mismatch")
    operation_id = value.get("operation_id")
    workspace_id = value.get("workspace_id")
    require(_task_identity(root) and value.get("root_id") == root,
            f"{label} root identity mismatch")
    require(operation_id == operation and _task_identity(operation_id),
            f"{label} operation identity mismatch")
    kind = value.get("kind")
    if not isinstance(kind, str) or kind not in _OPERATION_KINDS:
        raise AcceptanceFailure(f"{label} kind is invalid")
    subject = _canonical_operation_subject(value.get("subject"), label + " subject")
    checkpoint = _canonical_operation_scope(value, kind, subject, label)
    deadline_value = value.get("deadline")
    _canonical_timestamp(deadline_value, label + " deadline")
    worker = value.get("worker_executable")
    require(_raw_clean_absolute_path(worker), f"{label} worker_executable is invalid")
    identity = value.get("executable_identity", "")
    require(identity in ("", "installed-path-v1"),
            f"{label} executable_identity is invalid")
    worker_sha256 = value.get("worker_sha256", "")
    if identity == "installed-path-v1":
        require(isinstance(worker_sha256, str) and worker_sha256 == "",
                f"{label} installed worker carries a legacy digest")
    else:
        require(_hex_digest(worker_sha256), f"{label} worker_sha256 is invalid")
    supervisor = _canonical_supervisor_ref(value.get("supervisor"), label + " supervisor")

    arguments = value.get("arguments")
    if "arguments" in value:
        if not isinstance(arguments, list) or not arguments:
            raise AcceptanceFailure(f"{label} arguments must be a nonempty array when present")
        require(all(isinstance(argument, str) and "\x00" not in argument
                    for argument in arguments),
                f"{label} arguments are invalid")
    else:
        arguments = []
    if kind == "verify":
        require(arguments and arguments[0] != "", f"{label} verify arguments are missing")
    else:
        require(not arguments, f"{label} arguments are not allowed for {kind}")

    discard = value.get("discard", False)
    if "discard" in value:
        require(discard is True, f"{label} discard must be true when present")
    require(not discard or kind == "remove", f"{label} discard is invalid for {kind}")

    canonical: dict[str, object] = {
        "schema_version": value["schema_version"],
        "root_id": root,
        "operation_id": operation_id,
        "workspace_id": workspace_id,
        "kind": kind,
        "subject": subject,
        "deadline": deadline_value,
        "worker_executable": worker,
    }
    if worker_sha256:
        canonical["worker_sha256"] = worker_sha256
    if identity:
        canonical["executable_identity"] = identity
    canonical["supervisor"] = supervisor
    if arguments:
        canonical["arguments"] = arguments
    if discard:
        canonical["discard"] = True
    if checkpoint is not None:
        canonical["checkpoint"] = checkpoint
    return canonical_go_json(canonical)


def _canonical_operation_scope(
        value: dict[str, object], kind: str, subject: dict[str, str], label: str,
        ) -> dict[str, object] | None:
    """Mirror contract.validateOperationScope and return the canonical selection.

    workspace_id stays a required encoded field. It is empty only for an
    ordinary checkpoint and a checkpoint removal; every other kind, including a
    managed checkpoint, carries a valid workspace identity.
    """
    workspace_id = value.get("workspace_id")
    if kind == "checkpoint":
        require("checkpoint" in value, f"{label} checkpoint selection is missing")
        return _canonical_checkpoint_selection(value["checkpoint"], workspace_id, label)
    require("checkpoint" not in value, f"{label} checkpoint selection is not allowed for {kind}")
    if kind == "checkpoint-remove":
        require(workspace_id == "" and subject["kind"] == "operation",
                f"{label} checkpoint removal scope is invalid")
        return None
    require(_task_identity(workspace_id), f"{label} workspace identity is invalid")
    return None


def _canonical_checkpoint_selection(
        value: object, workspace_id: object, label: str) -> dict[str, object]:
    """Validate contract.CheckpointSelection and preserve its omitempty shape."""
    label = label + " checkpoint selection"
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(value, {"kind"}, _CHECKPOINT_SELECTION_OPTIONAL_KEYS, label)
    kind = value.get("kind")
    if kind == "managed":
        require(set(value) == {"kind"} and _task_identity(workspace_id),
                f"{label} managed scope is invalid")
        return {"kind": kind}
    require(kind == "ordinary", f"{label} kind is invalid")
    cwd = value.get("cwd")
    require(workspace_id == "" and _raw_clean_absolute_path(cwd) and "\x00" not in cwd,
            f"{label} ordinary scope is invalid")
    identity = value.get("cwd_identity")
    if not isinstance(identity, dict):
        raise AcceptanceFailure(f"{label} cwd identity is missing")
    _exact_keys(identity, _DIRECTORY_IDENTITY_KEYS, label=label + " cwd identity")
    for key in ("device", "inode"):
        require(type(identity.get(key)) is int and 0 <= identity[key] <= _MAX_UINT64,
                f"{label} cwd identity {key} is invalid")
    require(identity["inode"] != 0, f"{label} cwd identity is not pinned")
    return {"kind": kind, "cwd": cwd,
            "cwd_identity": {"device": identity["device"], "inode": identity["inode"]}}


def _canonical_operation_guard_bytes(value: object, request_digest: str, label: str) -> bytes:
    """Validate one create-once workspace operation guard."""
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(value, {"schema_version", "request_sha256"}, label=label)
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("request_sha256") == request_digest,
            f"{label} binding is invalid")
    return canonical_go_json({
        "schema_version": value["schema_version"],
        "request_sha256": value["request_sha256"],
    })


def _canonical_operation_receipt_bytes(value: object, request_digest: str, label: str) -> bytes:
    """Validate the numeric supervisor receipt for one workspace operation."""
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(value, {"schema_version", "request_sha256", "numeric_task_id"}, label=label)
    numeric = value.get("numeric_task_id")
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("request_sha256") == request_digest and
            type(numeric) is int and numeric >= 0,
            f"{label} binding is invalid")
    return canonical_go_json({
        "schema_version": value["schema_version"],
        "request_sha256": value["request_sha256"],
        "numeric_task_id": numeric,
    })


def _canonical_workspace_artifact(value: object, label: str) -> dict[str, object]:
    """Validate one workspace.Artifact and preserve its omitempty shape."""
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(value, {"name", "length"}, {"sha256", "omitted", "truncated"}, label)
    name = value.get("name")
    length = value.get("length")
    try:
        name_length = len(name.encode("utf-8")) if isinstance(name, str) else 0
    except UnicodeEncodeError as error:
        raise AcceptanceFailure(f"{label} name is not valid UTF-8") from error
    require(isinstance(name, str) and 0 < name_length <= 64 and
            name not in {".", ".."}
            and Path(name).name == name and "/" not in name and "\\" not in name
            and "\x00" not in name, f"{label} name is invalid")
    if type(length) is not int or length < 0:
        raise AcceptanceFailure(f"{label} length is invalid")
    sha256 = value.get("sha256")
    omitted = value.get("omitted")
    truncated = value.get("truncated", False)
    if "sha256" in value:
        require(_hex_digest(sha256), f"{label} sha256 is invalid")
    if "omitted" in value:
        require(omitted == "limit", f"{label} omitted is invalid")
    if "truncated" in value:
        require(truncated is True, f"{label} truncated is invalid")
    if omitted:
        require(length == 0 and sha256 is None and not truncated,
                f"{label} omitted retention is invalid")
    else:
        require(length <= 8 * 1024 * 1024 and _hex_digest(sha256),
                f"{label} retention is invalid")
    canonical: dict[str, object] = {"name": name, "length": length}
    if sha256 is not None:
        canonical["sha256"] = sha256
    if omitted:
        canonical["omitted"] = omitted
    if truncated:
        canonical["truncated"] = True
    return canonical


def _canonical_operation_result_bytes(
        value: object, operation: str, request_digest: str, kind: str, label: str,
        ) -> bytes:
    """Validate and marshal the workspace OperationResult record."""
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(value, {"schema_version", "operation_id", "request_sha256", "status"},
                {"reason_code", "command_exit", "artifacts"}, label)
    status = value.get("status")
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("operation_id") == operation and
            value.get("request_sha256") == request_digest and
            isinstance(status, str) and status in {"succeeded", "failed"},
            f"{label} binding is invalid")
    reason_code = value.get("reason_code")
    if "reason_code" in value:
        require(isinstance(reason_code, str) and reason_code,
                f"{label} reason_code is invalid")
    command_exit = value.get("command_exit")
    if "command_exit" in value:
        require(type(command_exit) is int and command_exit >= 0,
                f"{label} command_exit is invalid")
    if kind == "verify" and value["status"] == "succeeded":
        require("command_exit" in value, f"{label} successful verify has no command_exit")
    else:
        require("command_exit" not in value, f"{label} command_exit is not allowed")
    artifacts = value.get("artifacts")
    if "artifacts" in value:
        if not isinstance(artifacts, list) or not artifacts:
            raise AcceptanceFailure(f"{label} artifacts must be nonempty when present")
        artifacts = [_canonical_workspace_artifact(item, f"{label} artifact")
                     for item in artifacts]
    canonical: dict[str, object] = {
        "schema_version": value["schema_version"],
        "operation_id": value["operation_id"],
        "request_sha256": value["request_sha256"],
        "status": value["status"],
    }
    if reason_code is not None:
        canonical["reason_code"] = reason_code
    if command_exit is not None:
        canonical["command_exit"] = command_exit
    if artifacts is not None:
        canonical["artifacts"] = artifacts
    return canonical_go_json(canonical)
