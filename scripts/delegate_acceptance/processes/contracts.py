"""Consumer-owned capture protocol for the process lifecycle."""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
from typing import BinaryIO, Protocol


class OutputCapture(Protocol):
    """Bounded stdout capture owned by the shared process lifecycle."""

    writer: BinaryIO
    eof: bool
    error: str | None

    def drain(self) -> None:
        ...

    def sanitized(self, code: int) -> bytes:
        ...

    def close(self) -> None:
        ...


@dataclass(frozen=True)
class BoundedCommandResult:
    exit_code: int
    output: bytes


class CommandFailure(Enum):
    START_FAILED = "command_start_failed"
    TIMEOUT = "command_timeout"
    TERMINATION_TIMEOUT = "command_termination_timeout"
    TERMINATION_FAILED = "command_termination_failed"
    CAPTURE_UNAVAILABLE = "command_capture_unavailable"
    OUTPUT_EXCEEDED = "command_output_exceeded"


class BoundedCommandError(Exception):
    def __init__(self, reason: CommandFailure, exit_code: int | None = None):
        super().__init__(reason.value)
        self.reason = reason
        self.exit_code = exit_code
