"""Pure inventory summary values and arithmetic; inventory owners supply the facts."""

from __future__ import annotations

from collections.abc import Sequence
from typing import TypeGuard, TypedDict


class TreeSummary(TypedDict):
    root_count: int
    file_count: int
    byte_count: int
    complete: bool
    count: int
    bytes: int


def is_tree_summary(value: object) -> TypeGuard[TreeSummary]:
    if not isinstance(value, dict):
        return False
    return (all(type(value.get(key)) is int and value[key] >= 0
                for key in ("root_count", "file_count", "byte_count", "count", "bytes"))
            and type(value.get("complete")) is bool and value["bytes"] == value["byte_count"])


def _empty_summary() -> TreeSummary:
    return {"root_count": 0, "file_count": 0, "byte_count": 0, "complete": True,
            "count": 0, "bytes": 0}


def _summary_with_removed(after: TreeSummary, victims: Sequence[object]) -> TreeSummary:
    summaries: list[TreeSummary] = []
    for victim in victims:
        summary = victim.get("before") if isinstance(victim, dict) else None
        if not is_tree_summary(summary) or summary["complete"] is not True:
            return {"root_count": 0, "file_count": 0, "byte_count": 0,
                    "complete": False, "count": 0, "bytes": 0}
        summaries.append(summary)
    root_count = after["root_count"] + sum(summary["root_count"] for summary in summaries)
    file_count = after["file_count"] + sum(summary["file_count"] for summary in summaries)
    byte_count = after["byte_count"] + sum(summary["byte_count"] for summary in summaries)
    return {"root_count": root_count, "file_count": file_count, "byte_count": byte_count,
            "complete": after["complete"], "count": root_count, "bytes": byte_count}
