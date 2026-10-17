"""Private CLI admission boundary."""

from scripts.delegate_acceptance.private_cli.commands import run_json
import time


def observe_created_admission(
    delegate,
    state,
    env,
    response,
    receipt,
    timeout=60,
    phase="admission-observation",
    *,
    capture_root,
    command_prefix=(),
):
    """Confirm one created task after uncertain dispatch; never retry it."""
    failure = response.get("failure", {})
    uncertain = failure.get("code") == "task_submission_uncertain" and failure.get("next_action") == "check_status"
    created = response.get("task_record") == "created"
    admitted = response.get("admission") == "admitted"
    operational_failure = bool(failure or response.get("error"))
    needs_observation = uncertain or (created and admitted and operational_failure)
    if not created or (operational_failure and not needs_observation) or (not admitted and not uncertain):
        raise RuntimeError("upgrade probe was not durably created: %r" % response)
    if not needs_observation:
        return response
    task_id = response["task_id"]
    deadline = time.monotonic() + timeout
    attempt = 0
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("created fixture task admission remained uncertain: " + task_id)
        observed = run_json(
            delegate,
            env,
            [*command_prefix, "--root", str(state), "status", task_id, "--json"],
            timeout=min(45, remaining),
            receipt=receipt.with_name("observation-" + receipt.stem + "-%03d.json" % attempt),
            phase=phase,
            capture_root=capture_root,
        )
        if observed.get("task_id") != task_id:
            raise RuntimeError("admission observation returned a different task")
        # status uses exit zero for a valid uncertain observation, including
        # its diagnostic error text. Nonzero operational failures were rejected
        # by run_json; the admission field decides whether observation is done.
        if observed.get("admission") == "admitted":
            return observed
        if observed.get("admission") != "unknown":
            raise RuntimeError("created fixture task admission could not be confirmed: %r" % observed)
        attempt += 1
        time.sleep(min(0.2, max(0, deadline - time.monotonic())))


def read_created_task_status(
    delegate, state, env, task_id, receipt, phase="task-observation", timeout=45, *, capture_root
):
    """Read one created task without changing or resubmitting its request."""
    observed = run_json(
        delegate,
        env,
        ["--root", str(state), "status", task_id, "--json"],
        timeout=timeout,
        receipt=receipt,
        phase=phase,
        capture_root=capture_root,
        capture_response=True,
    )
    if observed.get("task_id") != task_id:
        raise RuntimeError("task observation returned a different task")
    return observed


def await_created_task_ended(
    delegate, state, env, response, receipt, timeout=30, phase="task-observation", *, capture_root
):
    """Poll one created task until the queue reports ended, without resubmitting it."""
    task_id = response.get("task_id")
    root_id = response.get("root_id")
    if not isinstance(task_id, str) or not isinstance(root_id, str) or not root_id:
        raise RuntimeError("task observation lacks its created task/root identity")
    deadline = time.monotonic() + timeout
    attempt = 0
    poll_receipts = []
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("created task queue status remained non-ended: " + task_id)
        poll_receipt = receipt.with_name("%s-%03d%s" % (receipt.stem, attempt, receipt.suffix))
        status = read_created_task_status(
            delegate,
            state,
            env,
            task_id,
            poll_receipt,
            phase=phase,
            timeout=min(45, remaining),
            capture_root=capture_root,
        )
        poll_receipts.append(poll_receipt.name)
        if status.get("root_id") != root_id:
            raise RuntimeError("task observation returned a different root")
        failure = status.get("failure")
        if status.get("error") or failure:
            raise RuntimeError("task status reported an operational failure: %r" % status)
        liveness = status.get("liveness")
        if liveness == "ended":
            return status, {
                "kind": "queue-ended-observation",
                "final_receipt": poll_receipt.name,
                "poll_receipts": poll_receipts,
                "poll_count": len(poll_receipts),
            }
        if liveness != "running":
            raise RuntimeError("task status did not report running or ended: %r" % status)
        attempt += 1
        time.sleep(min(0.2, max(0, deadline - time.monotonic())))
