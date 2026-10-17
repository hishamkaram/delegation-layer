"""Pure consistency rules for supplied capacity lifetime records."""

import hashlib


def _validate_capacity_launch_records(
    intent, launches, owner_digest, registration, registration_bytes, root_id, task_id, worker
):
    records = (intent, registration, worker, *launches)
    if any(
        record.get("schema_version") != 1 or record.get("root_id") != root_id or record.get("task_id") != task_id
        for record in records
    ):
        raise RuntimeError("task lifetime evidence changed identity: " + task_id)
    registration_digest = hashlib.sha256(registration_bytes).hexdigest()
    if (
        intent.get("owner_kind") != "task"
        or registration.get("owner_kind") != "task"
        or intent.get("owner_sha256") != owner_digest
        or registration.get("owner_sha256") != owner_digest
        or intent.get("scope_kind") != registration.get("scope_kind")
        or intent.get("assurance") != registration.get("assurance")
        or registration.get("scope_kind") not in {"cgroup-v2", "process-group"}
        or registration.get("assurance") not in {"exact", "cooperative"}
        or (registration.get("scope_kind") == "cgroup-v2" and registration.get("assurance") != "exact")
        or (registration.get("scope_kind") == "process-group" and registration.get("assurance") != "cooperative")
        or not isinstance(intent.get("scope_plan"), str)
        or not intent["scope_plan"]
        or not isinstance(registration.get("scope_id"), str)
        or not registration["scope_id"]
        or intent.get("boot_id") != registration.get("boot_id")
        or worker.get("boot_id") != registration.get("boot_id")
        or worker.get("registration_sha256") != registration_digest
        or not isinstance(worker.get("pid"), int)
        or worker["pid"] <= 0
        or not worker.get("process_start")
    ):
        raise RuntimeError("task lifetime evidence is inconsistent: " + task_id)
    for sequence, launch in enumerate(launches, start=1):
        if (
            launch.get("registration_sha256") != registration_digest
            or launch.get("sequence") != sequence
            or not isinstance(launch.get("pid"), int)
            or launch["pid"] <= 0
            or not launch.get("process_start")
        ):
            raise RuntimeError("task lifetime launch evidence is inconsistent: " + task_id)
    return registration_digest
