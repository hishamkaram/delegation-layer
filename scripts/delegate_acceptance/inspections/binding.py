"""Pure inspection binding, admission window, and group eligibility rules."""

from __future__ import annotations

import posixpath
from datetime import datetime
from scripts.delegate_acceptance.evidence.timestamps import (
    _canonical_timestamp,
    _timestamp_at_or_after,
    _timestamp_before,
)
from scripts.delegate_acceptance.evidence.validation import _exact_keys, _hex_digest, require
from scripts.delegate_acceptance.paths.policy import historical_clean_absolute_path
from scripts.delegate_acceptance.supervisor.binding import _validate_supervisor_binding
from pathlib import Path


INSPECTION_GROUP_PREFIX = "delegation-inspection-"


ADMISSION_INSPECTION_TIMEOUT_SECONDS = (60, 20)


MODEL_DISCOVERY_TIMEOUT_SECONDS = (300, 60)


_RUNTIME_INSPECTION_REVISION = "runtime-capability-v1"


_INSPECTION_BINDING_KEYS = {
    "definition_revision", "definition_sha256", "helper_executable", "helper_sha256",
    "worker_executable", "worker_sha256", "supervisor",
}


_INSPECTION_BINDING_OPTIONAL_KEYS = {
    "capability_sha256", "environment", "environment_recorded", "runner_ownership",
    "worker_exit_proof",
}


def _inspection_group(root_id: str) -> str:
    return INSPECTION_GROUP_PREFIX + root_id


def _inspection_label(root_id: str, task_id: str) -> str:
    return _inspection_group(root_id) + "-" + task_id


def _is_private_supervisor_config(state: Path, config_path: object) -> bool:
    """Mirror pueue.IsPrivateConfig for an already validated state root."""
    private_path = state / ".supervisor" / "pueue.yml"
    return (state.is_absolute() and state == Path(posixpath.normpath(state)) and
            isinstance(config_path, str) and Path(config_path) == private_path)


def _uses_concurrent_inspection_group(state: Path, binding: dict[str, object]) -> bool:
    """Mirror concurrentAdmissionInspection's runtime/private capability gate."""
    supervisor = binding.get("supervisor")
    require(isinstance(supervisor, dict), "inspection supervisor binding is invalid")
    capability = binding.get("capability_sha256")
    return (binding.get("definition_revision") == _RUNTIME_INSPECTION_REVISION and
            isinstance(capability, str) and bool(capability) and
            _is_private_supervisor_config(state, supervisor.get("config_path")))


def _inspection_group_parallelism(
        state: Path, root: str, binding: dict[str, object], group: object,
        label: str) -> int:
    """Validate the saved Go inspection group and return its expected width."""
    if group == _inspection_group(root):
        return 1
    require(group == "default" and _uses_concurrent_inspection_group(state, binding),
            f"{label} group is not an eligible private default group")
    return 0


def _validate_inspection_binding(value: object, label: str) -> None:
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _INSPECTION_BINDING_KEYS, _INSPECTION_BINDING_OPTIONAL_KEYS, label)
    require(isinstance(value.get("definition_revision"), str) and
            value["definition_revision"], f"{label} definition revision is invalid")
    for key in ("definition_sha256", "helper_sha256", "worker_sha256"):
        require(_hex_digest(value.get(key)), f"{label} {key} is invalid")
    if "capability_sha256" in value:
        require(_hex_digest(value["capability_sha256"]), f"{label} capability_sha256 is invalid")
    if "runner_ownership" in value:
        require(value["runner_ownership"] in {"managed", "custom"},
                f"{label} runner ownership is invalid")
    if "worker_exit_proof" in value:
        require(value["worker_exit_proof"] == "guardian-wait-v1",
                f"{label} worker exit proof is invalid")
    if "environment_recorded" in value:
        require(type(value["environment_recorded"]) is bool,
                f"{label} environment_recorded is invalid")
    for key in ("helper_executable", "worker_executable"):
        path = value.get(key)
        require(historical_clean_absolute_path(path),
                f"{label} {key} is invalid")
    if "environment" in value:
        environment = value["environment"]
        require(isinstance(environment, list) and
                all(isinstance(entry, str) and "=" in entry and
                    entry.split("=", 1)[0] for entry in environment),
                f"{label} environment is invalid")
    _validate_supervisor_binding(value.get("supervisor"), label + " supervisor")


def _validate_claim_record(value: object, request_digest: str, label: str,
                           created: tuple[datetime, int], deadline: tuple[datetime, int]) -> tuple[datetime, int]:
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, {"schema_version", "request_sha256", "created_at"}, label=label)
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("request_sha256") == request_digest,
            f"{label} binding mismatch")
    claimed = _canonical_timestamp(value.get("created_at"), label + " created_at")
    require(_timestamp_at_or_after(claimed, created) and _timestamp_before(claimed, deadline),
            f"{label} is outside inspection admission window")
    return claimed
