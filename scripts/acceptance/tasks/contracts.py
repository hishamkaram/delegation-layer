"""Process capabilities read by the task ledger; lifecycle ownership stays external."""

from __future__ import annotations

from collections.abc import Iterable, Mapping, Sequence
from pathlib import Path
from typing import Protocol

from scripts.acceptance.evidence.records import ProcessOutput


class TaskCommand(ProcessOutput, Protocol):
    """An observed command's capture; JSON remains untrusted until validated."""

    @property
    def result(self) -> Mapping[str, object] | None:
        ...

    def json(self, bound: int = 8 * 1024 * 1024) -> object:
        ...


class TaskDaemon(Protocol):
    """The exact daemon handle retained by the process owner, never a copied PID."""

    def poll(self, deadline: float | None = None) -> object | None:
        ...

    def wait(self, timeout: float = 15, expected: int | set[int] | None = 0) -> object:
        ...


class TaskProcesses(Protocol):
    """The task ledger can launch commands and request natural completion."""

    def run(self, name: str, argv: list[object], cwd: Path,
            expected: int | set[int] | None = 0, timeout: float = 15,
            stdin: bytes | None = None, *, environment: Mapping[str, str] | None = None) -> TaskCommand:
        ...

    def drain(self, timeout: float = 70,
              exclude: Iterable[object] = ()) -> Sequence[Mapping[str, object]]:
        ...
