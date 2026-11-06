"""Proof for native acceptance evidence."""

from __future__ import annotations

from pathlib import Path
import re
from .contract import (
    _lifecycle_summary,
    EvidenceError,
    MAX_RECEIPT_BYTES,
    MatrixRow,
    SAFE_TEXT,
    _digest_bytes,
    _expected_task_aliases,
    _safe_proof_text,
    _source_requested_options,
    _task_identifier_values,
)
from .identities import (
    _compact_installed_paths,
    _compact_runtime_identity,
    _compact_provider_runtime_identities,
    _runtime_identity_matches,
)
from .production_acp import compact_production_acp, requires_production_acp
from .filesystem import (
    _bounded_children,
    _read_json,
    _write_atomic,
)


def _cleanup_observation(case_directory: Path, status: str) -> tuple[bool, dict[str, object], str | None]:
    cleanup_path = case_directory / "cleanup.json"
    if cleanup_path.exists() or cleanup_path.is_symlink():
        try:
            value, data = _read_json(cleanup_path, MAX_RECEIPT_BYTES)
        except EvidenceError:
            return False, {"status": "unreadable"}, "cleanup_receipt_invalid"
        if not isinstance(value, dict):
            return False, {"status": "malformed"}, "cleanup_receipt_invalid"
        cleanup_status = value.get("status")
        shutdown = value.get("supervisor_shutdown_observed")
        valid_status = cleanup_status in {"removed", "complete"}
        valid_shutdown = shutdown is True
        if not valid_status or not valid_shutdown:
            return False, {
                "status": "unverified",
                "source_status": cleanup_status if isinstance(cleanup_status, str) else None,
                "supervisor_shutdown_observed": shutdown if isinstance(shutdown, bool) else None,
            }, "cleanup_receipt_incomplete"
        raw_captures = value.get("raw_process_captures")
        if status in {"PASS", "BLOCKED"} and raw_captures not in (None, "removed"):
            return False, {"status": "unverified", "raw_process_captures": "retained"}, "raw_case_evidence_retained"
        return True, {
            "status": "verified",
            "source_status": cleanup_status,
            "supervisor_shutdown_observed": shutdown if isinstance(shutdown, bool) else None,
            "raw_process_captures": raw_captures if isinstance(raw_captures, str) else None,
            "cleanup_receipt_sha256": _digest_bytes(data),
        }, None

    if status == "BLOCKED":
        try:
            child_names = sorted(item.name for item in _bounded_children(case_directory))
        except EvidenceError:
            return False, {"status": "unverified"}, "case_directory_unreadable"
        if "processes" in child_names:
            return False, {"status": "unverified", "unexpected": ["processes"]}, "raw_case_evidence_retained"
        proof = {
            "status": "complete", "supervisor_shutdown_observed": True,
            "raw_process_captures": "removed", "method": "blocked-before-runtime-cleanup",
        }
        try:
            proof_bytes = _write_atomic(cleanup_path, proof)
        except EvidenceError:
            return False, {"status": "unverified"}, "cleanup_receipt_publish_failed"
        return True, {
            "status": "verified", "method": "blocked-before-runtime-cleanup",
            "cleanup_receipt_sha256": _digest_bytes(proof_bytes),
        }, None
    return False, {"status": "missing"}, "cleanup_receipt_missing"


def _read_optional_object(path: Path) -> tuple[dict[str, object] | None, str | None]:
    if not path.exists() and not path.is_symlink():
        return None, None
    try:
        value, data = _read_json(path, MAX_RECEIPT_BYTES)
    except EvidenceError as error:
        return None, error.args[0] if error.args else "proof_file_invalid"
    if not isinstance(value, dict):
        return None, "proof_file_malformed"
    return value, _digest_bytes(data)


