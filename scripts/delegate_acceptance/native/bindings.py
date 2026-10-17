"""Single owner for immutable per-task provider runtime identities."""

from __future__ import annotations

from pathlib import Path
from types import MappingProxyType
from typing import Mapping

from scripts.delegate_acceptance.evidence.validation import _hex_digest, _task_identity, require
from scripts.delegate_acceptance.tasks.runtime_metadata import validate_provider_runtime_metadata


class ProviderRuntimeBindings:
    """Record and project provider identity facts bound to admitted tasks."""

    def __init__(self) -> None:
        self._by_task_id: dict[str, dict[str, str]] = {}

    def identity_for(self, task_id: str) -> Mapping[str, str] | None:
        identity = self._by_task_id.get(task_id)
        return None if identity is None else MappingProxyType(dict(identity))

    def snapshot(self) -> dict[str, dict[str, str]]:
        return {task_id: dict(identity) for task_id, identity in self._by_task_id.items()}

    def record(
            self, task_id: str, meta: dict[str, object], provider_executable: Path,
            launch_evidence: tuple[dict[str, object], dict[str, object]] | None = None,
            started_evidence: dict[str, object] | None = None) -> dict[str, str]:
        require(_task_identity(task_id), f"task {task_id} has an invalid identity")
        identity = validate_provider_runtime_metadata(
            meta, launch_evidence=launch_evidence, started_evidence=started_evidence,
            label=f"task {task_id} provider runtime metadata")
        if launch_evidence is None and started_evidence is None:
            require("runtime_preflight_sha256" not in meta,
                    f"task {task_id} pending provider runtime has no launch evidence")
            executable = meta.get("provider_executable")
            version = meta.get("provider_version")
            effective = meta.get("effective_config")
            policy = effective.get("policy") if isinstance(effective, dict) else None
            runtime_sha256 = policy.get("runtime_sha256") if isinstance(policy, dict) else None
            require(isinstance(executable, str) and isinstance(version, str) and
                    executable == str(provider_executable) and _hex_digest(runtime_sha256),
                    f"task {task_id} has no admitted provider runtime identity")
            identity = {
                "executable": executable,
                "version": version,
                "sha256": runtime_sha256,
            }
        require(isinstance(identity, dict),
                f"task {task_id} provider runtime identity is unavailable")
        existing = self._by_task_id.get(task_id)
        require(existing is None or existing == identity,
                f"task {task_id} changed its admitted provider runtime identity")
        self._by_task_id[task_id] = identity
        return dict(identity)

    def receipt(self, expected_task_ids: set[str]) -> dict[str, object]:
        identities = {
            task_id: dict(identity)
            for task_id, identity in sorted(self._by_task_id.items())
        }
        require(bool(expected_task_ids) and set(identities) == expected_task_ids,
                "provider runtime identity coverage is incomplete")
        return {
            "provider_versions": {
                task_id: identity["version"] for task_id, identity in identities.items()
            },
            "provider_runtime_identities": identities,
        }
