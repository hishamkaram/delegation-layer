"""Composed native acceptance behavior: dispatch."""

from __future__ import annotations


import secrets
import time
from pathlib import Path
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.evidence.records import parse_json_output
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.native.binding_policy import dispatch_binding
from scripts.delegate_acceptance.native.diagnostics import _delegate_status_summary
from scripts.delegate_acceptance.native.errors import BlockedFailure
from scripts.delegate_acceptance.native.acceptance_diagnostics import authentication_unavailable
from scripts.delegate_acceptance.native.provider_policy import authentication_blocked
from scripts.delegate_acceptance.native.constants import TASK_BUDGET, WATCH_SECONDS


def task_command_prefix(owner) -> list[object]:
    acceptance = owner
    argv: list[object] = [
        acceptance.delegate, "--root", acceptance.state, "--pueue-config", acceptance.config,
    ]
    if getattr(acceptance, "scenario", None) != "timeout-resume":
        argv.extend(["--runner", acceptance.runner])
    return argv

def dispatch_argv(owner, task_id: str, brief: Path,
                  predecessor: str | None = None,
                  budget: str = TASK_BUDGET) -> list[object]:
    acceptance = owner
    argv = acceptance.task_command_prefix()
    argv.extend(["dispatch", "--provider", acceptance.provider,
                 "--brief", brief, "--cwd", acceptance.workspace, "--id", task_id,
                 "--permission", acceptance.mode, "--budget", budget, "--json"])
    model = getattr(acceptance.args, "model", None)
    effort = getattr(acceptance.args, "effort", None)
    if model:
        argv.extend(["--model", model])
    if effort:
        argv.extend(["--effort", effort])
    if getattr(acceptance, "scenario", None) == "interactive":
        argv.append("--interactive")
    if acceptance.provider == "codex:review":
        argv.extend(["--review-target", "uncommitted"])
    if predecessor:
        argv.extend(["--resume-task", predecessor])
    return argv

def dispatch(owner, name: str, brief: Path, predecessor: str | None = None,
             budget: str = TASK_BUDGET) -> str:
    acceptance = owner
    task_id = secrets.token_hex(16)
    argv = acceptance.dispatch_argv(task_id, brief, predecessor, budget)
    acceptance.ops.root_id = acceptance.root_id
    try:
        response = acceptance.ops.dispatch(name, task_id, argv)
        root_id, numeric_id = dispatch_binding(response, task_id, name)
        if acceptance.root_id is None:
            acceptance.root_id = root_id
        require(root_id == acceptance.root_id, "dispatch changed root identity")
        acceptance.ops.root_id = acceptance.root_id
        acceptance.numeric_ids[task_id] = numeric_id
        acceptance.tasks[name] = task_id
        return task_id
    except AcceptanceFailure:
        acceptance._sync_ops_owned_tasks()
        observation = acceptance.ops.dispatch_observations[-1] if acceptance.ops.dispatch_observations else {}
        refused = observation.get("response") if isinstance(observation, dict) else None
        message = str(refused.get("error", "")) if isinstance(refused, dict) else ""
        if authentication_blocked(refused, message):
            raise BlockedFailure(f"{acceptance.provider} authentication is unavailable")
        raise

def continue_task(owner, name: str, brief: Path, predecessor: str) -> str:
    acceptance = owner
    return acceptance.dispatch(name, brief, predecessor=predecessor)

def continue_timed_out_task(owner, name: str, brief: Path, predecessor: str) -> str:
    acceptance = owner
    argv = acceptance.task_command_prefix()
    argv.extend(["continue", "--task", predecessor,
                 "--brief", brief, "--budget", TASK_BUDGET, "--json"])
    acceptance.ops.root_id = acceptance.root_id
    try:
        task_id, response = acceptance.ops.continue_task(name, argv)
        root_id, numeric_id = dispatch_binding(response, task_id, name)
        require(response.get("command") == "continue" and
                response.get("parent_task_id") == predecessor,
                "continuation response did not retain its predecessor")
        require(acceptance.root_id is not None and root_id == acceptance.root_id,
                "continuation changed root identity")
        acceptance.numeric_ids[task_id] = numeric_id
        acceptance.tasks[name] = task_id
        return task_id
    except AcceptanceFailure:
        acceptance._sync_ops_owned_tasks()
        observation = acceptance.ops.dispatch_observations[-1] if acceptance.ops.dispatch_observations else {}
        refused = observation.get("response") if isinstance(observation, dict) else None
        message = str(refused.get("error", "")) if isinstance(refused, dict) else ""
        if authentication_blocked(refused, message):
            raise BlockedFailure(f"{acceptance.provider} authentication is unavailable")
        raise

def wait_done(owner, name: str, task_id: str,
              expected_result: str | frozenset[str] | None = None) -> dict[str, object]:
    acceptance = owner
    require(acceptance.root_id is not None, "root identity is unavailable")
    acceptance.ops.root_id = acceptance.root_id
    try:
        return acceptance.ops.wait_task(name, task_id, expected_result)
    except (AcceptanceFailure, RuntimeError) as error:
        directory = acceptance.state / "tasks" / task_id
        status_detail = "status=unavailable"
        try:
            status = acceptance.ops.status(f"{name}-runner-failure-status", task_id,
                                     expected={0, 1})
            status_detail = _delegate_status_summary(status)
        except (AcceptanceFailure, RuntimeError, OSError, ValueError) as status_error:
            status_detail = f"status=unavailable ({type(status_error).__name__})"
        if authentication_unavailable(directory):
            raise BlockedFailure(f"{acceptance.provider} authentication is unavailable") from error
        raise AcceptanceFailure(
            f"{name} runner completion failed: {error}; Delegate {status_detail}") from error

