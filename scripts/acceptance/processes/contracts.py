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


class OutputSink(Protocol):
    """Consumer-owned receiver of a bounded command's stdout, in arrival order.

    The command owner never retains delivered bytes. A sink refuses output by
    raising ``OSError``: the owner then terminates and waits for its child and
    reports ``command_capture_unavailable`` with the exact exit code. Any other
    exception propagates unchanged after the same closure.
    """

    def __call__(self, chunk: bytes, /) -> None:
        ...


@dataclass(frozen=True)
class TerminalIO:
    """Borrowed terminal descriptors; the caller keeps them through exact Wait.

    stderr retains the ordinary separate process capture. This port changes
    stdio only, never process/session ownership, signals or cleanup authority.
    """

    input_fd: int
    output_fd: int


@dataclass(frozen=True)
class BoundedCommandResult:
    """``output`` is empty when a sink received the stdout bytes instead.

    ``stderr`` is the separately captured error stream; it stays empty unless the
    command was run with ``capture_stderr``. A sink never receives it.
    """

    exit_code: int
    output: bytes
    stderr: bytes = b""


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


def command_closure_unproven(error: BaseException) -> bool:
    """Read the existing child owner's closure verdict through wrapping exceptions."""
    visited: set[int] = set()

    def unproven(failure: BaseException) -> bool:
        if id(failure) in visited:
            return False
        visited.add(id(failure))
        if isinstance(failure, BoundedCommandError):
            # An exact Wait result is authoritative even if cleanup also failed.
            return failure.exit_code is None and failure.reason in (
                CommandFailure.TERMINATION_FAILED, CommandFailure.TERMINATION_TIMEOUT)
        if isinstance(failure, BaseExceptionGroup):
            return any(unproven(child) for child in failure.exceptions)
        cause = failure.__cause__
        if cause is None and not failure.__suppress_context__:
            cause = failure.__context__
        return cause is not None and unproven(cause)

    return unproven(error)
