"""Private CLI stop workflow."""

from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_BUDGET
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_TASK_BUDGET
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_STOP_TASK_COUNT
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLeaseCohort
from scripts.delegate_acceptance.private_cli.leases import LockedReleaseGate
from pathlib import Path
from scripts.delegate_acceptance.private_cli.admission import await_created_task_ended
from scripts.delegate_acceptance.private_cli.supervisor import await_managed_lifetime_closure
from scripts.delegate_acceptance.private_cli.capacity_records import capacity_lease_specifications
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_closure_evidence
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_dispatch_evidence
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_lease_evidence
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_status_row
from scripts.delegate_acceptance.private_cli.stop_policy import default_stop_stop_evidence
from scripts.delegate_acceptance.private_cli.fingerprints import file_fingerprint
from scripts.delegate_acceptance.private_cli.capacity_ownership import lease_candidate_specifications
from scripts.delegate_acceptance.private_cli.admission import observe_created_admission
from scripts.delegate_acceptance.private_cli.commands import run_json
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_private_supervisor
import time
import uuid
from scripts.delegate_acceptance.private_cli.supervisor import wait_for_pueue_state


def default_stop_live_lease(client, config, env, state, response, waiter_directory, lease_nonce, *, capture_root):
    """Prove one fixture provider is live before exercising its stop path."""
    task_id = response["task_id"]
    root_id = response["root_id"]
    label = "delegate:" + root_id + ":" + task_id
    wait_for_pueue_state(
        client, config, env, label, "Running", phase="default-stop-observation", capture_root=capture_root
    )
    deadline = time.monotonic() + 30
    while True:
        specifications = lease_candidate_specifications(
            capacity_lease_specifications(state, [response], waiter_directory, lease_nonce)
        )
        if len(specifications) == 1:
            lease_started = time.time_ns()
            cohort = LiveFIFOLeaseCohort.acquire(specifications, lease_nonce)
            cohort.assert_live()
            lease_observed = time.time_ns()
            return (
                specifications[0],
                cohort,
                {"observed_at_unix_ns": lease_observed, "acquire_elapsed_ns": max(0, lease_observed - lease_started)},
            )
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise RuntimeError(
                "fixture provider did not reach a live lease before its bounded observation deadline: " + task_id
            )
        time.sleep(min(0.05, remaining))


def require_empty_fixture_waiters(waiter_directory):
    entries = tuple(Path(waiter_directory).iterdir())
    if entries:
        raise RuntimeError(
            "fixture provider lease entries remained after canonical lifetime closure: " + str(len(entries))
        )


def run_default_stop_acceptance(
    delegate,
    client,
    state,
    workspace,
    brief,
    gate,
    waiter_directory,
    config,
    env,
    response_directory,
    provider_fixture,
    lease_nonce,
    run_receipt,
    attempted_task_ids,
    *,
    capture_root,
):
    """Exercise default user-stop and sub-second budget termination on live fixtures."""
    waiting_brief = brief.with_name("default-stop-waiting-brief.md")
    waiting_brief.write_text("DELEGATE_PRIVATE_UPGRADE_WAIT\n")
    waiting_brief.chmod(0o600)
    receipts = Path(response_directory)
    release_gate = LockedReleaseGate.acquire(gate)
    task_evidence = {}
    acceptance_evidence = initial_stop_evidence(provider_fixture, task_evidence)
    # Keep completed task evidence in the receipt even when fixture cleanup fails later.
    run_receipt.add_details(default_stop=acceptance_evidence)

    dispatch_fixture, close_task = _default_stop_task_operations(
        capture_root, delegate, env, receipts, run_receipt, state, waiting_brief, workspace
    )
    try:
        _prove_default_user_stop(
            attempted_task_ids,
            capture_root,
            client,
            close_task,
            config,
            delegate,
            dispatch_fixture,
            env,
            lease_nonce,
            receipts,
            run_receipt,
            state,
            task_evidence,
            waiter_directory,
        )
        _prove_default_budget_stop(
            attempted_task_ids,
            capture_root,
            client,
            close_task,
            config,
            dispatch_fixture,
            env,
            lease_nonce,
            run_receipt,
            state,
            task_evidence,
            waiter_directory,
        )
        require_empty_fixture_waiters(waiter_directory)
        acceptance_evidence["cleanup"]["fixture_waiter_entries"] = 0
        release_gate.release()
        release_gate = None
        shutdown_private_supervisor(
            client, config, env, state=state, task_ids=tuple(attempted_task_ids), capture_root=capture_root
        )
        if run_receipt is not None:
            run_receipt.update_counts(
                dispatch_completed_count=DEFAULT_STOP_TASK_COUNT, admitted_task_count=DEFAULT_STOP_TASK_COUNT
            )
        acceptance_evidence["cleanup"]["supervisor_shutdown"] = "verified"
        return acceptance_evidence
    finally:
        if release_gate is not None:
            release_gate.release()


