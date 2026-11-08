"""Pure task and provider binding shape validation."""

from __future__ import annotations

from scripts.acceptance.evidence.timestamps import _canonical_timestamp
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.validation import _exact_keys
from scripts.acceptance.evidence.validation import _hex_digest
from scripts.acceptance.evidence.validation import _task_identity
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.tasks.runtime_metadata import _provider_runtime_identity


def dispatch_binding(response: dict[str, object], task_id: str, label: str) -> tuple[str, int]:
    require(response.get("task_id") == task_id, f"{label} changed task identity")
    root_id = response.get("root_id")
    if not _task_identity(root_id):
        raise AcceptanceFailure(f"{label} returned no root identity")
    supervisor = response.get("supervisor")
    if not isinstance(supervisor, dict) or supervisor.get("matched") is not True:
        raise AcceptanceFailure(f"{label} has no matched supervisor identity")
    numeric_id = supervisor.get("numeric_task_id")
    if type(numeric_id) is not int or numeric_id < 0:
        raise AcceptanceFailure(f"{label} has no numeric supervisor identity")
    return root_id, numeric_id


def _validate_bound_provider_record(
        evidence: object, name: str, root_id: str, task_id: str,
        meta_digest: str, spec_digest: object) -> dict[str, object]:
    """Validate the durable envelope shared by interrupted provider records."""
    label = f"task {task_id} {name}"
    if not isinstance(evidence, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    if name == "provider.start":
        _exact_keys(evidence, {
            "schema_version", "root_id", "task_id", "spec_sha256",
            "meta_sha256", "budget_nanos", "created_at",
        }, label=label)
    elif name == "provider.started.json":
        _exact_keys(evidence, {
            "schema_version", "root_id", "task_id", "spec_sha256",
            "meta_sha256", "started_at", "diagnostic_nanos",
            "provider_executable",
        }, optional={"provider_version", "provider_sha256"}, label=label)
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
        budget = evidence.get("budget_nanos")
        require(type(budget) is int and 0 < budget < 2 ** 63,
                f"{label} budget_nanos is invalid")
        _canonical_timestamp(evidence.get("created_at"), f"{label} created_at")
    else:
        diagnostic = evidence.get("diagnostic_nanos")
        require(type(diagnostic) is int and 0 <= diagnostic < 2 ** 63,
                f"{label} diagnostic_nanos is invalid")
        _canonical_timestamp(evidence.get("started_at"), f"{label} started_at")
        _provider_runtime_identity(evidence, "started", label)
    return evidence
