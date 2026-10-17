"""Task admission, command observations, and continuation binding."""

from __future__ import annotations

import os
from pathlib import Path
import time
import uuid

import scripts.delegate_acceptance.private_cli.supervisor as private_cli_supervisor
import scripts.delegate_acceptance.private_cli.admission as private_cli_admission
import scripts.delegate_acceptance.private_cli.commands as private_cli_commands
from scripts.delegate_acceptance.tasks.publication import verify_collected_outcome
from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.evidence.files import write_json
from .context import WorkspaceContext
from .evidence import _compact
from .model import (
    ADMISSION_COMMAND_TIMEOUT_SECONDS, FIXTURE_SESSION, TASK_ID_RE, WAIT_SECONDS,
)
from .policy import (
    _validate_hex, classify_control_response,
)



def _control(
    context: WorkspaceContext,
    args: list[str],
    *,
    label: str,
    task_id: str | None = None,
    expected: tuple[int, ...] = (0,),
    timeout: float = 45,
    phase: str | None = None,
) -> tuple[int, dict[str, object]]:
    receipt = context.command_receipt(task_id, label)
    response = private_cli_commands.run_json(
        context.delegate,
        context.environment,
        args,
        expected=expected,
        timeout=timeout,
        receipt=receipt,
        phase=phase,
        capture_root=context.capture_root,
        capture_response=True,
    )
    try:
        evidence = read_json(receipt)
    except (OSError, ValueError) as error:
        raise RuntimeError("shared control helper did not retain command response") from error
    code = evidence.get("exit_code")
    require(type(code) is int, "shared control helper omitted command exit")
    return code, response


def _workspace_from(response: dict[str, object]) -> dict[str, object]:
    workspace = response.get("workspace")
    require(isinstance(workspace, dict), "response omitted additive workspace block")
    return workspace


def _workspace_id(response: dict[str, object]) -> str:
    workspace = _workspace_from(response)
    return _validate_hex(workspace.get("id"), TASK_ID_RE, "workspace ID")


def _operation_id(response: dict[str, object]) -> str | None:
    workspace = response.get("workspace")
    if not isinstance(workspace, dict):
        return None
    operation = workspace.get("operation_id")
    if operation is None:
        operation_result = workspace.get("operation")
        if isinstance(operation_result, dict):
            operation = operation_result.get("operation_id")
    if operation is None:
        return None
    return _validate_hex(operation, TASK_ID_RE, "workspace operation ID")


def _wait_workspace(
    context: WorkspaceContext,
    workspace_id: str,
    *,
    operation_id: str | None = None,
    state: str | None = None,
    timeout: float = WAIT_SECONDS,
    task_id: str | None = None,
) -> dict[str, object]:
    deadline = time.monotonic() + timeout
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("workspace observation expired: " + workspace_id)
        args = [
            "--root", str(context.state), "workspace", "status", workspace_id,
            "--watch", str(min(5.0, remaining)) + "s", "--json",
        ]
        if operation_id is not None:
            args.extend(["--operation", operation_id])
        _code, response = _control(
            context, args, label="workspace-status", task_id=task_id,
            # Observation can finish an accepted scheduling transaction. Its
            # fixed private-control cutoff is independent of --watch; use the
            # same finite transport budget as admission rather than aborting
            # that transaction after the short observation window.
            expected=(0, 3), timeout=ADMISSION_COMMAND_TIMEOUT_SECONDS,
            phase="workspace-observation",
        )
        workspace = _workspace_from(response)
        if workspace.get("state") in {"unreadable", "failed"}:
            raise RuntimeError("workspace entered terminal failure: " + repr(_compact(workspace)))
        if state is None:
            done = workspace.get("path") and workspace.get("state") in {"retained", "busy"}
        else:
            done = workspace.get("state") == state
        if operation_id is not None:
            result = workspace.get("operation")
            if isinstance(result, dict):
                result_status = result.get("status")
                if result_status == "failed":
                    raise RuntimeError("workspace operation failed: " + repr(_compact(result)))
                if result_status == "succeeded" and (state is None or done):
                    return response
        if done and operation_id is None:
            return response
        if classify_control_response(_code, response) == "failed":
            raise RuntimeError("workspace observation failed: " + repr(_compact(response)))


