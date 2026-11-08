"""Immutable prelaunch request intents for task launches Delegate may identify later.

Owner and writer: ``NativeTaskOps`` retains one ``TaskRequestIntent`` per request that
carries caller metadata, before the fallible command runs, and binds a continuation's
Delegate-assigned successor at most once. Readers: the future native closure projection
and validator. An intent is observational: it grants no launch permission, closure,
NoStart or cleanup authority, and an unbound continuation stays an explicit unknown.
"""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from dataclasses import dataclass

from scripts.acceptance.evidence.validation import _hex_digest
from scripts.acceptance.evidence.validation import _task_identity
from scripts.acceptance.evidence.validation import require


DISPATCH = "dispatch"
CONTINUE = "continue"


def _optional_text(*values: object) -> bool:
    return all(value is None or isinstance(value, str) for value in values)


@dataclass(frozen=True)
class TaskRequest:
    """The caller's known inputs for one potential launch.

    Configuration fields are the owner inputs the command was built from; they are None
    when the command does not carry them, as for a continuation that inherits them.
    """

    role: str
    brief_sha256: str
    budget: str
    predecessor_task_id: str | None = None
    provider: str | None = None
    permission: str | None = None
    scenario: str | None = None
    model: str | None = None
    effort: str | None = None
    cwd: str | None = None

    def __post_init__(self) -> None:
        require(isinstance(self.role, str) and self.role, "task request role is invalid")
        require(_hex_digest(self.brief_sha256), "task request brief digest is invalid")
        require(isinstance(self.budget, str) and self.budget, "task request budget is invalid")
        require(self.predecessor_task_id is None or _task_identity(self.predecessor_task_id),
                "task request predecessor identity is invalid")
        require(_optional_text(self.provider, self.permission, self.scenario, self.model,
                               self.effort, self.cwd),
                "task request configuration is invalid")


@dataclass(frozen=True)
class TaskRequestIntent:
    """One request retained before its launch; a dispatch knows its exact task identity."""

    ordinal: int
    command: str
    state_root: str
    root_id: str | None
    task_id: str | None
    request: TaskRequest

    def __post_init__(self) -> None:
        require(type(self.ordinal) is int and self.ordinal >= 0, "task request ordinal is invalid")
        require(isinstance(self.state_root, str) and self.state_root, "task request state root is invalid")
        require(self.root_id is None or _task_identity(self.root_id), "task request root identity is invalid")
        require(isinstance(self.request, TaskRequest), "task request inputs are invalid")
        if self.command == DISPATCH:
            require(_task_identity(self.task_id), "dispatch request has no exact task identity")
        else:
            require(self.command == CONTINUE and self.task_id is None and
                    self.request.predecessor_task_id is not None,
                    "continuation request must name its predecessor and no successor")


def require_successor_binding(intents: Sequence[TaskRequestIntent], successors: Mapping[int, str],
                              ordinal: int, task: str) -> None:
    """Allow one successor per declared continuation; refuse any conflicting binding."""
    require(type(ordinal) is int and 0 <= ordinal < len(intents) and
            intents[ordinal].command == CONTINUE,
            "successor binding has no declared continuation request")
    require(_task_identity(task), "continuation successor identity is invalid")
    require(successors.get(ordinal, task) == task,
            "continuation request is already bound to another successor")
    require(all(value != task for other, value in successors.items() if other != ordinal),
            "continuation successor is already bound to another request")
