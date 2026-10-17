"""Pure claude task and conversation identity predicates."""

from __future__ import annotations

from scripts.delegate_acceptance.provider_drivers.claude.constants import TASK_PATTERN, UUID_PATTERN


def is_session_id(value: object) -> bool:
    return isinstance(value, str) and UUID_PATTERN.fullmatch(value) is not None

def is_task_id(value: object) -> bool:
    return isinstance(value, str) and TASK_PATTERN.fullmatch(value) is not None