def _adopt_workspace_container(context: WorkspaceContext, root_id: str, workspace_id: str) -> Path:
    _validate_hex(root_id, TASK_ID_RE, "workspace root ID")
    expected_parent = context.state.parent / ("." + context.state.name + ".workspaces")
    if context.container_parent_token is None:
        if not expected_parent.is_dir() or expected_parent.is_symlink():
            raise RuntimeError("managed workspace container parent was not created canonically")
        context.container_parent_token = context.owner.adopt_created(
            "managed workspace container parent", expected_parent, expected_parent.parent,
        )
    expected = expected_parent / root_id
    if context.container_token is None:
        if not expected.is_dir() or expected.is_symlink():
            raise RuntimeError("managed workspace root container was not provisioned canonically")
        context.container_token = context.owner.adopt_created(
            "managed workspace root container", expected, expected_parent,
        )
    else:
        require(context.container_token.directory == expected, "workspace root container changed")
        context.container_token.assert_live()
    request = read_json(context.state / "workspaces" / workspace_id / "request.json")
    root_record = read_json(context.state / "root.json")
    require(root_record.get("root_id") == root_id, "state root identity changed")
    require(request.get("root_id") == root_id, "workspace request root identity changed")
    require(request.get("container") == str(expected), "workspace request container identity changed")
    anchor = expected / workspace_id
    require(anchor.is_dir() and not anchor.is_symlink(), "workspace anchor is not canonical")
    marker = anchor / ".delegate-workspace"
    require(marker.is_symlink() and os.readlink(marker) == str(context.state / "workspaces" / workspace_id),
            "workspace anchor marker is not exact")
    return expected


def _admit_created_task(
    context: WorkspaceContext,
    task_id: str,
    response: dict[str, object],
) -> dict[str, object]:
    """Apply one admission protocol to dispatch and exact continuation."""
    if response.get("task_id") != task_id:
        raise RuntimeError("admission response task identity changed")
    if response.get("task_record") != "created":
        raise RuntimeError("admission did not durably create task: " + repr(_compact(response)))
    context.add_task(task_id)
    _control(
        context, ["--root", str(context.state), "status", task_id, "--json"],
        label="first-status", task_id=task_id, expected=(0, 3),
        phase="first-created-task-observation",
    )
    failure = response.get("failure")
    if isinstance(failure, dict) and failure.get("code") == "task_submission_uncertain":
        receipt = context.command_receipt(task_id, "admission-observation")
        response = private_cli_admission.observe_created_admission(
            context.delegate, context.state, context.environment, response, receipt,
            timeout=WAIT_SECONDS, capture_root=context.capture_root,
            command_prefix=(),
        )
        write_json(receipt.with_name(receipt.stem + "-response.json"), {
            "arguments": ["status", task_id, "--json"],
            "exit_code": 0,
            "response": response,
        })
    if response.get("admission") not in {"admitted", None}:
        raise RuntimeError("task admission failed: " + repr(_compact(response)))
    return response


def _dispatch(
    context: WorkspaceContext,
    brief: Path,
    *,
    task_budget: str,
    fresh: bool,
    workspace_id: str | None = None,
) -> dict[str, object]:
    task_id = uuid.uuid4().hex
    with context.command_lock:
        context.attempted_task_ids.append(task_id)
    args = [
        "--root", str(context.state), "dispatch",
        "--provider", "pi:json", "--brief", str(brief),
        "--id", task_id, "--permission", "workspace-write",
        "--budget", task_budget, "--workspace-budget", context.workspace_budget,
        "--json",
    ]
    if fresh:
        args.extend([
            "--cwd", str(context.source), "--isolation", "git-worktree",
            "--base", context.source_baseline["head"],
        ])
    else:
        require(workspace_id is not None, "workspace reuse dispatch lacks ID")
        args.extend(["--workspace", workspace_id])
    code, response = _control(
        context, args, label="dispatch", task_id=task_id, expected=(0, 1, 2, 3),
        timeout=ADMISSION_COMMAND_TIMEOUT_SECONDS, phase="workspace-dispatch",
    )
    response = _admit_created_task(context, task_id, response)
    workspace_id = _workspace_id(response)
    context.add_workspace(workspace_id)
    root_id = response.get("root_id")
    require(isinstance(root_id, str), "dispatch omitted state root identity")
    return {
        "task_id": task_id,
        "workspace_id": workspace_id,
        "root_id": root_id,
        "dispatch": response,
        "workspace": response,
    }


