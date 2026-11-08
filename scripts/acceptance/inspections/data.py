"""Validated inspection facts; no process, task ledger, or I/O ownership."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import NotRequired, TypedDict

from scripts.acceptance.workspaces.data import WorkspaceAdmissionAttempt


class InspectionEvidence(TypedDict):
    task_id: str
    operation_id: str
    identity_id: str
    root_id: str
    group: str
    group_parallelism: int
    label: str
    numeric_task_id: int | None
    request: dict[str, object]
    request_digest: str
    receipt: dict[str, object] | None
    submission: dict[str, object] | None
    start: dict[str, object] | None
    result: dict[str, object] | None
    completion: dict[str, object] | None
    worker_observation: dict[str, object] | None
    managed_lifetime: dict[str, object] | None
    inspection_exit_authority: dict[str, object]
    ordinary_task_present: bool
    ordinary_task_matches: bool
    historical_aborted: bool
    attempt: WorkspaceAdmissionAttempt | None
    historical_attempts: NotRequired[list[InspectionEvidence]]


@dataclass(frozen=True)
class InspectionRequest:
    request_path: Path
    request: dict[str, object]
    operation_id: str
    label: str
    created: tuple[datetime, int]
    deadline: tuple[datetime, int]
    deadline_text: str
    binding: dict[str, object]
    group: str
    group_parallelism: int
    standalone: bool
    ordinary_present: bool
    ordinary_matches: bool
    attempt: WorkspaceAdmissionAttempt | None
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
