"""Pure task and provider binding shape validation."""

from __future__ import annotations

from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure, _exact_keys, _hex_digest, _task_identity, require,
)
from scripts.delegate_acceptance.tasks.runtime_metadata import _provider_runtime_identity


def dispatch_binding(response: dict[str, object], task_id: str, label: str) -> tuple[str, int]:
    require(response.get("task_id") == task_id, f"{label} changed task identity")
    root_id = response.get("root_id")
    require(isinstance(root_id, str) and len(root_id) == 32 and
            all(character in "0123456789abcdef" for character in root_id),
            f"{label} returned no root identity")
    supervisor = response.get("supervisor")
    require(isinstance(supervisor, dict) and supervisor.get("matched") is True,
            f"{label} has no matched supervisor identity")
    numeric_id = supervisor.get("numeric_task_id")
    require(type(numeric_id) is int and numeric_id >= 0,
            f"{label} has no numeric supervisor identity")
    return root_id, numeric_id


def _validate_bound_provider_record(
        evidence: object, name: str, root_id: str, task_id: str,
        meta_digest: str, spec_digest: object) -> dict[str, object]:
    """Validate the durable envelope shared by interrupted provider records."""
    label = f"task {task_id} {name}"
    require(isinstance(evidence, dict), f"{label} is not an object")
    if name == "provider.start":
        _exact_keys(evidence, {
            "schema_version", "root_id", "task_id", "spec_sha256",
            "meta_sha256", "budget_nanos", "created_at",
        }, label=label)
    elif name == "provider.started.json":
        _exact_keys(evidence, {
            "schema_version", "root_id", "task_id", "spec_sha256",
            "meta_sha256", "started_at", "diagnostic_nanos",
            "provider_executable", "provider_version", "provider_sha256",
        }, label=label)
    else:
        raise AcceptanceFailure(f"{label} has no supported envelope")
    require(type(evidence.get("schema_version")) is int and
            evidence.get("schema_version") == 1 and
            _task_identity(evidence.get("root_id")) and
            evidence.get("root_id") == root_id and
            _task_identity(evidence.get("task_id")) and
            evidence.get("task_id") == task_id and
            _hex_digest(evidence.get("spec_sha256")) and
            evidence.get("spec_sha256") == spec_digest and
            _hex_digest(evidence.get("meta_sha256")) and
            evidence.get("meta_sha256") == meta_digest,
            f"{label} binding is invalid")
    if name == "provider.start":
        require(type(evidence.get("budget_nanos")) is int and
                0 < evidence.get("budget_nanos") < 2 ** 63,
                f"{label} budget_nanos is invalid")
        _canonical_timestamp(evidence.get("created_at"), f"{label} created_at")
    else:
        require(type(evidence.get("diagnostic_nanos")) is int and
                0 <= evidence.get("diagnostic_nanos") < 2 ** 63,
                f"{label} diagnostic_nanos is invalid")
        _canonical_timestamp(evidence.get("started_at"), f"{label} started_at")
        _provider_runtime_identity(evidence, "started", label)
    return evidence
