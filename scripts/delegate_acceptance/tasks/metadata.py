"""Pure task metadata rules over records supplied by the admission reader."""

from __future__ import annotations

from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp
from scripts.delegate_acceptance.evidence.validation import _exact_keys, _hex_digest, require
from scripts.delegate_acceptance.paths.policy import historical_clean_absolute_path
from scripts.delegate_acceptance.tasks.runtime_metadata import validate_provider_runtime_metadata



_META_REQUIRED_KEYS = {
    "schema_version", "root_id", "task_id", "spec_sha256", "requested_config",
    "effective_config", "containment", "approval", "provider_executable",
    "provider_version", "publisher_build", "publisher_version", "predicate",
    "supervisor_config", "created_at",
}


_META_OPTIONAL_KEYS = {
    "runner_executable", "runner_ownership", "environment", "environment_recorded",
    "runtime_preflight_sha256", "output_artifacts", "output_writer_contract", "input_files",
}


_EFFECTIVE_CONFIG_KEYS = {"containment", "approval", "digest"}


_PREDICATE_KEYS = {"adapter", "mode", "version", "sha256"}


def _validate_ordinary_metadata(
        meta: dict[str, object],
        root: str,
        spec_digest: str,
        task: str,
        task_record: dict[str, object],
) -> None:
    _exact_keys(meta, _META_REQUIRED_KEYS, _META_OPTIONAL_KEYS,
                label=f"ordinary task metadata for {task}")
    validate_provider_runtime_metadata(
        meta, label=f"ordinary task provider runtime metadata for {task}")
    require(type(meta.get("schema_version")) is int and meta["schema_version"] == 1 and
            meta.get("root_id") == root and meta.get("task_id") == task and
            meta.get("spec_sha256") == spec_digest and
            meta.get("requested_config") == task_record["requested_config"],
            f"ordinary task metadata identity is invalid for {task}")
    effective = meta.get("effective_config")
    require(isinstance(effective, dict),
            f"ordinary task effective configuration is invalid for {task}")
    _exact_keys(effective, _EFFECTIVE_CONFIG_KEYS, {"policy"},
                label=f"ordinary task effective configuration for {task}")
    require(isinstance(effective.get("containment"), str) and effective["containment"] and
            isinstance(effective.get("approval"), str) and effective["approval"] and
            _hex_digest(effective.get("digest")) and
            meta.get("containment") == effective["containment"] and
            meta.get("approval") == effective["approval"],
            f"ordinary task effective configuration changed for {task}")
    if "policy" in effective:
        require(isinstance(effective["policy"], dict),
                f"ordinary task policy is invalid for {task}")
    if "runner_executable" in meta:
        executable = meta["runner_executable"]
        require(historical_clean_absolute_path(executable),
                f"ordinary task runner_executable is invalid for {task}")
    if "runner_ownership" in meta:
        require(meta["runner_ownership"] in {"managed", "custom"},
                f"ordinary task runner ownership is invalid for {task}")
    for key in ("publisher_build", "publisher_version"):
        require(isinstance(meta.get(key), str) and bool(meta[key].strip()),
                f"ordinary task {key} is invalid for {task}")
    if "environment" in meta:
        environment = meta["environment"]
        require(isinstance(environment, list) and
                all(isinstance(item, str) and "=" in item and item.split("=", 1)[0]
                    for item in environment),
                f"ordinary task environment is invalid for {task}")
    for key in ("input_files", "output_artifacts"):
        if key in meta:
            require(isinstance(meta[key], list),
                    f"ordinary task {key} is invalid for {task}")
    if "output_writer_contract" in meta:
        require(isinstance(meta["output_writer_contract"], str),
                f"ordinary task output writer contract is invalid for {task}")
    predicate = meta.get("predicate")
    require(isinstance(predicate, dict), f"ordinary task predicate is invalid for {task}")
    _exact_keys(predicate, _PREDICATE_KEYS, label=f"ordinary task predicate for {task}")
    require(predicate.get("adapter") == task_record["provider"] and
            predicate.get("mode") == task_record["mode"] and
            isinstance(predicate.get("version"), str) and bool(predicate["version"].strip()) and
            _hex_digest(predicate.get("sha256")),
            f"ordinary task predicate binding is invalid for {task}")
    _canonical_timestamp(meta.get("created_at"), f"ordinary task created_at for {task}")