def _task_session_bindings(source: dict[str, object]) -> list[dict[str, str]]:
    tasks = source.get("tasks")
    sessions = source.get("provider_sessions")
    if not isinstance(tasks, dict) or not isinstance(sessions, dict) or set(tasks) != set(sessions):
        return []
    result: list[dict[str, str]] = []
    for alias in sorted(tasks):
        task_id = tasks.get(alias)
        session_id = sessions.get(alias)
        if (not isinstance(alias, str) or SAFE_TEXT.fullmatch(alias) is None
                or not isinstance(task_id, str) or _safe_proof_text(task_id) is None
                or not isinstance(session_id, str) or _safe_proof_text(session_id) is None):
            return []
        result.append({"alias": alias, "task_id": task_id, "session_id": session_id})
    return result


def _compact_provider_versions(raw: dict[str, object]) -> dict[str, str] | None:
    """Qualify optional diagnostics keyed by task ID or historical task alias."""
    value = raw.get("provider_versions")
    if not isinstance(value, dict):
        return None
    identities = _compact_provider_runtime_identities(raw)
    if identities is None:
        return None
    tasks = raw.get("tasks")
    result: dict[str, str] = {}
    for key, version in value.items():
        if (not isinstance(key, str) or SAFE_TEXT.fullmatch(key) is None
                or not isinstance(version, str)):
            return None
        task_id = key if key in identities else tasks.get(key) if isinstance(tasks, dict) else None
        if (not isinstance(task_id, str) or task_id not in identities
                or version != identities[task_id]["version"]):
            return None
        result[key] = version
    return dict(sorted(result.items()))


def _binary_identities(
        row: MatrixRow,
        case_directory: Path,
        source: dict[str, object] | None) -> dict[str, object]:
    binding, binding_digest = _read_optional_object(case_directory / "binding.json")
    paths: dict[str, str] = {}
    versions: dict[str, str] = {}
    binding_matches = (binding is not None
                       and binding.get("schema_version") == 2
                       and binding.get("representation") == "installed-paths"
                       and binding.get("provider") == row.definition.provider
                       and binding.get("mode") == row.definition.permission
                       and binding.get("scenario") == row.definition.scenario)
    provider_binding: dict[str, str] | None = None
    if binding_matches:
        for key, value in binding.items():
            if key.endswith("_version") and isinstance(value, str):
                safe = _safe_proof_text(value)
                if safe is not None:
                    versions[key] = safe
        provider_task_id = binding.get("provider_task_id")
        provider_identity = _compact_runtime_identity({
            key: binding["provider_" + key] for key in ("executable", "version")
            if "provider_" + key in binding})
        if (binding.get("provider_identity_scope") == "first-admitted-task"
                and isinstance(provider_task_id, str)
                and re.fullmatch(r"[0-9a-f]{32}", provider_task_id) is not None
                and provider_identity is not None):
            provider_binding = {"task_id": provider_task_id, **{key: value for key, value in provider_identity.items() if key != "sha256"}}
    provider_runtimes = _compact_provider_runtime_identities(source) if source is not None else None
    if provider_runtimes is not None:
        provider_runtimes = {task: {key: value for key, value in identity.items() if key != "sha256"}
                             for task, identity in provider_runtimes.items()}
    if source is not None:
        provider_versions = _compact_provider_versions(source)
        if provider_versions is not None:
            versions.update({f"provider:{key}": value for key, value in provider_versions.items()})
    source_tasks = source.get("tasks") if isinstance(source, dict) else None
    source_task_ids = (
        sorted(source_tasks.values())
        if isinstance(source_tasks, dict) and source_tasks
        and all(isinstance(value, str) and re.fullmatch(r"[0-9a-f]{32}", value)
                for value in source_tasks.values())
        else []
    )
    runtime_coverage = (provider_runtimes is not None
                        and sorted(provider_runtimes) == source_task_ids
                        and len(source_task_ids) == len(set(source_task_ids)))
    provider_binding_matches = (
        provider_binding is not None and provider_runtimes is not None
        and _runtime_identity_matches(provider_runtimes.get(provider_binding["task_id"], {}),
                                      provider_binding)
    )
    selected = binding.get("installed_executables") if binding_matches else None
    paths = _compact_installed_paths(selected, complete=True) or {}
    version_diagnostics_valid = (source is not None and (
        "provider_versions" not in source or provider_versions is not None))
    state = ("complete" if binding_matches and bool(paths)
             and runtime_coverage and provider_binding_matches
             and version_diagnostics_valid else "unavailable")
    result: dict[str, object] = {
        "state": state,
        "schema_version": 2,
        "representation": "installed-paths",
        "paths": dict(sorted(paths.items())),
        "task_ids": source_task_ids,
        "provider_binding": provider_binding,
        "provider_runtime_identities": provider_runtimes or {},
    }
    if versions:
        result["versions"] = dict(sorted(versions.items()))
    if binding_digest is not None:
        result["binding_receipt_sha256"] = binding_digest
    return result