def _default_stop_task_operations(capture_root, delegate, env, receipts, run_receipt, state, waiting_brief, workspace):
    def dispatch_fixture(task_id, budget, receipt_name):
        response = run_json(
            delegate,
            env,
            [
                "--root",
                str(state),
                "dispatch",
                "--provider",
                "pi:json",
                "--brief",
                str(waiting_brief),
                "--cwd",
                str(workspace),
                "--id",
                task_id,
                "--permission",
                "read-only",
                "--budget",
                budget,
                "--json",
            ],
            expected=(0, 1),
            timeout=45,
            receipt=receipts / receipt_name,
            phase="default-stop-dispatch",
            capture_root=capture_root,
        )
        if response.get("task_id") != task_id or response.get("task_record") != "created":
            raise RuntimeError("default-stop dispatch did not create the requested task")
        if not response.get("root_id") or response.get("admission") != "admitted":
            raise RuntimeError("default-stop dispatch did not admit the requested task")
        observed = observe_created_admission(
            delegate,
            state,
            env,
            response,
            receipts / receipt_name,
            phase="default-stop-admission-observation",
            capture_root=capture_root,
        )
        if observed.get("task_id") != task_id or observed.get("admission") != "admitted":
            raise RuntimeError("default-stop admission observation did not confirm the same task")
        run_receipt.increment("dispatch_completed_count")
        run_receipt.increment("admitted_task_count")
        return response

    def close_task(response, cause, status_receipt):
        task_id = response["task_id"]
        closure = await_managed_lifetime_closure(state, (task_id,), timeout=30)
        task_closure = closure.get("tasks", {}).get(task_id)
        closure_evidence = default_stop_closure_evidence(task_closure, task_id, cause)
        status, queue_observation = await_created_task_ended(
            delegate,
            state,
            env,
            response,
            receipts / status_receipt,
            phase="default-stop-observation",
            capture_root=capture_root,
        )
        return status, closure_evidence, queue_observation

    return dispatch_fixture, close_task


def _prove_default_user_stop(
    attempted_task_ids,
    capture_root,
    client,
    close_task,
    config,
    delegate,
    dispatch_fixture,
    env,
    lease_nonce,
    receipts,
    run_receipt,
    state,
    task_evidence,
    waiter_directory,
):
    run_receipt.phase("default_stop_user")
    user_task_id = uuid.uuid4().hex
    attempted_task_ids.append(user_task_id)
    user_dispatch = dispatch_fixture(user_task_id, DEFAULT_STOP_TASK_BUDGET, "dispatch-default-stop-user.json")
    user_specification, user_cohort, user_lease = default_stop_live_lease(
        client, config, env, state, user_dispatch, waiter_directory, lease_nonce, capture_root=capture_root
    )
    run_receipt.update_counts(running_task_count=1, live_lease_count=1)
    with user_cohort:
        cancel_started = time.time_ns()
        cancel = run_json(
            delegate,
            env,
            ["--root", str(state), "cancel", user_task_id, "--json"],
            receipt=receipts / "cancel-default-stop-user.json",
            phase="default-stop-user-request",
            capture_root=capture_root,
        )
        cancel_finished = time.time_ns()
        if cancel.get("task_id") != user_task_id or cancel.get("root_id") != user_dispatch.get("root_id"):
            raise RuntimeError("default-stop cancel response changed task identity")
        user_stop = cancel.get("stop")
        if not isinstance(user_stop, dict) or (
            user_stop.get("requested") is not True
            or user_stop.get("cause") != "user"
            or user_stop.get("in_flight") is not True
            or user_stop.get("attempted") is not True
            or not isinstance(user_stop.get("request_id"), str)
        ):
            raise RuntimeError("default-stop cancel did not record a live user stop request")
        user_status, user_closure, user_queue_observation = close_task(
            user_dispatch, "requested", "status-default-stop-user.json"
        )
        user_row = default_stop_status_row(user_status, user_stop["request_id"])
        if user_row.get("cause") != "user" or user_row.get("terminated") is not True:
            raise RuntimeError("default-stop status did not retain the completed user stop")
        task_evidence["user_stop"] = {
            "budget": DEFAULT_STOP_TASK_BUDGET,
            "dispatch": default_stop_dispatch_evidence(user_dispatch),
            "lease": default_stop_lease_evidence(user_specification, user_lease),
            "cancel": {
                "receipt": "cancel-default-stop-user.json",
                "started_at_unix_ns": cancel_started,
                "finished_at_unix_ns": cancel_finished,
                "stop": default_stop_stop_evidence(user_stop),
            },
            "status": {
                "receipt": user_queue_observation["final_receipt"],
                "liveness": user_status.get("liveness"),
                "stop": default_stop_stop_evidence(user_row),
                "queue_ended_observation": user_queue_observation,
            },
            "closure": user_closure,
            "fixture_lease_cleanup": {"state": "pending"},
        }
        task_evidence["user_stop"]["fixture_lease_cleanup"] = user_cohort.remove_closed_fifos()
        run_receipt.update_counts(running_task_count=0, live_lease_count=0)