def _continue(
    context: WorkspaceContext,
    predecessor: dict[str, object],
    *,
    brief: Path,
    task_budget: str,
) -> dict[str, object]:
    predecessor_id = _validate_hex(
        predecessor.get("task_id"), TASK_ID_RE, "continuation predecessor task ID",
    )
    predecessor_root_id = _validate_hex(
        predecessor.get("root_id"), TASK_ID_RE, "continuation predecessor root ID",
    )
    args = [
        "--root", str(context.state), "continue", "--task", predecessor_id,
        "--brief", str(brief), "--budget", task_budget, "--json",
    ]
    _code, response = _control(
        context, args, label="continue", expected=(0, 1, 2, 3),
        timeout=ADMISSION_COMMAND_TIMEOUT_SECONDS, phase="workspace-continuation",
    )
    task_id = _validate_hex(response.get("task_id"), TASK_ID_RE, "returned successor task ID")
    with context.command_lock:
        context.attempted_task_ids.append(task_id)
    response = _admit_created_task(context, task_id, response)
    require(response.get("parent_task_id") == predecessor_id,
            "continuation response omitted exact predecessor")
    workspace_id = _workspace_id(response)
    context.add_workspace(workspace_id)
    root_id = response.get("root_id")
    require(isinstance(root_id, str), "continuation omitted state root identity")
    require(root_id == predecessor_root_id, "continuation changed state root identity")
    return {
        "task_id": task_id,
        "workspace_id": workspace_id,
        "root_id": root_id,
        "parent_task_id": predecessor_id,
        "continue": response,
        "workspace": response,
    }


def _await_task(context: WorkspaceContext, task: dict[str, object], timeout: float) -> dict[str, object]:
    # A completed queue row may already have been reclaimed. Wait for the
    # validated provider publication; workspace capture and the canonical
    # root-level lifetime validator separately prove native closure below.
    return _collect_task(context, task, timeout)


def _collect_task(context: WorkspaceContext, task: dict[str, object], timeout: float) -> dict[str, object]:
    task_id = task["task_id"]
    _code, response = _control(
        context,
        ["--root", str(context.state), "collect", task_id, "--watch", str(timeout) + "s", "--json"],
        label="collect", task_id=task_id, expected=(0, 4),
        timeout=timeout + 10, phase="task-collection",
    )
    outcome = response.get("outcome")
    require(isinstance(outcome, dict), "collection omitted provider outcome")
    task_directory = context.state / "tasks" / task_id
    stored = read_json(task_directory / "outcome.json")
    require(stored == outcome, "collection is not bound to sole outcome authority")
    verify_collected_outcome(response, task_directory, stored["verdict"])
    return response


def _prove_lifetimes_closed(context: WorkspaceContext, task_ids: list[str]) -> dict[str, object]:
    proof = private_cli_supervisor.await_managed_lifetime_closure(
        context.state, tuple(task_ids), timeout=WAIT_SECONDS,
    )
    require(isinstance(proof, dict), "managed lifetime closure proof is not an object")
    return proof


def _wait_capture(context: WorkspaceContext, task: dict[str, object]) -> tuple[dict[str, object], dict[str, object]]:
    workspace_id = task["workspace_id"]
    deadline = time.monotonic() + WAIT_SECONDS
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("workspace capture observation expired: " + workspace_id)
        response = _wait_workspace(
            context, workspace_id, timeout=min(15, remaining), task_id=task["task_id"],
        )
        workspace = _workspace_from(response)
        capture = workspace.get("capture")
        if isinstance(capture, dict):
            if capture.get("status") == "captured":
                return response, capture
            if capture.get("status") == "unavailable":
                raise RuntimeError("workspace capture became unavailable")
        time.sleep(0.1)


def _provider_session(context: WorkspaceContext, task_id: str) -> dict[str, object]:
    reference_path = context.state / "tasks" / task_id / "provider.ref.json"
    require(reference_path.is_file() and not reference_path.is_symlink(),
            "successful fixture task did not publish provider session reference")
    reference = read_json(reference_path)
    root_record = read_json(context.state / "root.json")
    require(reference.get("root_id") == root_record.get("root_id") and
            reference.get("task_id") == task_id and
            reference.get("provider") == "pi:json",
            "provider session reference has wrong task identity")
    require(reference.get("conversation_id") == FIXTURE_SESSION,
            "provider session reference has wrong fixture session")
    return {
        "provider": reference.get("provider"),
        "conversation_id": reference.get("conversation_id"),
    }


def _fixture_session_marker(
    context: WorkspaceContext,
    *,
    resumed: bool,
) -> dict[str, object]:
    marker_path = context.fixture_session_marker
    require(marker_path.is_file() and not marker_path.is_symlink(),
            "native fixture session marker is absent")
    marker = read_json(marker_path)
    expected = {
        "schema_version": 1,
        "session": FIXTURE_SESSION,
        "resumed": resumed,
    }
    require(marker == expected, "native fixture session marker is not exact")
    return marker