def _source_proof(
        row: MatrixRow,
        source: dict[str, object] | None,
        cleanup: dict[str, object],
        case_directory: Path,
        cleanup_ok: bool,
        source_digest: str | None) -> dict[str, object]:
    source = source or {}
    diagnostics, diagnostics_digest = _read_optional_object(case_directory / "failure-diagnostics.json")
    source_gate = _safe_proof_text(source.get("gate"))
    source_provider = _safe_proof_text(source.get("provider"))
    source_mode = _safe_proof_text(source.get("mode")) or row.definition.permission
    source_scenario = _safe_proof_text(source.get("scenario")) or row.definition.scenario
    binding, binding_marker = _read_optional_object(case_directory / "binding.json")
    binding_error = binding_marker if binding is None else None
    requested_options = _source_requested_options(
        source, binding, require_binding=row.status == "PASS", binding_error=binding_error,
    )
    task_sessions = _task_session_bindings(source)
    task_ids = sorted({binding["task_id"] for binding in task_sessions})
    diagnostic_task_ids: list[str] = []
    if diagnostics is not None:
        diagnostic_task_ids = _task_identifier_values(
            diagnostics.get("task_ids", diagnostics.get("tasks", {})),
        )
    session_ids = sorted({binding["session_id"] for binding in task_sessions})
    sessions_by_alias = {
        binding["alias"]: binding["session_id"] for binding in task_sessions
    }
    binary = _binary_identities(row, case_directory, source)
    expected_aliases = set(_expected_task_aliases(
        row.definition.provider, row.definition.scenario,
    ))
    observed_aliases = {binding["alias"] for binding in task_sessions}
    session_continuity = (
        bool(task_sessions)
        and len(task_ids) == len(task_sessions)
        and len(session_ids) == 1
    )
    lifecycle = _lifecycle_summary(source, cleanup, cleanup_ok, diagnostics_digest)
    production_acp = (
        compact_production_acp(source, task_sessions, tuple(sorted(expected_aliases)))
        if requires_production_acp(row.definition.provider) else None)
    if row.status == "BLOCKED":
        proof_state = "blocked-before-runtime"
    elif row.status == "FAIL":
        proof_state = "unresolved"
    elif (source_gate == "acceptance-native" and source_provider == row.definition.provider
          and source_mode == row.definition.permission and source_scenario == row.definition.scenario
          and observed_aliases == expected_aliases and session_continuity
          and binary.get("state") == "complete"
          and lifecycle["state"] == "complete"
          and requested_options["requested_options_state"] == "recorded"
          and (production_acp is None or production_acp["state"] == "complete")):
        proof_state = "complete"
    else:
        proof_state = "unresolved"
    return {
        "state": proof_state,
        "source_receipt_sha256": source_digest,
        "final_status": row.status,
        "source_status": source.get("status") if isinstance(source.get("status"), str) else None,
        "gate": source_gate or "acceptance-native",
        "provider": source_provider or row.definition.provider,
        "permission": row.definition.permission,
        "mode": source_mode,
        "scenario": source_scenario,
        **requested_options,
        "task_ids": task_ids,
        "task_session_bindings": task_sessions,
        "diagnostic_task_ids": diagnostic_task_ids,
        "session_ids": {
            "state": "recorded" if task_sessions else "not-recorded",
            "values": session_ids,
            "by_alias": sessions_by_alias,
        },
        "binary_identities": binary,
        "lifecycle_proof": lifecycle,
        **({} if production_acp is None else {"production_acp": production_acp}),
    }
