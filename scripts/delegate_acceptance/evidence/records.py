"""Read bounded typed control records, process JSON, and root identity."""

from __future__ import annotations

import json
from datetime import datetime
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.json_decoder import unique_object
from scripts.delegate_acceptance.evidence.limits import MAX_CONTROL_BYTES, MAX_PROVIDER_BYTES
from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _exact_keys,
    _task_identity,
    require,
)
from scripts.delegate_acceptance.supervisor.queue_policy import _validate_pueue_status
from pathlib import Path
from typing import Callable


def _read_strict_json(path: Path, bound: int) -> object:
    """Read bounded JSON with the same alias rejection as the Go decoder."""
    try:
        with Path(path).open("rb") as stream:
            data = stream.read(bound + 1)
    except OSError:
        raise
    require(len(data) <= bound, "JSON evidence exceeds bound: " + str(path))

    def reject_constant(value: str) -> object:
        raise ValueError("non-standard JSON constant: " + value)

    try:
        return json.loads(data, object_pairs_hook=unique_object,
                          parse_constant=reject_constant)
    except (json.JSONDecodeError, UnicodeDecodeError, RecursionError, TypeError, ValueError) as error:
        raise AcceptanceFailure("invalid JSON evidence: " + str(path) + ": " + str(error)) from error


def parse_json_output(process: object, label: str, bound: int = MAX_CONTROL_BYTES) -> dict[str, object]:
    """Read one bounded JSON object from an owned process's stdout."""
    try:
        value = _read_strict_json(process.directory / "stdout", bound)
    except (OSError, ValueError, TypeError, RecursionError) as error:
        raise AcceptanceFailure(f"{label} did not produce one JSON object: {error}") from error
    require(isinstance(value, dict), f"{label} JSON response must be an object")
    return value


def status_jobs(process: object) -> dict[str, object]:
    """Read one bounded pueue status object from an owned client."""
    result = getattr(process, "result", None)
    require(isinstance(result, dict) and result.get("natural_wait") is True and
            type(result.get("exit_code")) is int and result.get("exit_code") == 0,
            "pueue status did not exit successfully")
    value = parse_json_output(process, "pueue status", MAX_PROVIDER_BYTES)
    _validate_pueue_status(value)
    return value


def bounded_text(path: Path, label: str, bound: int = MAX_CONTROL_BYTES) -> str:
    """Read bounded UTF-8 text before any whitespace normalization."""
    require(type(bound) is int and bound > 0, label + " bound is invalid")
    with Path(path).open("rb") as stream:
        data = stream.read(bound + 1)
    require(len(data) <= bound, label + " exceeds the bounded output limit")
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError as error:
        raise AcceptanceFailure(label + " is not valid UTF-8") from error


def _read_canonical_control_record(
        path: Path, label: str, encoder: Callable[[object], bytes],
        ) -> tuple[dict[str, object], str]:
    """Read one bounded record and require the exact Go canonical bytes."""
    value = _read_control_record(path, label)
    try:
        data = path.read_bytes()
    except OSError as error:
        raise AcceptanceFailure(f"cannot read {label}") from error
    require(len(data) <= MAX_CONTROL_BYTES, f"{label} exceeds the control bound")
    require(data == encoder(value), f"{label} is not canonical Go JSON")
    return value, sha(data)


def _state_root_identity(state: Path, label: str) -> tuple[str, tuple[datetime, int]]:
    """Read the immutable root identity used by task and inspection records."""
    root = _read_control_record(state / "root.json", label)
    _exact_keys(root, {"schema_version", "root_id", "created_at"}, label=label)
    require(type(root.get("schema_version")) is int and root["schema_version"] == 1,
            f"{label} schema mismatch")
    created_at = _canonical_timestamp(root.get("created_at"), f"{label} created_at")
    root_id = root.get("root_id")
    require(_task_identity(root_id), f"{label} has no valid root identity")
    return root_id, created_at


def _read_control_record(path: Path, label: str) -> dict[str, object]:
    require(path.is_file() and not path.is_symlink(), f"{label} is not a regular record")
    require(path.stat().st_size <= MAX_CONTROL_BYTES, f"{label} exceeds the control bound")
    value = _read_strict_json(path, MAX_CONTROL_BYTES)
    require(isinstance(value, dict), f"{label} is not an object")
    return value


def _read_optional_control_record(path: Path, label: str) -> dict[str, object] | None:
    if not path.exists() and not path.is_symlink():
        return None
    return _read_control_record(path, label)
