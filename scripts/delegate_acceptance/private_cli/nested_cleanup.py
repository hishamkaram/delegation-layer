"""Child-before-parent cleanup through existing durable cancellation owners."""

from pathlib import Path
import time

from scripts.delegate_acceptance.evidence.records import _read_control_record
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.private_cli.nested_observation import (
    lifetime_checkpoint, await_actor_closure, reconcile_prepared_actor,
)
from scripts.delegate_acceptance.evidence.files import path_present, write_json
from scripts.delegate_acceptance.private_cli.supervisor import (
    await_observed_disposal_readiness, shutdown_custody_roots,
)
from scripts.delegate_acceptance.private_cli.disposal_observer import public_status_observer


def fence_creator(case, deadline):
    fence = case.controls.directory / "fence"
    if not path_present(fence):
        write_json(fence, {"nonce": case.configuration["nonce"], "requested_at_unix_ns": time.time_ns()})
    request = _read_control_record(fence, "leader fence request")
    require(request.get("nonce") == case.configuration["nonce"]
            and type(request.get("requested_at_unix_ns")) is int and request["requested_at_unix_ns"] > 0,
            "leader fence request changed identity")
    requested_ns = request["requested_at_unix_ns"]
    leader = case.responses.get("leader")
    if leader is None:
        if not path_present(case.a / "tasks" / case.configuration["leader"]):
            return
        leader = reconcile_prepared_actor(case, case.a, case.configuration["leader"])
        case.responses["leader"] = leader
    directory = case.a / "tasks" / leader["task_id"]
    child_intents = (case.controls.directory / (role + "-intent.json") for role in ("w1", "w2"))
    if not path_present(directory / "provider.start") and not any(path_present(path) for path in child_intents):
        # Its direct dispatch invocation has ended. The installed fence also
        # prevents a late provider from initiating any nested control command.
        cancel_actor(case, "leader", case.a, deadline)
        return
    while time.monotonic() < deadline:
        termination = directory / "lifetime" / "terminated.json"
        if termination.exists():
            lifetime_checkpoint(case.a, leader, deadline)
            return
        if (case.controls.directory / "fenced.json").is_file():
            acknowledgement = _read_control_record(case.controls.directory / "fenced.json", "leader fence")
            launch = case.match_launch(case.a, leader, acknowledgement)
            require(acknowledgement.get("pid") == launch["pid"]
                    and acknowledgement.get("nonce") == case.configuration["nonce"]
                    and type(acknowledgement.get("observed_at_unix_ns")) is int
                    and acknowledgement["observed_at_unix_ns"] >= requested_ns, "leader fence was not acknowledged")
            return
        time.sleep(0.02)
    raise TimeoutError("leader creator could not be fenced before child recovery")


def reconcile_children(case):
    for role in ("w1", "w2"):
        task_id = case.configuration[role]
        directory = case.b / "tasks" / task_id
        if directory.exists():
            task = _read_control_record(directory / "task.json", "created nested task")
            require(task.get("task_id") == task_id, "nested task reconciliation changed identity")
            case.responses.setdefault(role, {"task_id": task_id, "root_id": task["root_id"]})
            if task_id not in case.b_ids:
                case.b_ids.append(task_id)
    if "w2" in case.responses:
        first = case.b / "tasks" / case.configuration["w1"] / "provider.start"
        require(first.is_file() and not first.is_symlink(), "W2 admission lacks W1's consumed launch fence")


def cancel_actor(case, role, state, deadline):
    response = case.responses.get(role)
    if response is None:
        return
    directory = state / "tasks" / response["task_id"]
    if (directory / "outcome.json").exists():
        outcome = _read_control_record(directory / "outcome.json", "terminal task outcome")
        require(outcome.get("task_id") == response["task_id"] and outcome.get("root_id") == response["root_id"]
                and outcome.get("verdict") in {"committed", "rejected"}, "terminal outcome binding mismatch")
        return
    if role == "leader" and (directory / "lifetime" / "terminated.json").exists():
        lifetime_checkpoint(state, response, deadline)
        return
    result = case.command(state, ["cancel", response["task_id"]], "cleanup-cancel-" + role,
                          expected=(0, 1), deadline=deadline)
    stop = result.get("stop")
    require(isinstance(stop, dict) and stop.get("requested") is True and stop.get("cause") == "user"
            and isinstance(stop.get("request_id"), str), "cleanup cancellation did not report its permanent stop")
    record = _read_control_record(directory / "stop" / (stop["request_id"] + ".request.json"), "permanent user stop")
    require(record.get("task_id") == response["task_id"] and record.get("root_id") == response["root_id"]
            and record.get("cause") == "user", "cleanup permanent stop changed task identity")


