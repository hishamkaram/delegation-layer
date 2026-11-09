"""One owned PTY and a bounded byte transcript, not a terminal screenshot."""

from __future__ import annotations

import errno
import fcntl
import os
import pty
import re
import selectors
import struct
import termios
import time

from scripts.acceptance.processes.contracts import TerminalIO


MAX_TRANSCRIPT_BYTES = 4 * 1024 * 1024
_CSI = re.compile(rb"\x1b\[[0-?]*[ -/]*[@-~]")
_OSC = re.compile(rb"\x1b\].*?(?:\x07|\x1b\\)", re.DOTALL)
_MODE = re.compile(rb"\x1b\[\?([0-9;]+)([hl])")


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
            self.original = termios.tcgetattr(self.slave)
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
        self.drain(0.05)
        if termios.tcgetattr(self.slave) != self.original:
            raise RuntimeError("terminal termios was not restored")
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
