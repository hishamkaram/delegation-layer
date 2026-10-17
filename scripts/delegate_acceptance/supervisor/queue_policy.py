"""Pure Pueue row, terminal-result, and queue identity rules."""

from __future__ import annotations

import shlex
from copy import deepcopy
from datetime import datetime
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _required_keys,
    require,
)
from pathlib import Path


PUEUE_STATE_VARIANTS = frozenset({"Queued", "Running", "Paused", "Stashed", "Done", "Locked"})


_PUEUE_STATUS_KEYS = {"tasks", "groups"}


_PUEUE_GROUP_KEYS = {"status", "parallel_tasks"}


_PUEUE_ROW_KEYS = {
    "id", "created_at", "original_command", "command", "path", "envs", "group",
    "dependencies", "priority", "label", "status",
}


def row_state(row: dict[str, object]) -> str:
    value = row.get("status")
    require(isinstance(value, dict), "pueue row status is not a one-state object")
    require(set(value) <= PUEUE_STATE_VARIANTS,
            "pueue row status contains an unknown lifecycle variant")
    recognized = [name for name in value if name in PUEUE_STATE_VARIANTS]
    require(len(recognized) == 1, "pueue row status is not a one-state object")
    return recognized[0]


def done_result(row: dict[str, object]) -> str | None:
    """Return a recognized pueue terminal result, if the row is Done."""
    status = row.get("status")
    if not isinstance(status, dict):
        return None
    if not set(status) <= PUEUE_STATE_VARIANTS:
        return None
    recognized = [name for name in status if name in PUEUE_STATE_VARIANTS]
    if recognized != ["Done"]:
        return None
    done = status.get("Done")
    if not isinstance(done, dict):
        return None
    result = done.get("result")
    if isinstance(result, str) and result in {"Success", "Killed", "Errored", "DependencyFailed"}:
        return result
    if not isinstance(result, dict) or len(result) != 1:
        return None
    name, value = next(iter(result.items()))
    if name == "Failed" and isinstance(value, int) and not isinstance(value, bool):
        return name
    if name == "FailedToSpawn" and isinstance(value, str):
        return name
    return None


def _validate_pueue_timestamp(value: object, label: str) -> None:
    require(isinstance(value, str) and value, f"pueue {label} is invalid")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise AcceptanceFailure(f"pueue {label} is invalid") from error
    require(parsed.tzinfo is not None, f"pueue {label} has no timezone")


def _validate_pueue_state(value: object, label: str, depth: int = 0) -> None:
    require(isinstance(value, dict), f"pueue {label} state is invalid")
    require(depth <= 32, f"pueue {label} state nesting is excessive")
    require(set(value) <= PUEUE_STATE_VARIANTS,
            f"pueue {label} state contains an unknown lifecycle variant")
    recognized = [name for name in value if name in PUEUE_STATE_VARIANTS]
    require(len(recognized) == 1, f"pueue {label} state is invalid")
    name = recognized[0]
    state = value[name]
    if name == "Queued":
        _required_keys(state, {"enqueued_at"}, label=f"pueue {label} queued state")
        _validate_pueue_timestamp(state["enqueued_at"], f"{label} enqueued_at")
    elif name in {"Running", "Paused"}:
        _required_keys(state, {"enqueued_at", "start"}, label=f"pueue {label} running state")
        _validate_pueue_timestamp(state["enqueued_at"], f"{label} enqueued_at")
        _validate_pueue_timestamp(state["start"], f"{label} start")
    elif name == "Stashed":
        _required_keys(state, {"enqueue_at"}, label=f"pueue {label} stashed state")
        if state["enqueue_at"] is not None:
            _validate_pueue_timestamp(state["enqueue_at"], f"{label} enqueue_at")
    elif name == "Done":
        _required_keys(state, {"enqueued_at", "start", "end", "result"},
                       label=f"pueue {label} done state")
        for key in ("enqueued_at", "start", "end"):
            _validate_pueue_timestamp(state[key], f"{label} {key}")
        result = state["result"]
        if isinstance(result, str):
            require(result in {"Success", "Killed", "Errored", "DependencyFailed"},
                    f"pueue {label} result is unknown")
        else:
            require(isinstance(result, dict) and len(result) == 1,
                    f"pueue {label} result is invalid")
            variant, detail = next(iter(result.items()))
            if variant == "Failed":
                require(type(detail) is int, f"pueue {label} failure result is invalid")
            elif variant == "FailedToSpawn":
                require(isinstance(detail, str), f"pueue {label} spawn result is invalid")
            else:
                raise AcceptanceFailure(f"pueue {label} result is unknown")
    elif name == "Locked":
        _required_keys(state, {"previous_status"}, label=f"pueue {label} locked state")
        _validate_pueue_state(state["previous_status"], label, depth + 1)


