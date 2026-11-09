"""One owned PTY and a bounded byte transcript, not a terminal screenshot."""

from __future__ import annotations

from collections.abc import Sequence
import errno
import fcntl
import json
import os
import pty
import re
import selectors
import struct
import termios
import time

from scripts.acceptance.processes.contracts import TerminalIO


MAX_TRANSCRIPT_BYTES = 4 * 1024 * 1024
MAX_TERMIOS_CONTROL_SLOTS = 64
MAX_TERMIOS_DIAGNOSTIC_BYTES = 4096
_CSI = re.compile(rb"\x1b\[[0-?]*[ -/]*[@-~]")
_OSC = re.compile(rb"\x1b\].*?(?:\x07|\x1b\\)", re.DOTALL)
_MODE = re.compile(rb"\x1b\[\?([0-9;]+)([hl])")


class TerminalTermiosMismatch(RuntimeError):
    """The still-open observer read terminal attributes unequal to its baseline."""

    def __init__(self, before: Sequence[object], after: Sequence[object]):
        evidence = {"before": _termios_snapshot(before), "after": _termios_snapshot(after)}
        message = "terminal termios was not restored; " + json.dumps(evidence, separators=(",", ":"))
        if len(message.encode("ascii")) > MAX_TERMIOS_DIAGNOSTIC_BYTES:
            raise RuntimeError("terminal termios diagnostic exceeded bound")
        super().__init__(message)


def _termios_snapshot(state: Sequence[object]) -> dict[str, object]:
    names = ("iflag", "oflag", "cflag", "lflag", "ispeed", "ospeed")
    if len(state) != 7 or any(not isinstance(value, int) or isinstance(value, bool)
                             or not -(1 << 63) <= value < 1 << 64 for value in state[:6]):
        raise RuntimeError("terminal termios numeric attributes are invalid")
    controls = state[6]
    if not isinstance(controls, list) or len(controls) > MAX_TERMIOS_CONTROL_SLOTS:
        raise RuntimeError("terminal termios control slots exceeded bound")
    slots: list[tuple[str, int]] = []
    for value in controls:
        if isinstance(value, bytes) and len(value) == 1:
            slots.append(("bytes", value[0]))
        elif isinstance(value, int) and not isinstance(value, bool) and 0 <= value <= 255:
            slots.append(("int", value))
        else:
            raise RuntimeError("terminal termios control attribute is invalid")
    return {**dict(zip(names, state[:6], strict=True)), "cc": slots}


def plain_text(data: bytes) -> str:
    return _CSI.sub(b"", _OSC.sub(b"", data)).decode("utf-8", errors="replace")


def restored_modes(data: bytes) -> bool:
    modes = {25: True, 1049: False, 2004: False, 1000: False,
             1002: False, 1003: False, 1006: False}
    for match in _MODE.finditer(data):
        for number in match[1].split(b";"):
            mode = int(number)
            if mode in modes:
                modes[mode] = match[2] == b"h"
    return modes == {25: True, 1049: False, 2004: False, 1000: False,
                     1002: False, 1003: False, 1006: False}


class Terminal:
    """The caller owns descriptors until the exact process Wait is observed."""

    def __init__(self):
        self.master, self.slave = pty.openpty()
        try:
            # A controlling session's exit revokes Darwin slave descriptors.
            # Keep the same live master as the initial and final observer.
            self.original = termios.tcgetattr(self.master)
            self.resize(110, 35)
            os.set_blocking(self.master, False)
            self.selector = selectors.DefaultSelector()
            self.selector.register(self.master, selectors.EVENT_READ)
        except BaseException:
            os.close(self.master)
            os.close(self.slave)
            raise
        self.data = bytearray()

    @property
    def stdio(self) -> TerminalIO:
        return TerminalIO(self.slave, self.slave)

    def resize(self, width: int, height: int) -> None:
        fcntl.ioctl(self.slave, termios.TIOCSWINSZ,
                    struct.pack("HHHH", height, width, 0, 0))

    def send(self, data: bytes) -> None:
        if self.master < 0:
            raise RuntimeError("terminal master is already closed")
        offset = 0
        deadline = time.monotonic() + 5
        while offset < len(data):
            try:
                offset += os.write(self.master, data[offset:])
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise RuntimeError("terminal input deadline exceeded") from None
                self.drain(0.025)

    def drain(self, timeout: float = 0.05) -> None:
        if self.master < 0:
            return
        for _key, _mask in self.selector.select(timeout):
            try:
                chunk = os.read(self.master, 65536)
            except BlockingIOError:
                continue
            except OSError as error:
                if error.errno == errno.EIO:
                    return
                raise
            if len(self.data) + len(chunk) > MAX_TRANSCRIPT_BYTES:
                raise RuntimeError("terminal transcript exceeded bound")
            self.data.extend(chunk)

    def wait_text(self, text: str, *, after: int = 0, timeout: float = 10) -> None:
        deadline = time.monotonic() + timeout
        while text not in plain_text(bytes(self.data[after:])):
            if time.monotonic() >= deadline:
                raise RuntimeError("terminal did not display " + repr(text))
            self.drain(min(0.05, max(0.0, deadline - time.monotonic())))

    def assert_restored(self) -> None:
        if self.master < 0:
            raise RuntimeError("terminal master is already closed")
        self.drain(0.05)
        observed = termios.tcgetattr(self.master)
        if observed != self.original:
            raise TerminalTermiosMismatch(self.original, observed)
        data = bytes(self.data)
        if b"\x1b[?1049h" not in data:
            raise RuntimeError("TUI never entered alternate screen")
        if not restored_modes(data):
            raise RuntimeError("terminal cursor/screen/paste/mouse modes were not restored")

    def hangup(self) -> None:
        """Release our master; the kernel owns controlling-terminal HUP."""
        if self.master >= 0:
            self.selector.unregister(self.master)
            os.close(self.master)
            self.master = -1

    def close(self) -> None:
        try:
            self.hangup()
        finally:
            try:
                self.selector.close()
            finally:
                if self.slave >= 0:
                    os.close(self.slave)
                    self.slave = -1
