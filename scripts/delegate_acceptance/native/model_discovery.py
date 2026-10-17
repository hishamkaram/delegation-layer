"""Authoritative mutable state for one model discovery operation."""

from __future__ import annotations

from dataclasses import dataclass

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure


@dataclass
class ModelDiscoveryState:
    response: dict[str, object] | None = None
    summary: dict[str, object] | None = None
    exit_code: int | None = None
    root_id: str | None = None
    task_id: str | None = None

    def record_observation(
            self, response: dict[str, object], summary: dict[str, object],
            exit_code: int) -> None:
        self.response = response
        self.summary = summary
        self.exit_code = exit_code

    def record_task(self, task_id: str) -> None:
        if self.task_id is None:
            self.task_id = task_id
        elif self.task_id != task_id:
            raise AcceptanceFailure("model discovery task identity changed")
