"""Finite observation of exact runner rows through the supplied client."""

from __future__ import annotations

import time
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.supervisor.queue_policy import done_result
from typing import Callable, Protocol


class StatusResponse(Protocol):
    def json(self) -> object:
        ...


class StatusClient(Protocol):
    def __call__(self, name: str, arguments: list[str], /, *, timeout: float) -> StatusResponse:
        ...


# Hermetic consumers keep the established fast cadence; a consumer whose
# every observation is a retained process capture chooses a slower one.
RUNNER_POLL_SECONDS = 0.1


def wait_runner_terminal(
    client: StatusClient,
    root_id: str,
    task_id: str,
    expected_result: str | frozenset[str],
    timeout: float = 150,
    sleep_fn: Callable[[float], None] = time.sleep,
    monotonic_fn: Callable[[], float] = time.monotonic,
    *,
    poll_seconds: float = RUNNER_POLL_SECONDS,
) -> dict[str, object]:
    """Observe one exact delegate runner row until it reaches one expected result.

    Every status call and every pause is clamped to the remaining deadline, and
    no status call starts once the deadline has passed. Only a terminal row the
    client actually returned can satisfy the wait.
    """
    require(type(poll_seconds) in (int, float) and 0 < poll_seconds < float("inf"),
            "runner poll interval must be a positive finite number of seconds")
    label = "delegate:" + root_id + ":" + task_id
    deadline = monotonic_fn() + timeout
    while True:
        remaining = deadline - monotonic_fn()
        if remaining <= 0:
            break
        result = client("runner-status", ["status", "--json"], timeout=remaining)
        value = result.json()
        if not isinstance(value, dict):
            raise AcceptanceFailure("supervisor status response is not an object")
        rows = value.get("tasks", {})
        if not isinstance(rows, dict):
            raise AcceptanceFailure("supervisor status has no tasks object")
        matching = [row for row in rows.values()
                    if isinstance(row, dict) and row.get("label") == label]
        require(len(matching) == 1, "submitted task has no unique supervisor row")
        row = matching[0]
        status = row.get("status")
        if isinstance(status, dict) and "Done" in status:
            actual_result = done_result(row)
            if isinstance(expected_result, str):
                accepted = actual_result == expected_result
                expectation = repr(expected_result)
            else:
                require(isinstance(expected_result, frozenset) and expected_result and
                        all(isinstance(item, str) for item in expected_result),
                        "expected supervisor results are invalid")
                accepted = actual_result in expected_result
                expectation = "one of " + repr(sorted(expected_result))
            require(accepted,
                    f"supervisor result {actual_result!r}, expected {expectation}")
            return row
        remaining = deadline - monotonic_fn()
        if remaining <= 0:
            break
        sleep_fn(min(poll_seconds, remaining))
    raise RuntimeError("runner completion was not observed within acceptance deadline")


def wait_runner_done(
    client: StatusClient,
    root_id: str,
    task_id: str,
    timeout: float = 150,
    sleep_fn: Callable[[float], None] = time.sleep,
    monotonic_fn: Callable[[], float] = time.monotonic,
) -> dict[str, object]:
    """Observe one exact delegate runner row until it finishes successfully."""
    return wait_runner_terminal(client, root_id, task_id, "Success", timeout, sleep_fn, monotonic_fn)
