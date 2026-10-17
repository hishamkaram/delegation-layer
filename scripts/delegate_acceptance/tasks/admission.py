"""Read durable ordinary admission without creating another task ledger."""

from __future__ import annotations

from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.evidence.limits import MAX_PROVIDER_BYTES
from scripts.delegate_acceptance.evidence.records import _read_control_record
from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp
from scripts.delegate_acceptance.evidence.validation import _exact_keys, require
from scripts.delegate_acceptance.lifetime.closure import _ordinary_lifetime_control_started
from scripts.delegate_acceptance.lifetime.evidence import _validate_managed_ordinary_lifetime
from scripts.delegate_acceptance.supervisor.binding import (
    _SUPERVISOR_BINDING_KEYS,
    _SUPERVISOR_BINDING_OPTIONAL_KEYS,
    _validate_supervisor_binding,
)
from scripts.delegate_acceptance.tasks.metadata import _validate_ordinary_metadata
from scripts.delegate_acceptance.tasks.specification import _validate_task_record
from scripts.delegate_acceptance.workspaces.admission_evidence import (
    _validate_historical_workspace_attempt,
)
from pathlib import Path
from typing import Callable


def durable_ordinary_admission(state: Path, known_root: Callable[[], str], managed_runner: bool, task: str, journal: dict[str, object] | None):
    """Read one immutable ordinary submission when its caller response was lost."""
    directory = state / "tasks" / task
    present = directory.exists() or directory.is_symlink()
    if not present:
        return None
    require(directory.is_dir() and not directory.is_symlink(),
            f"ordinary task directory is invalid for {task}")
    root = known_root()

    task_path = directory / "task.json"
    task_record = _read_control_record(task_path, f"ordinary task record for {task}")
    spec_digest = _validate_task_record(
        task_record, root, task, f"ordinary task record for {task}")
    require(spec_digest == digest(task_path),
            f"ordinary task record is not canonical for {task}")
    brief_path = directory / "brief.md"
    require(brief_path.is_file() and not brief_path.is_symlink(),
            f"ordinary task brief is not a regular file for {task}")
    require(brief_path.stat().st_size == task_record["brief_length"] and
            brief_path.stat().st_size <= MAX_PROVIDER_BYTES and
            digest(brief_path) == task_record["brief_sha256"],
            f"ordinary task brief binding is invalid for {task}")

    journal_binding = None
    if journal is not None:
        require(journal.get("ordinary_task_present") is True and
                journal.get("task_id") == task and journal.get("root_id") == root,
                f"inspection journal does not bind ordinary task {task}")
        request = journal["request"]
        if request.get("task") == task_record and request.get("task_sha256") == spec_digest:
            journal_binding = request["binding"]["supervisor"]
        else:
            _validate_historical_workspace_attempt(
                state, journal.get("attempt"), task_record, root,
                f"inspection task history for {task}")
            require(journal.get("historical_aborted") is True,
                    f"inspection journal task binding changed for {task}")

    meta_path = directory / "meta.json"
    meta = _read_control_record(meta_path, f"ordinary task metadata for {task}")
    _validate_ordinary_metadata(meta, root, spec_digest, task, task_record)
    binding = meta.get("supervisor_config")
    _validate_supervisor_binding(binding, f"ordinary task supervisor for {task}")
    require(journal_binding is None or binding == journal_binding,
            f"ordinary task metadata supervisor changed for {task}")

    expected_label, numeric, meta_digest = _read_ordinary_supervisor_receipt(
        binding, directory, meta_path, root, spec_digest, task,
    )
    lifetime_started = _ordinary_lifetime_control_started(directory)
    managed_lifetime = None
    if meta.get("runner_ownership") == "managed" or lifetime_started:
        managed_lifetime = _validate_managed_ordinary_lifetime(
            directory, root, task, spec_digest, meta_digest)
    require(not managed_runner or meta.get("runner_ownership") == "managed",
            f"ordinary task managed runner marker is missing for {task}")
    return {"task_id": task, "label": expected_label, "numeric_task_id": numeric,
            "expected_result": "Success", "managed_lifetime": managed_lifetime}




def _read_ordinary_supervisor_receipt(
        binding: dict[str, object],
        directory: Path,
        meta_path: Path,
        root: str,
        spec_digest: str,
        task: str,
) -> tuple[str, int, str]:
    expected_label = f"delegate:{root}:{task}"
    submit = _read_control_record(directory / "submit.json", f"ordinary submit record for {task}")
    _exact_keys(submit, {"schema_version", "root_id", "task_id", "spec_sha256",
                         "meta_sha256", "label", "supervisor", "created_at"},
                label=f"ordinary submit record for {task}")
    meta_digest = digest(meta_path)
    require(type(submit.get("schema_version")) is int and submit["schema_version"] == 1 and
            submit.get("root_id") == root and submit.get("task_id") == task and
            submit.get("spec_sha256") == spec_digest and
            submit.get("meta_sha256") == meta_digest and
            submit.get("label") == expected_label and submit.get("supervisor") == binding,
            f"ordinary submit record is not bound to task {task}")
    _canonical_timestamp(submit.get("created_at"), f"ordinary submit created_at for {task}")

    supervisor = _read_control_record(
        directory / "supervisor.ref.json", f"ordinary supervisor receipt for {task}")
    _exact_keys(supervisor, {
        "schema_version", "root_id", "task_id", "spec_sha256", "meta_sha256",
        "numeric_task_id", "label", *_SUPERVISOR_BINDING_KEYS,
    }, _SUPERVISOR_BINDING_OPTIONAL_KEYS,
                label=f"ordinary supervisor receipt for {task}")
    numeric = supervisor.get("numeric_task_id")
    require(type(supervisor.get("schema_version")) is int and
            supervisor["schema_version"] == 1 and
            supervisor.get("root_id") == root and supervisor.get("task_id") == task and
            supervisor.get("spec_sha256") == spec_digest and
            supervisor.get("meta_sha256") == meta_digest and
            supervisor.get("label") == expected_label and
            type(numeric) is int and numeric >= 0,
            f"ordinary supervisor receipt identity is invalid for {task}")
    receipt_binding = {
        key: supervisor[key]
        for key in _SUPERVISOR_BINDING_KEYS | _SUPERVISOR_BINDING_OPTIONAL_KEYS
        if key in supervisor
    }
    _validate_supervisor_binding(
        receipt_binding, f"ordinary supervisor receipt binding for {task}")
    require(receipt_binding == binding,
            f"ordinary supervisor receipt binding changed for {task}")
    return expected_label, numeric, meta_digest