def wait_timed_out(owner, task_id: str, timeout: float = WATCH_SECONDS,
                   sleep_fn=time.sleep, monotonic_fn=time.monotonic) -> dict[str, object]:
    acceptance = owner
    require(acceptance.root_id is not None, "timeout source has no root identity")
    require(timeout > 0, "timeout observation deadline must be positive")
    acceptance.ops.root_id = acceptance.root_id
    deadline = monotonic_fn() + timeout
    attempt = 0
    while True:
        remaining = deadline - monotonic_fn()
        if remaining <= 0:
            break
        response = acceptance.ops.status(f"timeout-status-{attempt}", task_id,
                                   expected={0, 1}, timeout=min(45, remaining))
        attempt += 1
        if monotonic_fn() >= deadline:
            break
        if response.get("status") != "timed_out":
            delay = min(0.25, max(0.0, deadline - monotonic_fn()))
            if delay <= 0:
                break
            sleep_fn(delay)
            continue
        continuation = response.get("continuation")
        require(isinstance(continuation, dict) and continuation.get("resumable") is True and
                continuation.get("mode") == "native" and
                continuation.get("predecessor_task_id") == task_id and
                isinstance(continuation.get("continue_command"), str) and
                "continue --task " + task_id in continuation["continue_command"],
                "timed-out task is not resumable through Delegate")
        return response
    raise AcceptanceFailure("timed-out task never published resumable continuation evidence")

def _interaction_events(owner, name: str, task_id: str, after: int,
                        watch_seconds: int) -> dict[str, object]:
    acceptance = owner
    argv: list[object] = [
        acceptance.delegate, "--root", acceptance.state, "--pueue-config", acceptance.config,
        "--runner", acceptance.runner, "events", task_id, "--after", str(after),
        "--watch", f"{watch_seconds}s", "--json",
    ]
    process = acceptance.direct(name, argv, expected={0, 3},
                          timeout=watch_seconds + 20)
    response = parse_json_output(process, name)
    require(response.get("task_id") == task_id,
            f"{name} observed the wrong interactive task")
    state = response.get("interaction")
    require(isinstance(state, dict), f"{name} returned no interaction state")
    events = response.get("events", [])
    require(isinstance(events, list) and all(isinstance(event, dict) for event in events),
            f"{name} returned malformed interaction events")
    cursor = response.get("cursor")
    require(type(cursor) is int and cursor >= after,
            f"{name} returned an invalid interaction cursor")
    write_json(acceptance.output / (name + ".json"), response)
    return response

def wait_for_interaction(owner, task_id: str, after: int = 0,
                         pending: bool = False) -> tuple[dict[str, object], int, list[dict[str, object]]]:
    acceptance = owner
    deadline = time.monotonic() + WATCH_SECONDS
    observed: list[dict[str, object]] = []
    cursor = after
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise AcceptanceFailure("interactive event observation expired")
        observation_name = acceptance._interaction_observer.next_name()
        response = acceptance._interaction_events(
            observation_name, task_id, cursor, max(1, min(5, int(remaining))))
        state = response["interaction"]
        events = response.get("events", [])
        require(isinstance(state, dict) and isinstance(events, list),
                "interactive event response changed shape")
        observed.extend(event for event in events if isinstance(event, dict))
        next_cursor = response.get("cursor")
        require(type(next_cursor) is int and next_cursor >= cursor,
                "interactive event cursor moved backwards")
        cursor = next_cursor
        requests = state.get("pending_requests")
        require(isinstance(requests, list), "interactive pending request state is malformed")
        if pending and requests:
            return state, cursor, observed
        if not pending and state.get("closed") is True:
            return state, cursor, observed

def reply_interaction(owner, task_id: str, request_id: str, turn_id: str) -> tuple[str, dict[str, object]]:
    acceptance = owner
    message_id = secrets.token_hex(16)
    answers = acceptance.briefs / "interactive-answer.json"
    write_json(answers, {"1": ["Blue"]})
    argv: list[object] = [
        acceptance.delegate, "--root", acceptance.state, "--pueue-config", acceptance.config,
        "--runner", acceptance.runner, "reply", task_id, "--request", request_id,
        "--answers", answers, "--id", message_id, "--json",
    ]
    response = parse_json_output(
        acceptance.direct("interactive-reply", argv, expected=0, timeout=45),
        "interactive-reply")
    require(response.get("task_id") == task_id,
            "interactive reply targeted the wrong task")
    delivery = response.get("delivery")
    require(isinstance(delivery, dict) and delivery.get("message_id") == message_id and
            delivery.get("state") == "queued",
            "interactive reply was not durably queued")
    state = response.get("interaction")
    require(isinstance(state, dict) and state.get("turn_id") == turn_id,
            "interactive reply changed the interaction turn")
    write_json(acceptance.output / "interactive-reply.json", response)
    return message_id, delivery