def _prove_default_budget_stop(
    attempted_task_ids,
    capture_root,
    client,
    close_task,
    config,
    dispatch_fixture,
    env,
    lease_nonce,
    run_receipt,
    state,
    task_evidence,
    waiter_directory,
):
    run_receipt.phase("default_stop_budget")
    budget_task_id = uuid.uuid4().hex
    attempted_task_ids.append(budget_task_id)
    budget_dispatch = dispatch_fixture(budget_task_id, DEFAULT_STOP_BUDGET, "dispatch-default-stop-budget.json")
    budget_specification, budget_cohort, budget_lease = default_stop_live_lease(
        client, config, env, state, budget_dispatch, waiter_directory, lease_nonce, capture_root=capture_root
    )
    run_receipt.update_counts(running_task_count=1, live_lease_count=1)
    with budget_cohort:
        budget_status, budget_closure, budget_queue_observation = close_task(
            budget_dispatch, "budget", "status-default-stop-budget.json"
        )
        budget_row = default_stop_status_row(budget_status, "budget")
        if budget_row.get("cause") != "budget" or budget_row.get("terminated") is not True:
            raise RuntimeError("default-stop status did not retain the budget termination")
        if any(row.get("cause") == "user" for row in (budget_status.get("stops") or [])):
            raise RuntimeError("budget fixture unexpectedly acquired a user stop request")
        task_evidence["budget_stop"] = {
            "budget": DEFAULT_STOP_BUDGET,
            "dispatch": default_stop_dispatch_evidence(budget_dispatch),
            "lease": default_stop_lease_evidence(budget_specification, budget_lease),
            "status": {
                "receipt": budget_queue_observation["final_receipt"],
                "liveness": budget_status.get("liveness"),
                "stop": default_stop_stop_evidence(budget_row),
                "queue_ended_observation": budget_queue_observation,
            },
            "closure": budget_closure,
            "fixture_lease_cleanup": {"state": "pending"},
        }
        task_evidence["budget_stop"]["fixture_lease_cleanup"] = budget_cohort.remove_closed_fifos()
        run_receipt.update_counts(running_task_count=0, live_lease_count=0)


def initial_stop_evidence(provider_fixture, task_evidence):
    acceptance_evidence = {
        "scenario": DEFAULT_STOP_SCENARIO,
        "requested_task_count": DEFAULT_STOP_TASK_COUNT,
        "provider": "pi:json",
        "fixture": "fake-pi-live-fifo",
        "supervisor": "real-pueue-private",
        "stop_poll_policy": {
            "override": False,
            "mode": "production-default",
            "timing_claim": "observed native lifecycle timestamps; no fixed latency guarantee",
        },
        "fingerprints": {"provider_fixture": file_fingerprint(provider_fixture)},
        "tasks": task_evidence,
        "cleanup": {
            "fixture_waiter_entries": "pending",
            "lifetime_closure": "canonical-managed-validator",
            "supervisor_shutdown": "pending",
        },
    }
    return acceptance_evidence
