"""Validated inspection facts; no process, task ledger, or I/O ownership."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from pathlib import Path


@dataclass(frozen=True)
class InspectionRequest:
    request_path: Path
    request: dict[str, object]
    operation_id: str
    label: str
    created: tuple[datetime, int]
    deadline: tuple[datetime, int]
    binding: dict[str, object]
    group: str
    group_parallelism: int
    standalone: bool
    ordinary_present: bool
    ordinary_matches: bool
    attempt: dict[str, object] | None
    historical_aborted: bool


@dataclass(frozen=True)
class InspectionCompletion:
    receipt: dict[str, object] | None
    receipt_id: int | None
    submission: dict[str, object] | None
    start: dict[str, object] | None
    result: dict[str, object] | None
    completion: dict[str, object] | None
    observation: dict[str, object] | None
    managed_worker_succeeded: bool
    start_created: tuple[datetime, int] | None
