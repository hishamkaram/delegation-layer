"""Pure codex task and conversation identity predicates."""

from __future__ import annotations

from typing import TypeGuard

from scripts.acceptance.provider_drivers.codex.constants import TASK_PATTERN
from scripts.acceptance.provider_drivers.codex.constants import UUID_PATTERN


def is_task_id(value: object) -> TypeGuard[str]:
    return isinstance(value, str) and TASK_PATTERN.fullmatch(value) is not None

def is_thread_id(value: object) -> TypeGuard[str]:
    return isinstance(value, str) and UUID_PATTERN.fullmatch(value) is not None
