"""Private CLI stop policy boundary."""

from scripts.acceptance.evidence.timestamps import _canonical_timestamp
from scripts.acceptance.evidence.timestamps import _timestamp_before
from scripts.acceptance.evidence.timestamps import _timestamp_unix_ns
from scripts.acceptance.evidence.validation import _task_identity
from scripts.acceptance.private_cli.receipt import resource_admission_evidence


def _exact_stop_row(status, request_id):
    rows = status.get("stops") or []
    for row in rows:
        if isinstance(row, dict) and row.get("request_id") == request_id:
            return row
    return None


def default_stop_status_row(status, request_id):
    row = _exact_stop_row(status, request_id)
    if row is None:
        raise RuntimeError("status did not retain the exact stop request: " + request_id)
    return row


def is_documented_user_interruption(status, task_id, root_id, request_id):
    """Recognize a status observation as the documented interruption of this exact user stop.

    The documented user-stop projection is `interrupted`/`task_interrupted` with
    `stop_and_report` guidance and no native resume offer. Canonical closure
    can project it while the queue is still `running`, before the exact stop is
    terminated; afterwards the queue is `ended` and the stop is terminated. No
    other liveness/termination pair is recognized. `timed_out`/`task_cancelled`
    or any budget stop row means a budget stop also fired, which invalidates a
    user-only control. Recognition only lets queue observation proceed; it never
    implies the queue ended, completion, or provider success.
    """
    rows = status.get("stops")
    if not isinstance(rows, list) or not all(isinstance(row, dict) for row in rows):
        return False
    exact_rows = [row for row in rows if row.get("request_id") == request_id]
    if len(exact_rows) != 1 or any(row.get("cause") == "budget" for row in rows):
        return False
    row = exact_rows[0]
    liveness = status.get("liveness")
    continuation = status.get("continuation")
    failure = status.get("failure")
    pending = status.get("pending")
    return (
        status.get("task_id") == task_id
        and status.get("root_id") == root_id
        and (
            (liveness == "running" and row.get("terminated") is False)
            or (liveness == "ended" and row.get("terminated") is True)
        )
        and status.get("status") == "interrupted"
        and status.get("publication") == "unknown"
        and "outcome" not in status
        and "error" not in status
        and (pending is None or (isinstance(pending, dict) and pending.get("kind") == "supervisor-command"))
        and row.get("cause") == "user"
        and row.get("requested") is True
        and isinstance(continuation, dict)
        and continuation.get("mode") == "native"
        and continuation.get("predecessor_task_id") == task_id
        and continuation.get("resumable") is False
        and "continue_command" not in continuation
        and isinstance(failure, dict)
        and failure.get("code") == "task_interrupted"
        and failure.get("stage") == "status"
        and failure.get("next_action") == "stop_and_report"
    )


def _public_dispatch_code(value: object) -> str | None:
    """Keep bounded public identifiers, never arbitrary diagnostic text or objects."""
    if (not isinstance(value, str) or not 0 < len(value) <= 128 or
            value[0] not in "abcdefghijklmnopqrstuvwxyz" or
            any(character not in "abcdefghijklmnopqrstuvwxyz0123456789_-" for character in value)):
        return None
    return value


def default_stop_dispatch_evidence(response: object, *, expected_task_id: str | None = None) -> dict[str, object]:
    """Project only public dispatch facts; malformed values never enter the receipt."""
    data = response if isinstance(response, dict) else {}
    task_id, root_id = data.get("task_id"), data.get("root_id")
    record, admission = data.get("task_record"), data.get("admission")
    failure: dict[str, object] = {}
    raw_failure = data.get("failure")
    if isinstance(raw_failure, dict):
        for key in ("code", "stage", "next_action", "reason_code"):
            value = _public_dispatch_code(raw_failure.get(key))
            if value is not None:
                failure[key] = value
        retry = raw_failure.get("retry_after_seconds")
        # Public retry advice is a positive Go int on the supported 64-bit hosts.
        if type(retry) is int and 0 < retry < 2**63:
            failure["retry_after_seconds"] = retry
    evidence: dict[str, object] = {
        "task_id": task_id if _task_identity(task_id) else None,
        "root_id": root_id if _task_identity(root_id) else None,
        "task_record": record if isinstance(record, str) and record in {"created", "not_created", "unknown"} else None,
        "admission": admission if isinstance(admission, str) and admission in {"admitted", "not-admitted", "unknown"} else None,
        "failure": failure,
    }
    resource = resource_admission_evidence(data)
    if resource is not None:
        evidence["resource_admission"] = resource
    if expected_task_id is not None:
        evidence["expected_task_id_match"] = (
            _task_identity(expected_task_id) and _task_identity(task_id) and task_id == expected_task_id)
    return evidence


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


BUDGET_CLOSURE_ALLOWANCE_SECONDS = 30


def budget_deadline_ns(deadline_text, lease_observed_ns, task_id):
    """Return the recorded budget deadline in Unix ns after proving the lease was live strictly before it."""
    deadline_ns = _timestamp_unix_ns(_canonical_timestamp(deadline_text, "budget lifetime deadline"))
    if type(lease_observed_ns) is not int or lease_observed_ns >= deadline_ns:
        raise RuntimeError("fixture lease was not observed live strictly before its budget deadline: " + task_id)
    return deadline_ns


def budget_closure_watch_seconds(deadline_ns, now_ns):
    """Closure watch: the time left until the recorded deadline plus the existing closure allowance."""
    return max(0, deadline_ns - now_ns) / 1_000_000_000 + BUDGET_CLOSURE_ALLOWANCE_SECONDS


def budget_closure_evidence(closure, task_id, deadline_text):
    """Budget closure evidence that also proves termination happened at or after the recorded deadline."""
    evidence = default_stop_closure_evidence(closure, task_id, "budget")
    if closure["intent"].get("deadline") != deadline_text:
        raise RuntimeError("canonical lifetime closure deadline differs from the observed budget deadline: " + task_id)
    deadline = _canonical_timestamp(deadline_text, "budget lifetime deadline")
    terminated = _canonical_timestamp(closure["termination"].get("observed_at"), "budget termination observed_at")
    if _timestamp_before(terminated, deadline):
        raise RuntimeError("budget termination was recorded before its deadline: " + task_id)
    evidence["budget_deadline"] = {"deadline": deadline_text, "termination_observed_at": evidence["termination"]["observed_at"]}
    return evidence


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
