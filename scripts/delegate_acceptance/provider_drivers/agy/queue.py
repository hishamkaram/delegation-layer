"""Pueue status JSON parsing and exact row predicates."""

from __future__ import annotations

from pathlib import Path

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.supervisor.queue_policy import (
    validate_queue_row as validate_exact_queue_row,
)
from scripts.delegate_acceptance.provider_drivers.agy.constants import MAX_STATUS_BYTES
from scripts.delegate_acceptance.provider_drivers.agy.process_capture import OwnedProcess

def parse_json_output(process: OwnedProcess, label: str) -> dict[str, object]:
    try:
        value = process.json()
    except (OSError, ValueError, TypeError) as error:
        raise AcceptanceFailure(f"{label} did not produce one JSON object: {error}") from error
    require(isinstance(value, dict), f"{label} JSON response must be an object")
    return value

def status_jobs(process: OwnedProcess) -> dict[str, object]:
    value = process.json(MAX_STATUS_BYTES)
    require(isinstance(value, dict) and isinstance(value.get("tasks"), dict), "pueue status JSON has no tasks object")
    require(isinstance(value.get("groups"), dict), "pueue status JSON has no groups object")
    return value

def status_row(status: dict[str, object], task_number: int) -> dict[str, object] | None:
    tasks = status["tasks"]
    require(isinstance(tasks, dict), "pueue tasks is not an object")
    row = tasks.get(str(task_number))
    return row if isinstance(row, dict) else None

def row_state(row: dict[str, object]) -> str:
    status = row.get("status")
    require(isinstance(status, dict), "pueue row status is not a one-state object")
    recognized = [name for name in status
                  if name in {"Queued", "Running", "Paused", "Stashed", "Done", "Locked"}]
    require(len(status) == len(recognized),
            "pueue row status contains an unknown lifecycle variant")
    require(len(recognized) == 1, "pueue row status is not a one-state object")
    return recognized[0]

def validate_queue_row(row: dict[str, object], label: str, runner: Path, state: Path, task: str) -> None:
    require(type(row.get("id")) is int and row["id"] >= 0,
            f"pueue numeric identity is invalid for {task}")
    validate_exact_queue_row(
        row, label, runner, state, task, row["id"], "default", supervised=True)
    command = f"{row['original_command']}\n{row['command']}"
    require("--brief" not in command and "OUTSIDE-WRITE-PROBE" not in command,
            f"brief content leaked into pueue command for {task}")

def pueue_command(pueue: Path, config: Path, *args: str) -> list[str]:
    return [str(pueue), "-c", str(config), *args]
