"""Private CLI stop policy boundary."""


def default_stop_status_row(status, request_id):
    rows = status.get("stops") or []
    for row in rows:
        if isinstance(row, dict) and row.get("request_id") == request_id:
            return row
    raise RuntimeError("status did not retain the exact stop request: " + request_id)


def default_stop_dispatch_evidence(response):
    failure = response.get("failure") or {}
    return {
        "task_id": response.get("task_id"),
        "root_id": response.get("root_id"),
        "task_record": response.get("task_record"),
        "admission": response.get("admission"),
        "failure": {key: failure.get(key) for key in ("code", "stage", "next_action") if failure.get(key) is not None},
    }


def default_stop_lease_evidence(specification, lease_observation):
    return {
        "pid": specification["pid"],
        "scope_kind": specification["scope_kind"],
        "assurance": specification["assurance"],
        "owner_sha256": specification["owner_sha256"],
        "registration_sha256": specification["registration_sha256"],
        "scope_plan_sha256": specification["scope_plan_sha256"],
        "scope_id_sha256": specification["scope_id_sha256"],
        **lease_observation,
    }


def default_stop_stop_evidence(stop):
    if not isinstance(stop, dict):
        raise RuntimeError("stop response omitted its durable stop facts")
    return {
        key: stop.get(key)
        for key in (
            "request_id",
            "cause",
            "requested",
            "matched",
            "attempted",
            "action",
            "numeric_task_id",
            "acknowledged",
            "in_flight",
            "observed_state",
            "terminated",
        )
        if key in stop
    }


def default_stop_closure_evidence(closure, task_id, cause):
    if not isinstance(closure, dict):
        raise RuntimeError("canonical lifetime closure omitted task evidence: " + task_id)
    termination = closure.get("termination")
    registration = closure.get("registration")
    worker_wait = closure.get("worker_wait")
    launches = closure.get("launches")
    if not isinstance(termination, dict) or termination.get("cause") != cause:
        raise RuntimeError("canonical lifetime closure recorded the wrong termination cause: " + task_id)
    if termination.get("scope_empty") is not True:
        raise RuntimeError("canonical lifetime closure did not prove an empty native scope: " + task_id)
    intent = closure.get("intent")
    if not isinstance(intent, dict) or not isinstance(registration, dict) or not isinstance(worker_wait, dict):
        raise RuntimeError("canonical lifetime closure omitted registration or worker Wait evidence: " + task_id)
    if not isinstance(launches, list) or not launches:
        raise RuntimeError("canonical lifetime closure omitted provider launch evidence: " + task_id)
    return {
        "task_id": task_id,
        "termination": {
            "cause": termination.get("cause"),
            "scope_empty": termination.get("scope_empty"),
            "observed_at": termination.get("observed_at"),
        },
        "lifetime": {
            "deadline": intent.get("deadline"),
            "created_at": intent.get("created_at"),
            "registered_at": registration.get("created_at"),
        },
        "worker_wait": {
            key: worker_wait.get(key)
            for key in ("child_pid", "result", "exit_code", "observed_at")
            if key in worker_wait
        },
        "launch_count": len(launches),
        "scope": {"kind": registration.get("scope_kind"), "assurance": registration.get("assurance")},
    }
