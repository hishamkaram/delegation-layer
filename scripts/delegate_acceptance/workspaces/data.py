"""Plain workspace admission records returned by their authoritative validators."""

from __future__ import annotations

from typing import TypedDict


class WorkspaceBinding(TypedDict):
    id: str
    reservation: int
    request_sha256: str
    budget_nanos: int


class WorkspaceAdmissionAttempt(TypedDict):
    root_id: str
    workspace_id: str
    task_id: str
    request_sha256: str
    sequence: int
    operation_id: str
    task_record: dict[str, object]
    task_digest: str
    binding: WorkspaceBinding
    resource_request: dict[str, object]
    resource_request_sha256: str
    reservation: dict[str, object]
    reservation_sha256: str
    closure: dict[str, object] | None
    closure_sha256: str | None


class InspectionIdentity(TypedDict):
    task_id: str
    operation_id: str
    task: dict[str, object]
    task_sha256: str
    attempt: WorkspaceAdmissionAttempt | None
