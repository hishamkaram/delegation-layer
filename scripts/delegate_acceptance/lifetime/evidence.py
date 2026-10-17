"""Read and compose managed lifetime records through canonical policy."""

from __future__ import annotations

from datetime import datetime
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.evidence.records import (
    _read_control_record,
    _read_optional_control_record,
)
from scripts.delegate_acceptance.evidence.timestamps import (
    _canonical_timestamp,
    _timestamp_at_or_after,
    _timestamp_before,
)
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _exact_keys,
    _hex_digest,
    require,
)
from scripts.delegate_acceptance.lifetime.absence_policy import (
    _validate_lifetime_absence_completion,
)
from scripts.delegate_acceptance.lifetime.identity import (
    MAX_LIFETIME_LAUNCHES,
    _lifetime_launch_sequence,
    _lifetime_record_name,
    _lifetime_token,
    ordinary_lifetime_owner_digest,
)
from scripts.delegate_acceptance.lifetime.policy import (
    _validate_lifetime_intent,
    _validate_lifetime_registration,
    _validate_lifetime_termination,
    _validate_lifetime_wait,
)
from pathlib import Path


def _validate_managed_lifetime(
        *, directory: Path, root: str, task: str, owner_kind: str, owner_digest: str,
        created_floor: tuple[datetime, int] | None = None,
        expected_deadline: str | None = None,
        window_deadline: tuple[datetime, int] | None = None) -> dict[str, object]:
    """Validate one owner's complete managed-worker and empty-scope proof."""
    evidence = {"inspection": "inspection", "task": "ordinary task",
                "workspace": "workspace operation"}.get(owner_kind, "lifetime")
    require(owner_kind in {"inspection", "task", "workspace"} and _hex_digest(owner_digest),
            f"{evidence} lifetime owner is invalid for {task}")
    require(directory.is_dir() and not directory.is_symlink(),
            f"{evidence} lifetime directory is invalid for {task}")
    wait = _read_optional_control_record(
        directory / _lifetime_record_name(owner_kind, "worker-wait"),
        f"{evidence} lifetime worker Wait for {task}")
    recovered = wait is not None and wait.get("result") == "absent"

    intent_path = directory / _lifetime_record_name(owner_kind, "intent")
    intent = _read_control_record(intent_path, f"{evidence} lifetime intent for {task}")
    scope_kind, assurance, intent_created = _validate_lifetime_intent(
        created_floor=created_floor, evidence=evidence, expected_deadline=expected_deadline, intent=intent,
        owner_digest=owner_digest, owner_kind=owner_kind, recovered=recovered,
        root=root, task=task, window_deadline=window_deadline,
    )
    registration_path = directory / _lifetime_record_name(owner_kind, "registration")
    registration = _read_optional_control_record(
        registration_path, f"{evidence} lifetime registration for {task}")
    if registration is None:
        return _validate_intent_only_lifetime_absence(
            directory, root, task, owner_kind, intent, intent_path, wait)
    registration_created = _validate_lifetime_registration(
        assurance=assurance, evidence=evidence, intent=intent, intent_created=intent_created, owner_digest=owner_digest,
        owner_kind=owner_kind, recovered=recovered,
        registration=registration, root=root, scope_kind=scope_kind, task=task, window_deadline=window_deadline,
    )
    registration_digest = digest(registration_path)

    worker_path = directory / _lifetime_record_name(owner_kind, "worker")
    worker = _read_optional_control_record(worker_path, f"{evidence} lifetime worker for {task}")
    worker_started, worker_digest = _read_lifetime_worker(
        evidence=evidence, recovered=recovered, registration=registration, registration_created=registration_created,
        registration_digest=registration_digest, root=root, task=task,
        worker=worker, worker_path=worker_path,
    )
    _validate_lifetime_wait(
        assurance=assurance, evidence=evidence, recovered=recovered, registration_digest=registration_digest,
        root=root, task=task, wait=wait, worker=worker,
        worker_digest=worker_digest, worker_started=worker_started,
    )
    launches = _read_lifetime_launches(
        directory=directory, evidence=evidence, owner_kind=owner_kind, recovered=recovered, registration_created=registration_created,
        registration_digest=registration_digest,
        root=root, task=task, window_deadline=window_deadline,
    )
    termination = _read_control_record(
        directory / _lifetime_record_name(owner_kind, "termination"),
        f"{evidence} lifetime termination for {task}")
    _validate_lifetime_termination(
        evidence=evidence, launches=launches, recovered=recovered, registration_created=registration_created,
        registration_digest=registration_digest, root=root, task=task,
        termination=termination, wait=wait,
    )
    if recovered:
        _validate_lifetime_absence_completion(
            wait=wait, root=root, task=task, recorded_boot=str(registration["boot_id"]), registration_digest=registration_digest,
            intent_digest="", worker=worker, worker_digest=worker_digest, termination=termination)
    return {"intent": intent, "registration": registration, "worker": worker,
            "worker_wait": wait, "launches": launches, "termination": termination}