def _task_record(context: WorkspaceContext, task_id: str) -> dict[str, object]:
    record_path = context.state / "tasks" / task_id / "task.json"
    require(record_path.is_file() and not record_path.is_symlink(),
            "task request record is absent: " + task_id)
    record = read_json(record_path)
    require(record.get("task_id") == task_id, "task request identity changed: " + task_id)
    return record


def _immutable_task_snapshot(
    context: WorkspaceContext,
    task_id: str,
) -> dict[str, bytes | None]:
    directory = context.state / "tasks" / task_id
    names = (
        "task.json",
        "meta.json",
        "brief.md",
        "provider.started.json",
        "provider.exit",
        "provider.ref.json",
        "outcome.json",
    )
    snapshot: dict[str, bytes | None] = {}
    for name in names:
        path = directory / name
        if name in {"provider.exit", "outcome.json"} and not path.exists() and not path.is_symlink():
            snapshot[name] = None
            continue
        require(path.is_file() and not path.is_symlink(),
                "immutable predecessor record is absent: " + name)
        snapshot[name] = path.read_bytes()
    return snapshot


def _assert_immutable_task_snapshot(
    context: WorkspaceContext,
    task_id: str,
    snapshot: dict[str, bytes | None],
) -> None:
    directory = context.state / "tasks" / task_id
    for name, expected in snapshot.items():
        path = directory / name
        if expected is None:
            require(not path.exists() and not path.is_symlink(),
                    "continuation created an absent predecessor record: " + name)
            continue
        require(path.is_file() and not path.is_symlink(),
                "immutable predecessor record disappeared: " + name)
        require(path.read_bytes() == expected,
                "predecessor immutable record changed: " + name)


def _validate_continuation_binding(
    context: WorkspaceContext,
    predecessor: dict[str, object],
    successor: dict[str, object],
    session: str,
) -> dict[str, object]:
    predecessor_id = _validate_hex(
        predecessor.get("task_id"), TASK_ID_RE, "continuation predecessor task ID",
    )
    successor_id = _validate_hex(
        successor.get("task_id"), TASK_ID_RE, "continuation successor task ID",
    )
    expected_workspace_id = _validate_hex(
        predecessor.get("workspace_id"), TASK_ID_RE, "continuation workspace ID",
    )
    require(isinstance(session, str) and session == FIXTURE_SESSION,
            "continuation session identity is not the fixture session")
    predecessor_record = _task_record(context, predecessor_id)
    successor_record = _task_record(context, successor_id)
    root_id = _validate_hex(
        predecessor.get("root_id"), TASK_ID_RE, "continuation root ID",
    )
    require(predecessor_record.get("root_id") == root_id,
            "predecessor task root identity changed")
    require(successor_record.get("root_id") == root_id and
            successor.get("root_id") == root_id,
            "successor task root identity changed")
    require(predecessor_record.get("provider") == "pi:json" and
            successor_record.get("provider") == "pi:json",
            "continuation changed provider identity")
    predecessor_cwd = predecessor_record.get("canonical_cwd")
    successor_cwd = successor_record.get("canonical_cwd")
    require(isinstance(predecessor_cwd, str) and predecessor_cwd and
            successor_cwd == predecessor_cwd,
            "continuation changed canonical cwd")
    predecessor_workspace = predecessor_record.get("workspace")
    successor_workspace = successor_record.get("workspace")
    require(isinstance(predecessor_workspace, dict) and
            isinstance(successor_workspace, dict),
            "continuation omitted managed workspace binding")
    require(predecessor_workspace.get("id") == expected_workspace_id and
            successor_workspace.get("id") == expected_workspace_id and
            successor.get("workspace_id") == expected_workspace_id,
            "continuation changed managed workspace identity")
    prior = successor_record.get("prior_session")
    require(prior == {
        "provider": "pi:json",
        "conversation_id": session,
        "predecessor_task_id": predecessor_id,
    }, "successor prior_session binding is not exact")
    predecessor_reference = _provider_session(context, predecessor_id)
    successor_reference = _provider_session(context, successor_id)
    require(predecessor_reference["conversation_id"] == session and
            successor_reference["conversation_id"] == session,
            "continuation changed provider session identity")
    workspace_response = successor.get("workspace")
    workspace = (
        _workspace_from(workspace_response)
        if isinstance(workspace_response, dict)
        else {}
    )
    workspace_path = workspace.get("path")
    require(workspace_path == predecessor_cwd,
            "continuation workspace path does not match task cwd")
    return {
        "predecessor_task_id": predecessor_id,
        "successor_task_id": successor_id,
        "predecessor_session": predecessor_reference,
        "successor_session": successor_reference,
        "prior_session": prior,
        "canonical_cwd": predecessor_cwd,
        "workspace_id": expected_workspace_id,
    }
