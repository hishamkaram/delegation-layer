"""Own unique event observation names for one acceptance run."""

from __future__ import annotations


class InteractionObserver:
    def __init__(self) -> None:
        self._observation_number = 0

    @property
    def observation_number(self) -> int:
        return self._observation_number

    def next_name(self) -> str:
        self._observation_number += 1
        return f"interactive-events-{self._observation_number}"