def _validate_intent_only_lifetime_absence(
        directory: Path, root: str, task: str, owner_kind: str,
        intent: dict[str, object], intent_path: Path,
        wait: dict[str, object] | None) -> dict[str, object]:
    require(wait is not None and wait.get("result") == "absent",
            f"unregistered lifetime lacks recovered absence for {task}")
    worker = _read_optional_control_record(
        directory / _lifetime_record_name(owner_kind, "worker"), f"lifetime worker for {task}")
    termination = _read_optional_control_record(
        directory / _lifetime_record_name(
            owner_kind, "termination",
        ), f"lifetime termination for {task}")
    try:
        entries = tuple(directory.iterdir())
    except OSError as error:
        raise AcceptanceFailure(f"cannot inspect unregistered lifetime for {task}") from error
    require(worker is None and termination is None and not any(
        _lifetime_launch_sequence(owner_kind, entry.name) is not None for entry in entries),
        f"unregistered lifetime has contradictory native records for {task}")
    if owner_kind in {"inspection", "workspace"}:
        require(not (directory / "start.json").exists() and not (directory / "start.json").is_symlink(

        ),
                f"unregistered lifetime has a worker start guard for {task}")
    _validate_lifetime_absence_completion(
        wait=wait, root=root, task=task, recorded_boot=str(intent["boot_id"]), registration_digest="",
        intent_digest=digest(intent_path), worker=None, worker_digest="", termination=None)
    return {"intent": intent, "registration": None, "worker": None,
            "worker_wait": wait, "launches": [], "termination": None}


def _validate_managed_inspection_lifetime(directory: Path, root: str, task: str,
                                          request_digest: str,
                                          request_created: tuple[datetime, int],
                                          request_deadline: str,
                                          deadline: tuple[datetime, int]) -> dict[str, object]:
    """Validate the guardian-owned Wait and empty-scope proof for one inspection."""
    return _validate_managed_lifetime(
        directory=directory, root=root, task=task, owner_kind="inspection", owner_digest=request_digest,
        created_floor=request_created, expected_deadline=request_deadline,
        window_deadline=deadline)


def _validate_managed_ordinary_lifetime(directory: Path, root: str, task: str,
                                        spec_digest: str,
                                        meta_digest: str) -> dict[str, object]:
    """Validate taskdir's managed lifetime records for one ordinary task."""
    return _validate_managed_lifetime(
        directory=directory / "lifetime", root=root, task=task, owner_kind="task",
        owner_digest=ordinary_lifetime_owner_digest(spec_digest, meta_digest))






def _read_lifetime_worker(
        *, evidence: str,
        recovered: bool,
        registration: dict[str, object],
        registration_created: tuple[datetime, int],
        registration_digest: str,
        root: str,
        task: str,
        worker: dict[str, object] | None,
        worker_path: Path,
) -> tuple[tuple[datetime, int], str]:
    worker_started = registration_created
    worker_digest = ""
    if worker is not None:
        _exact_keys(worker, {
            "schema_version", "root_id", "task_id", "registration_sha256", "pid",
            "process_start", "boot_id", "started_at",
        }, label=f"{evidence} lifetime worker for {task}")
        worker_pid = worker.get("pid")
        require(type(worker.get("schema_version")) is int and worker["schema_version"] == 1 and
                worker.get("root_id") == root and worker.get("task_id") == task and
                worker.get("registration_sha256") == registration_digest and
                type(worker_pid) is int and worker_pid > 0 and
                _lifetime_token(worker.get("process_start")) and
                worker.get("boot_id") == registration.get("boot_id"),
                f"{evidence} lifetime worker binding mismatch for {task}")
        worker_started = _canonical_timestamp(
            worker.get("started_at"), f"{evidence} lifetime worker started_at for {task}")
        require(recovered or _timestamp_at_or_after(worker_started, registration_created),
                f"{evidence} lifetime worker time is invalid for {task}")
        worker_digest = digest(worker_path)
    return worker_started, worker_digest




def _read_lifetime_launches(
        *, directory: Path,
        evidence: str,
        owner_kind: str,
        recovered: bool,
        registration_created: tuple[datetime, int],
        registration_digest: str,
        root: str,
        task: str,
        window_deadline: tuple[datetime, int] | None,
) -> list[dict[str, object]]:
    launches: list[dict[str, object]] = []
    try:
        sequences = sorted(sequence for entry in directory.iterdir()
                           if (sequence := _lifetime_launch_sequence(
                               owner_kind, entry.name)) is not None)
    except OSError as error:
        raise AcceptanceFailure(
            f"cannot inspect {evidence} lifetime launches for {task}") from error
    require(len(sequences) <= MAX_LIFETIME_LAUNCHES and
            sequences == list(range(1, len(sequences) + 1)),
            f"{evidence} lifetime launch sequence is invalid for {task}")
    for sequence in sequences:
        launch = _read_control_record(
            directory / _lifetime_record_name(owner_kind, "launch", sequence),
            f"{evidence} lifetime launch {sequence} for {task}")
        _exact_keys(launch, {
            "schema_version", "root_id", "task_id", "registration_sha256", "sequence",
            "pid", "process_start", "started_at",
        }, {"pgid"}, label=f"{evidence} lifetime launch {sequence} for {task}")
        pid = launch.get("pid")
        pgid = launch.get("pgid", 0)
        require(type(launch.get("schema_version")) is int and launch["schema_version"] == 1 and
                launch.get("root_id") == root and launch.get("task_id") == task and
                launch.get("registration_sha256") == registration_digest and
                type(launch.get("sequence")) is int and launch["sequence"] == sequence and
                type(pid) is int and pid > 0 and
                type(pgid) is int and pgid >= 0 and _lifetime_token(launch.get("process_start")),
                f"{evidence} lifetime launch binding mismatch for {task}")
        launched_at = _canonical_timestamp(
            launch.get("started_at"), f"{evidence} lifetime launch started_at for {task}")
        require(recovered or (_timestamp_at_or_after(launched_at, registration_created) and
                (window_deadline is None or _timestamp_before(launched_at, window_deadline))),
                f"{evidence} lifetime launch time is invalid for {task}")
        launches.append(launch)
    return launches
