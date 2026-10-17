"""Bounded Pueue status capture and shared process-owner binding."""

from __future__ import annotations

from functools import partial
import json
import os

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.processes.operations import Process, Processes
from scripts.delegate_acceptance.provider_drivers.agy.constants import MAX_STATUS_BYTES

class StatusCapture:
    """Drain status through bounded memory; persist only JSON without envs."""

    def __init__(self):
        read_fd, write_fd = os.pipe()
        self.reader = os.fdopen(read_fd, "rb", buffering=0)
        self.writer = os.fdopen(write_fd, "wb", buffering=0)
        os.set_blocking(read_fd, False)
        self.buffer = bytearray()
        self.eof = False
        self.error = None

    def drain(self):
        # Bound each poll as well as total memory, including malicious output.
        for _ in range(128):
            try:
                chunk = os.read(self.reader.fileno(), 65536)
            except BlockingIOError:
                return
            if not chunk:
                self.eof = True
                self.reader.close()
                return
            if self.error is None:
                if len(self.buffer) + len(chunk) > MAX_STATUS_BYTES:
                    self.error = "status output exceeds bound"
                    self.buffer.clear()
                else:
                    self.buffer.extend(chunk)

    def sanitized(self, code):
        if self.error is not None:
            return b""
        if code != 0:
            self.buffer.clear()
            return b""
        def object_without_environment(pairs):
            result, seen = {}, set()
            for key, value in pairs:
                if key in seen:
                    raise ValueError("duplicate key")
                seen.add(key)
                if key != "envs":
                    result[key] = value
            return result
        def reject_constant(value):
            raise ValueError("nonfinite number")
        try:
            value = json.loads(self.buffer.decode("utf-8"),
                               object_pairs_hook=object_without_environment,
                               parse_constant=reject_constant)
            if not isinstance(value, dict) or not isinstance(value.get("tasks"), dict):
                raise ValueError("invalid status object")
            encoded = (json.dumps(value, ensure_ascii=True) + "\n").encode()
            if len(encoded) > MAX_STATUS_BYTES:
                self.error = "sanitized status output exceeds bound"
                return b""
            return encoded
        except (ValueError, UnicodeError, RecursionError):
            self.error = "status output is not valid bounded JSON"
            return b""
        finally:
            self.buffer.clear()

    def close(self):
        for stream in (self.writer, self.reader):
            try:
                stream.close()
            except (OSError, ValueError):
                pass

OwnedProcess = Process

# A provider compatibility factory must receive the caller's live ownership
# authority.  The sentinel makes omission fail closed at runtime while the
# ownership verifier sees the explicit owner binding through functools.partial.
_REQUIRED_PROCESS_OWNER = object()


def _agy_stdout_capture(argv: list[str]):
    return StatusCapture() if argv[-2:] == ["status", "--json"] else None


# Compatibility names retain the AGY acceptance imports; all lifecycle code
# remains in the shared Processes/Process implementation.
ProcessBook = partial(
    Processes,
    owner=_REQUIRED_PROCESS_OWNER,
    stdout_capture_factory=_agy_stdout_capture,
    environment_keys_only=True,
    return_result=True,
    failure_type=AcceptanceFailure,
)