def _validate_pueue_status(value: dict[str, object]) -> None:
    _required_keys(value, _PUEUE_STATUS_KEYS, label="pueue status")
    tasks = value["tasks"]
    groups = value["groups"]
    require(isinstance(tasks, dict), "pueue status JSON has no tasks object")
    require(isinstance(groups, dict), "pueue status JSON has no groups object")
    for name, group in groups.items():
        require(isinstance(name, str), "pueue group name is invalid")
        require(isinstance(group, dict), f"pueue group {name} is not an object")
        _required_keys(group, _PUEUE_GROUP_KEYS, label=f"pueue group {name}")
        require(group["status"] in {"Running", "Paused", "Reset"},
                f"pueue group {name} status is invalid")
        require(type(group["parallel_tasks"]) is int and group["parallel_tasks"] >= 0,
                f"pueue group {name} parallelism is invalid")
    for key, row in tasks.items():
        require(isinstance(key, str) and key.isdecimal() and str(int(key)) == key,
                "pueue task map key is not canonical")
        require(isinstance(row, dict), f"pueue task {key} is not an object")
        _required_keys(row, _PUEUE_ROW_KEYS, label=f"pueue task {key}")
        require(type(row["id"]) is int and row["id"] >= 0 and row["id"] == int(key),
                f"pueue task {key} numeric identity is invalid")
        _validate_pueue_timestamp(row["created_at"], f"task {key} created_at")
        for field in ("original_command", "command", "path", "group"):
            require(isinstance(row[field], str), f"pueue task {key} {field} is invalid")
        envs = row["envs"]
        require(isinstance(envs, dict) and all(isinstance(k, str) and isinstance(v, str)
                                                for k, v in envs.items()),
                f"pueue task {key} envs is invalid")
        dependencies = row["dependencies"]
        require(isinstance(dependencies, list) and all(type(item) is int and item >= 0
                                                       for item in dependencies),
                f"pueue task {key} dependencies are invalid")
        require(type(row["priority"]) is int, f"pueue task {key} priority is invalid")
        require(row["label"] is None or isinstance(row["label"], str),
                f"pueue task {key} label is invalid")
        _validate_pueue_state(row["status"], f"task {key}")


def validate_queue_row(row: dict[str, object], label: str, runner: Path,
                       state: Path, task: str, numeric: int | None = None,
                       group: str | None = None,
                       supervised: bool = False) -> None:
    """Bind a pueue status row to one exact runner/state/task invocation."""
    require(row.get("label") == label, f"pueue label mismatch for {task}")
    for key in ("original_command", "command", "path"):
        require(isinstance(row.get(key), str), f"pueue row missing {key} for {task}")
    if numeric is not None:
        require(type(row.get("id")) is int and row["id"] == numeric,
                f"pueue numeric identity mismatch for {task}")
    if group is not None:
        require(row.get("group") == group, f"pueue group mismatch for {task}")
    require(type(supervised) is bool, f"runner supervision mode is invalid for {task}")
    expected = [str(runner)]
    if supervised:
        expected.extend(["--guardian", "--supervised"])
    expected.extend(["--root", str(state), task])
    commands = []
    for key in ("original_command", "command"):
        try:
            commands.append(shlex.split(row[key]))
        except ValueError as error:
            raise AcceptanceFailure(f"pueue row command is invalid for {task}") from error
    require(commands[0] == expected and commands[1] == expected,
            f"pueue row does not bind exact runner/state/task argv for {task}")


def canonical_queue(value: dict[str, object]) -> dict[str, object]:
    """Keep the complete private queue snapshot, including groups."""
    tasks = value.get("tasks")
    groups = value.get("groups")
    require(isinstance(tasks, dict) and isinstance(groups, dict), "queue snapshot is malformed")
    return {"tasks": deepcopy(tasks), "groups": deepcopy(groups)}


def failure_queue_finished(status: dict[str, object], dispatch_attempts: set[str],
                           tasks: dict[str, str], labels: dict[str, str],
                           runner: Path, state: Path,
                           numbers: dict[str, int]) -> bool:
    """Return true only for a complete, positively observed private queue."""
    rows = status.get("tasks")
    groups = status.get("groups")
    if not isinstance(rows, dict) or not isinstance(groups, dict):
        return False
    if set(groups) - {"default"}:
        return False
    if "default" in groups:
        try:
            _required_keys(groups["default"], _PUEUE_GROUP_KEYS, label="private queue default group")
            require(groups["default"].get("status") in {"Running", "Paused", "Reset"} and
                    type(groups["default"].get("parallel_tasks")) is int and
                    groups["default"]["parallel_tasks"] >= 0,
                    "private queue default group is malformed")
        except (AcceptanceFailure, KeyError, TypeError):
            return False
    if not dispatch_attempts:
        return not rows
    expected: dict[str, tuple[str, str]] = {}
    for name, task in tasks.items():
        if task not in dispatch_attempts:
            return False
        label = labels.get(name)
        if not isinstance(label, str) or label in expected:
            return False
        expected[label] = (name, task)
    if len(expected) != len(dispatch_attempts) or {
            task for _, task in expected.values()} != dispatch_attempts:
        return False
    if len(rows) != len(expected):
        return False
    seen_labels: set[str] = set()
    for key, row in rows.items():
        if not isinstance(row, dict):
            return False
        if (not isinstance(key, str) or not key.isdecimal() or str(int(key)) != key or
                type(row.get("id")) is not int or row["id"] < 0 or row["id"] != int(key)):
            return False
        label = row.get("label")
        if not isinstance(label, str) or label not in expected or label in seen_labels:
            return False
        seen_labels.add(label)
        name, task = expected[label]
        if name not in numbers or type(numbers[name]) is not int or numbers[name] < 0:
            return False
        numeric = numbers[name]
        try:
            validate_queue_row(row, label, runner, state, task, numeric, "default")
            state_name = row_state(row)
        except (AcceptanceFailure, KeyError):
            return False
        if state_name != "Done" or done_result(row) is None:
            return False
    return seen_labels == set(expected)