def cleanup_case(case, deadline):
    if not case.lifetime.attempted_task_ids and not case.b_ids:
        forget_uninitialized_roots(case)
        case.evidence["cleanup"]["status"] = "no-task-or-supervisor-created"
        return
    fence_creator(case, deadline)
    reconcile_children(case)
    # StopTask commits permanent refusal before opening/recovering B. Queued
    # work is removed while paused; no cleanup resume is assumed necessary.
    for role in ("w2", "w1"):
        cancel_actor(case, role, case.b, deadline)
        case.release(role)
    if case.b == case.a:
        for role in ("w2", "w1"):
            if role in case.responses:
                await_actor_closure(case, case.b, case.responses[role], deadline)
    if case.b.exists() and case.b != case.a:
        cancel_owned_setup(case, case.b, case.b_ids, deadline)
        close_root(case, case.b, case.b_ids, deadline)
    elif not case.b.exists() and case.b != case.a:
        # No creator can still initialize this exact root after the fence.
        case.lifetime.managed_state_roots.remove((case.b, case.b_ids))
        case.lifetime.supervisor_cleanups[:] = [entry for entry in case.lifetime.supervisor_cleanups
                                               if entry.get("state") != case.b]
    case.release("descendant")
    case.release("leader")
    cancel_actor(case, "leader", case.a, deadline)
    cancel_owned_setup(case, case.a, case.lifetime.attempted_task_ids, deadline)
    close_root(case, case.a, case.lifetime.attempted_task_ids, deadline)
    descendant = case.controls.directory / "descendant.json"
    if descendant.exists():
        original = _read_control_record(descendant, "ordinary descendant")
        observed = case.native(original["pid"], expected_start=original["process_start"])
        require(not observed["present"] or observed["identity_matches"] is False,
                "ordinary descendant remained live after cleanup release")
    case.evidence["cleanup"]["status"] = "canonical-closure-proven"


def forget_uninitialized_roots(case):
    """Before any Delegate admission, only empty driver-owned state paths exist."""
    for state in {case.a, case.b}:
        if path_present(state):
            require(state.is_dir() and not state.is_symlink() and not any(state.iterdir()),
                    "uninitialized root contains unknown state; retain for diagnosis")
        case.lifetime.managed_state_roots[:] = [entry for entry in case.lifetime.managed_state_roots if entry[0] != state]
        case.lifetime.supervisor_cleanups[:] = [entry for entry in case.lifetime.supervisor_cleanups
                                               if entry.get("state") != state]


def cancel_owned_setup(case, state, task_ids, deadline):
    actors = {case.configuration[role] for role in ("leader", "w1", "w2")}
    for task_id in task_ids:
        if task_id in actors:
            continue
        task = _read_control_record(state / "tasks" / task_id / "task.json", "owned setup task")
        role = "setup-" + task_id
        case.responses[role] = {"task_id": task_id, "root_id": task["root_id"]}
        cancel_actor(case, role, state, deadline)


def close_root(case, state, task_ids, deadline):
    observer = public_status_observer(Path(case.configuration["delegate"]), state, case.lifetime.env,
                                      capture_root=case.lifetime.capture_token)
    closure = await_observed_disposal_readiness(state, tuple(task_ids), shutdown_custody_roots(state, case.lifetime.env),
                                               deadline, observer, reader=case.read_closure)
    for task_id in closure["tasks"]:
        await_actor_closure(case, state, {"root_id": closure["root_id"], "task_id": task_id}, deadline)
