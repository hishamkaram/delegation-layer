"""Pure workspace request, reservation, closure, and operation encoders."""

from __future__ import annotations

from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _exact_keys,
    _hex_digest,
    _task_identity,
    require,
)
from scripts.delegate_acceptance.paths.policy import _raw_clean_absolute_path
from scripts.delegate_acceptance.supervisor.binding import _canonical_supervisor_ref
from scripts.delegate_acceptance.tasks.specification import _MAX_INT64, _MAX_UINT64
from pathlib import Path


_OPERATION_REQUEST_REQUIRED_KEYS = {
    "schema_version", "root_id", "operation_id", "workspace_id", "kind", "subject",
    "deadline", "worker_executable", "worker_sha256", "supervisor",
}


_OPERATION_REQUEST_OPTIONAL_KEYS = {"arguments", "discard"}


_OPERATION_SUBJECT_KEYS = {"kind", "id", "sha256"}


_OPERATION_KINDS = {"provision", "capture", "verify", "remove", "forget"}


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
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _WORKSPACE_SUBJECT_KEYS, label=label)
    kind = value.get("kind")
    subject_id = value.get("id")
    request_sha256 = value.get("sha256")
    require(kind in {"task", "operation", "admission"}, f"{label} kind is invalid")
    require(_task_identity(subject_id), f"{label} id is invalid")
    require(_hex_digest(request_sha256), f"{label} sha256 is invalid")
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
    require(isinstance(value, dict), f"{label} is not an object")
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
    require(isinstance(value, dict), f"{label} is not an object")
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
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _WORKSPACE_CLOSURE_KEYS, _WORKSPACE_CLOSURE_OPTIONAL_KEYS, label)
    stored_sequence = value.get("sequence")
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("workspace_id") == workspace and stored_sequence == sequence and
            type(stored_sequence) is int and 0 < stored_sequence <= _MAX_UINT64,
            f"{label} sequence is invalid")
    require(type(sequence) is int and 0 < sequence <= _MAX_UINT64,
            f"{label} expected sequence is invalid")
    disposition = value.get("disposition")
    require(disposition in {"captured", "unavailable", "aborted-not-launched",
                            "operation-completed"},
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
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _OPERATION_SUBJECT_KEYS, label=label)
    kind = value.get("kind")
    subject_id = value.get("id")
    subject_sha256 = value.get("sha256")
    require(kind in {"task", "operation", "admission"}, f"{label} kind is invalid")
    require(_task_identity(subject_id), f"{label} id is invalid")
    require(_hex_digest(subject_sha256), f"{label} sha256 is invalid")
    return {"kind": kind, "id": subject_id, "sha256": subject_sha256}


def _canonical_operation_request_bytes(
        value: object, root: str, operation: str, label: str = "workspace operation request",
        ) -> bytes:
    """Validate one exact workspace.OperationRequest and marshal its Go bytes."""
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _OPERATION_REQUEST_REQUIRED_KEYS, _OPERATION_REQUEST_OPTIONAL_KEYS, label)
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1,
            f"{label} schema mismatch")
    operation_id = value.get("operation_id")
    workspace_id = value.get("workspace_id")
    require(_task_identity(root) and value.get("root_id") == root,
            f"{label} root identity mismatch")
    require(operation_id == operation and _task_identity(operation_id),
            f"{label} operation identity mismatch")
    require(_task_identity(workspace_id), f"{label} workspace identity is invalid")
    kind = value.get("kind")
    require(kind in _OPERATION_KINDS, f"{label} kind is invalid")
    subject = _canonical_operation_subject(value.get("subject"), label + " subject")
    deadline_value = value.get("deadline")
    _canonical_timestamp(deadline_value, label + " deadline")
    worker = value.get("worker_executable")
    require(_raw_clean_absolute_path(worker), f"{label} worker_executable is invalid")
    worker_sha256 = value.get("worker_sha256")
    require(_hex_digest(worker_sha256), f"{label} worker_sha256 is invalid")
    supervisor = _canonical_supervisor_ref(value.get("supervisor"), label + " supervisor")

    arguments = value.get("arguments")
    if "arguments" in value:
        require(isinstance(arguments, list) and not isinstance(arguments, tuple) and arguments,
                f"{label} arguments must be a nonempty array when present")
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
        "worker_sha256": worker_sha256,
        "supervisor": supervisor,
    }
    if arguments:
        canonical["arguments"] = arguments
    if discard:
        canonical["discard"] = True
    return canonical_go_json(canonical)


def _canonical_operation_guard_bytes(value: object, request_digest: str, label: str) -> bytes:
    """Validate one create-once workspace operation guard."""
    require(isinstance(value, dict), f"{label} is not an object")
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
    require(isinstance(value, dict), f"{label} is not an object")
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
    require(isinstance(value, dict), f"{label} is not an object")
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
    require(type(length) is int and length >= 0, f"{label} length is invalid")
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
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, {"schema_version", "operation_id", "request_sha256", "status"},
                {"reason_code", "command_exit", "artifacts"}, label)
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("operation_id") == operation and
            value.get("request_sha256") == request_digest and
            value.get("status") in {"succeeded", "failed"},
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
        require(isinstance(artifacts, list) and artifacts,
                f"{label} artifacts must be nonempty when present")
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
