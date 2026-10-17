"""Private CLI capacity records workflow."""

import hashlib
import json
from scripts.delegate_acceptance.private_cli.capacity_policy import _validate_capacity_launch_records
from scripts.delegate_acceptance.private_cli.lease_policy import lease_fifo_path
from scripts.delegate_acceptance.lifetime.identity import ordinary_lifetime_owner_digest


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
        launch_paths = sorted(lifetime.glob("*.launch.json"))
        if not launch_paths:
            continue
        if len(launch_paths) > 64 or any(path.is_symlink() or not path.is_file() for path in launch_paths):
            raise RuntimeError("task has an invalid lifetime launch set: " + task_id)
        if termination_path.exists():
            raise RuntimeError("gated task terminated before the capacity proof: " + task_id)
        try:
            task_digest = hashlib.sha256(task_path.read_bytes()).hexdigest()
            meta_digest = hashlib.sha256(meta_path.read_bytes()).hexdigest()
            owner_digest = ordinary_lifetime_owner_digest(task_digest, meta_digest)
            intent = json.loads(intent_path.read_text())
            registration_bytes = registration_path.read_bytes()
            registration = json.loads(registration_bytes)
            worker = json.loads(worker_path.read_text())
            launches = [json.loads(path.read_text()) for path in launch_paths]
        except (OSError, json.JSONDecodeError) as error:
            raise RuntimeError("task lifetime evidence is unreadable: " + task_id) from error
        registration_digest = _validate_capacity_launch_records(
            intent, launches, owner_digest, registration, registration_bytes, root_id, task_id, worker
        )
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
            }
        )
    pids = [specification["pid"] for specification in specifications]
    if len(pids) != len(set(pids)):
        raise RuntimeError("capacity fixture launch identities are not unique")
    return specifications
