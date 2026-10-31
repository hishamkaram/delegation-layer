"""Private CLI capacity records workflow."""

import hashlib
import os
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.evidence.records import _read_control_record
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.private_cli.capacity_policy import (
    _validate_capacity_launch,
    _validate_capacity_registration,
)
from scripts.delegate_acceptance.private_cli.lease_policy import lease_fifo_path
from scripts.delegate_acceptance.lifetime.identity import (
    MAX_LIFETIME_LAUNCH_BYTES,
    MAX_LIFETIME_LAUNCHES,
    _lifetime_launch_sequences,
    _lifetime_record_name,
    ordinary_lifetime_owner_digest,
)
from scripts.delegate_acceptance.lifetime.policy import _account_lifetime_launch


def capacity_lease_specifications(state, responses, waiter_directory, lease_nonce):
    """Return exact fixture leases backed by consistent nonterminal launch evidence."""
    specifications = []
    for response in responses:
        task_id = response["task_id"]
        root_id = response["root_id"]
        lifetime = state / "tasks" / task_id / "lifetime"
        task_path = state / "tasks" / task_id / "task.json"
        meta_path = state / "tasks" / task_id / "meta.json"
        intent_path = lifetime / "intent.json"
        registration_path = lifetime / "registration.json"
        worker_path = lifetime / "worker.json"
        owner_lock_path = lifetime / "owner.lock"
        termination_path = lifetime / "terminated.json"
        required = (task_path, meta_path, intent_path, registration_path, worker_path, owner_lock_path)
        if not all(path.exists() for path in required):
            continue
        if any(path.is_symlink() or not path.is_file() for path in required):
            raise RuntimeError("task lifetime control record is unsafe: " + task_id)
        launch_paths = [
            lifetime / _lifetime_record_name("task", "launch", sequence)
            for sequence in _task_launch_sequences(lifetime, task_id)
        ]
        if not launch_paths:
            continue
        if any(path.is_symlink() or not path.is_file() for path in launch_paths):
            raise RuntimeError("task has an invalid lifetime launch set: " + task_id)
        if termination_path.exists():
            raise RuntimeError("gated task terminated before the capacity proof: " + task_id)
        try:
            owner_digest = ordinary_lifetime_owner_digest(digest(task_path), digest(meta_path))
            intent = _read_control_record(intent_path, "task lifetime intent")
            registration = _read_control_record(registration_path, "task lifetime registration")
            registration_digest = digest(registration_path)
            worker = _read_control_record(worker_path, "task lifetime worker")
        except (OSError, AcceptanceFailure) as error:
            raise RuntimeError("task lifetime evidence is unreadable: " + task_id) from error
        _validate_capacity_registration(
            intent=intent, owner_digest=owner_digest, registration=registration, registration_digest=registration_digest,
            root_id=root_id, task_id=task_id, worker=worker
        )
        launches = _read_task_launches(launch_paths, registration_digest, root_id, task_id)
        pid = launches[-1]["pid"]
        specifications.append(
            {
                "root_id": root_id,
                "task_id": task_id,
                "pid": pid,
                "path": lease_fifo_path(waiter_directory, lease_nonce, pid),
                "owner_lock": owner_lock_path,
                "owner_sha256": owner_digest,
                "registration_sha256": registration_digest,
                "scope_kind": registration["scope_kind"],
                "assurance": registration["assurance"],
                "scope_plan_sha256": hashlib.sha256(intent["scope_plan"].encode()).hexdigest(),
                "scope_id_sha256": hashlib.sha256(registration["scope_id"].encode()).hexdigest(),
                "deadline": intent.get("deadline"),
            }
        )
    pids = [specification["pid"] for specification in specifications]
    if len(pids) != len(set(pids)):
        raise RuntimeError("capacity fixture launch identities are not unique")
    return specifications


def _task_launch_sequences(lifetime, task_id):
    """Inventory launches under the canonical count bound while reading the directory."""
    message = "task has an invalid lifetime launch set: " + task_id
    try:
        with os.scandir(lifetime) as entries:
            return _lifetime_launch_sequences(
                "task", (entry.name for entry in entries),
                limit=MAX_LIFETIME_LAUNCHES, label=message,
            )
    except AcceptanceFailure as error:
        raise RuntimeError(message) from error
    except OSError as error:
        raise RuntimeError("task lifetime evidence is unreadable: " + task_id) from error


def _read_task_launches(launch_paths, registration_digest, root_id, task_id):
    """Read each bounded launch, validate it, and enforce the canonical byte budget before keeping it."""
    launches = []
    launch_bytes = 0
    for sequence, path in enumerate(launch_paths, start=1):
        try:
            launch = _read_control_record(path, "task lifetime launch")
        except (OSError, AcceptanceFailure) as error:
            raise RuntimeError("task lifetime evidence is unreadable: " + task_id) from error
        _validate_capacity_launch(
            launch=launch, registration_digest=registration_digest, root_id=root_id, sequence=sequence, task_id=task_id
        )
        try:
            launch_bytes = _account_lifetime_launch(
                launch_bytes, launch, limit=MAX_LIFETIME_LAUNCH_BYTES, label="task lifetime launch"
            )
        except AcceptanceFailure as error:
            raise RuntimeError("task has an invalid lifetime launch set: " + task_id) from error
        launches.append(launch)
    return launches
