"""One process registry with finite child input, capture, and natural waiting."""

from __future__ import annotations

if __name__ != "scripts.delegate_acceptance.processes.operations":
    raise ImportError("Import shared owners through scripts.delegate_acceptance")

import json
import os
import selectors
import struct
import subprocess
import time
from scripts.delegate_acceptance.directories.operations import DirectoryOwner, OwnedDirectory
from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.files import digest, read_json, write_json
from scripts.delegate_acceptance.processes.contracts import (
    BoundedCommandError, BoundedCommandResult, CommandFailure, OutputCapture,
)
from scripts.delegate_acceptance.processes.observation import inherited_environment
from pathlib import Path
from typing import Callable, NoReturn


class Process:
    def __init__(self, directory, argv, cwd, environment, capture_root,
                 on_started=None, *,
                 stdin=None, stdout_capture: OutputCapture | None = None,
                 environment_keys_only=False, return_result=False,
                 failure_type=RuntimeError):
        self.directory = Path(directory)
        self.argv = [str(arg) for arg in argv]
        self.cwd = Path(cwd)
        self.environment = dict(environment)
        self.stdout = None
        self.stderr = None
        self.stdout_path = self.directory / "stdout"
        self.stderr_path = self.directory / "stderr"
        self.result = None
        self.process = None
        self._stdout_capture = stdout_capture
        self._return_result = return_result
        self._failure_type = failure_type
        require(callable(self._failure_type), "process failure type must be callable")
        self._stdin_data = bytes(stdin) if stdin is not None else None
        self._stdin_offset = 0
        self._stdin_error = None
        self._stdin_closed = False
        ownership = DirectoryOwner()
        started = False
        try:
            ownership.create("process capture", self.directory, self.directory.parent)
            _record_process_invocation(cwd=cwd, environment_keys_only=environment_keys_only, process=self, stdin=stdin)
            self.stdout = self.stdout_path.open("xb")
            self.stderr = self.stderr_path.open("xb")
            self.process = subprocess.Popen(
                self.argv,
                cwd=str(cwd),
                env=self.environment,
                stdin=subprocess.PIPE if stdin is not None else subprocess.DEVNULL,
                stdout=self._stdout_capture.writer if self._stdout_capture is not None else self.stdout,
                stderr=self.stderr,
                shell=False,
                close_fds=True,
            )
            started = True
            if self._stdin_data is not None:
                os.set_blocking(self.process.stdin.fileno(), False)
            if on_started is not None:
                on_started(self)
            ownership.transfer_child_to(capture_root)
            if self._stdout_capture is not None:
                self._stdout_capture.writer.close()
        except BaseException as error:
            _cleanup_failed_process_start(ownership=ownership, process=self, started=started)
            if (isinstance(error, RuntimeError) and self._failure_type is not RuntimeError and
                    not isinstance(error, self._failure_type)):
                raise self._failure_type(str(error)) from error
            raise

    @property
    def pid(self):
        return self.process.pid

    def _close_stdin(self):
        if self._stdin_closed:
            return
        stream = getattr(self.process, "stdin", None) if self.process is not None else None
        if stream is None:
            self._stdin_closed = True
            return
        try:
            stream.close()
        except (BrokenPipeError, OSError, ValueError):
            pass
        self._stdin_closed = True

    def _write_stdin(self, deadline=None):
        if (self._stdin_data is None or self._stdin_closed or self.process is None or
                getattr(self.process, "stdin", None) is None):
            return True
        if self._stdin_offset >= len(self._stdin_data):
            self._close_stdin()
            return True
        stream = self.process.stdin
        try:
            while self._stdin_offset < len(self._stdin_data):
                if deadline is not None and time.monotonic() >= deadline:
                    return False
                written = os.write(stream.fileno(),
                                  self._stdin_data[self._stdin_offset:])
                if written <= 0:
                    raise OSError("stdin write made no progress")
                self._stdin_offset += written
        except BlockingIOError:
            return False
        except BrokenPipeError as error:
            self._stdin_error = "child closed stdin before EOF: " + str(error)
            self._close_stdin()
            return True
        except OSError as error:
            self._stdin_error = "stdin write failed: " + str(error)
            self._close_stdin()
            return True
        self._close_stdin()
        return True

    def poll(self, deadline=None):
        if self.result is not None:
            completed = self.directory / "completed.json"
            require(not completed.is_symlink(),
                    "completed process receipt is a symlink: " + str(completed))
            if not completed.is_file():
                # A previous observation may have recorded the in-memory
                # result before a receipt write failed.  Retry that receipt
                # before treating the process as fully observed.
                write_json(completed, self.result)
            else:
                require(read_json(completed) == self.result,
                        "completed process receipt does not match observation: " + str(completed))
            return self.result
        if self._stdout_capture is not None and not self._stdout_capture.eof:
            self._stdout_capture.drain()
        if self.process.poll() is None:
            if not self._write_stdin(deadline):
                return None
        else:
            if (self._stdin_data is not None and self._stdin_offset < len(self._stdin_data) and
                    self._stdin_error is None):
                self._stdin_error = "child exited before stdin EOF"
            self._close_stdin()
        if self.process.poll() is None:
            return None
        if self._stdout_capture is not None and not self._stdout_capture.eof:
            return None
        code = self.process.wait()
        if self._stdout_capture is not None:
            self.stdout.write(self._stdout_capture.sanitized(code))
            self._stdout_capture.close()
        self.stdout.close()
        self.stderr.close()
        self.result = {
            "pid": self.pid, "exit_code": code, "natural_wait": code >= 0,
            "ended_ns": time.time_ns(),
            "stdout_sha256": digest(self.stdout_path),
            "stderr_sha256": digest(self.stderr_path),
            "stdout_bytes": self.stdout_path.stat().st_size,
            "stderr_bytes": self.stderr_path.stat().st_size,
            "stdin_bytes_sent": self._stdin_offset,
            "stdin_complete": self._stdin_data is None or self._stdin_offset == len(self._stdin_data),
            "stdin_error": self._stdin_error,
        }
        if self._stdout_capture is not None:
            self.result["stdout_redaction"] = "pueue-status-envs"
            self.result["capture_error"] = self._stdout_capture.error
        write_json(self.directory / "completed.json", self.result)
        return self.result

    def wait(self, timeout=15, expected=0):
        deadline = time.monotonic() + timeout
        while self.poll(deadline=deadline) is None:
            if time.monotonic() >= deadline:
                if (self._stdin_data is not None and self._stdin_offset < len(self._stdin_data) and
                        self._stdin_error is None):
                    self._stdin_error = "stdin write observation expired"
                self._close_stdin()
                marker = self.directory / "observation-expired.json"
                if not marker.exists():
                    write_json(marker, {"pid": self.pid, "timeout_seconds": timeout,
                                        "termination": "unknown", "signals_sent": 0,
                                        "stdin_bytes_sent": self._stdin_offset,
                                        "stdin_complete": self._stdin_data is None or
                                        self._stdin_offset == len(self._stdin_data)})
                raise self._failure_type(
                    "process observation expired; ownership retained: " + str(self.directory))
            time.sleep(0.025)
        if not self.result["natural_wait"]:
            raise self._failure_type("process ended by signal: " + str(self.directory))
        if self.result.get("capture_error"):
            raise self._failure_type("status capture failed: " + str(self.directory))
        if self.result.get("stdin_error"):
            raise self._failure_type("stdin write failed: " + str(self.directory))
        if expected is not None:
            allowed = {expected} if isinstance(expected, int) else set(expected)
            if self.result["exit_code"] not in allowed:
                raise self._failure_type(
                    "unexpected exit %s for %s" % (self.result["exit_code"], self.directory))
        return self.result if self._return_result else self.result["exit_code"]

    def output(self, bound=8 * 1024 * 1024):
        if self.result is None:
            raise self._failure_type("output requested before observed completion")
        data = self.stdout_path.read_bytes()
        if len(data) > bound:
            raise self._failure_type("owned stdout exceeds bound: " + str(self.directory))
        return data

    def json(self, bound=8 * 1024 * 1024):
        if self.result is None:
            raise self._failure_type("output requested before observed completion")
        if self.result.get("capture_error"):
            raise self._failure_type(
                "status capture incomplete or invalid: " + str(self.directory))
        return read_json(self.stdout_path, bound)


class Processes:
    def __init__(self, directory, environment=None, owner=None,
                 capture_root=None, stdout_capture_factory: Callable[[list[str]], OutputCapture | None] | None = None,
                 environment_keys_only=False, return_result=False,
                 failure_type=RuntimeError):
        self.directory = Path(directory)
        self.failure_type = failure_type
        require(callable(self.failure_type), "process failure type must be callable")
        self._capture_owner = None
        try:
            require(not (owner is not None and capture_root is not None),
                    "process capture accepts either owner or capture_root, not both")
            if owner is not None:
                require(isinstance(owner, DirectoryOwner),
                        "process capture owner is invalid")
            if capture_root is not None:
                require(isinstance(capture_root, OwnedDirectory),
                        "process capture root identity token is invalid")
                require(capture_root.directory == self.directory,
                        "process capture root identity token does not match path")
                self.capture_root = capture_root
            else:
                if self.directory.exists() or self.directory.is_symlink():
                    raise RuntimeError("process capture root identity token is required for an existing directory")
                capture_owner = owner if owner is not None else DirectoryOwner()
                self.capture_root = capture_owner.create(
                    "raw process captures", self.directory, self.directory.parent,
                    mode=0o700, parents=False)
                self._capture_owner = capture_owner if owner is None else None
        except BaseException as error:
            if (isinstance(error, RuntimeError) and self.failure_type is not RuntimeError and
                    not isinstance(error, self.failure_type)):
                raise self.failure_type(str(error)) from error
            raise
        self.environment = inherited_environment() if environment is None else dict(environment)
        self.stdout_capture_factory = stdout_capture_factory
        self.environment_keys_only = environment_keys_only
        self.return_result = return_result
        self.entries = []

    def start(self, name, argv, cwd, stdin=None, on_spawned=None):
        if callable(stdin) and on_spawned is None:
            on_spawned, stdin = stdin, None
        directory = self.directory / ("%03d-%s" % (len(self.entries) + 1, name))
        capture = self.stdout_capture_factory([str(arg) for arg in argv]) \
            if self.stdout_capture_factory is not None else None
        process = Process(directory, argv, cwd, self.environment, self.capture_root,
                          stdin=stdin, stdout_capture=capture,
                          environment_keys_only=self.environment_keys_only,
                          return_result=self.return_result,
                          failure_type=self.failure_type,
                          on_started=self.entries.append)
        # Transfer the actual handle to the lifecycle owner before any
        # fallible post-Start receipt write.  ``entries`` is the shared owner;
        # the callback gives a higher-level owner the same guarantee.
        if on_spawned is not None:
            on_spawned(process)
        write_json(directory / "started.json", {
            "pid": process.pid, "started_ns": time.time_ns()})
        return process

    def run(self, name, argv, cwd, expected=0, timeout=15, stdin=None):
        process = self.start(name, argv, cwd, stdin=stdin)
        process.wait(timeout, expected)
        return process

    def pending(self, exclude=()):
        excluded = tuple(exclude)
        result = []
        for process in self.entries:
            if any(process is entry for entry in excluded):
                continue
            observed = process.poll()
            if observed is None or observed.get("natural_wait") is not True:
                result.append({"pid": process.pid, "argv": process.argv,
                               "directory": str(process.directory)})
        return result

    def drain(self, timeout=70, exclude=()):
        """Observe finite owned processes after a failed assertion; never signal."""
        deadline = time.monotonic() + timeout
        excluded = tuple(exclude)

        def unresolved():
            pending = self.pending(excluded)
            return [process for process in self.entries
                    if any(process.pid == entry["pid"] for entry in pending)]

        active = unresolved()
        while active and time.monotonic() < deadline:
            if any(process.result is not None for process in active):
                break
            time.sleep(0.1)
            active = unresolved()
        return [{"pid": p.pid, "argv": p.argv, "directory": str(p.directory)} for p in active]

    def finished_naturally(self, exclude=()):
        """Require positive natural exit evidence for every owned process."""
        excluded = tuple(exclude)
        for process in self.entries:
            if any(process is entry for entry in excluded):
                continue
            result = process.poll()
            if result is None or result.get("natural_wait") is not True:
                return False
        return True


def _record_process_invocation(
        *, cwd: Path,
        environment_keys_only: bool,
        process: Process,
        stdin: bytes | None,
) -> None:
    executable = Path(process.argv[0])
    require(executable.is_absolute(), "executable must be absolute")
    invocation = {
        "argv": process.argv,
        "cwd": str(cwd),
        "executable_sha256": digest(executable),
        "stdin": "PIPE" if stdin is not None else "DEVNULL",
        "stdin_bytes": len(process._stdin_data) if process._stdin_data is not None else 0,
        "stdin_sha256": sha(process._stdin_data) if process._stdin_data is not None else None,
        "shell": False,
    }
    if environment_keys_only:
        invocation.update({
            "environment_keys": sorted(process.environment),
            "environment_values_in_record": False,
            "harness_environment_preserved": True,
        })
    else:
        invocation["environment_sha256"] = sha(
            json.dumps(process.environment, sort_keys=True).encode())
    write_json(process.directory / "invocation.json", invocation)


def _cleanup_failed_process_start(
        *, ownership: DirectoryOwner,
        process: Process,
        started: bool,
) -> None:
    if process.process is not None and getattr(process.process, "stdin", None) is not None:
        try:
            process.process.stdin.close()
        except (BrokenPipeError, OSError, ValueError):
            pass
    if process._stdout_capture is not None:
        try:
            process._stdout_capture.writer.close()
        except (OSError, ValueError):
            pass
        if process.process is None:
            process._stdout_capture.close()
    if process.stdout is not None:
        process.stdout.close()
    if process.stderr is not None:
        process.stderr.close()
    if not started:
        try:
            ownership.rollback()
        except (OSError, RuntimeError):
            pass
    else:
        try:
            ownership.release()
        except (OSError, RuntimeError):
            pass


def _kill_wait_bounded_command(process: subprocess.Popen[bytes]) -> None:
    try:
        process.kill()
    except ProcessLookupError:
        pass  # Absence still requires the exact child's Wait below.
    finally:
        process.wait(timeout=1)


def _terminate_bounded_command(process: subprocess.Popen[bytes]) -> None:
    try:
        _kill_wait_bounded_command(process)
    except subprocess.TimeoutExpired:
        try:
            _kill_wait_bounded_command(process)
        except subprocess.TimeoutExpired:
            raise BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT) from None


def _bounded_command_pending_bytes(process: subprocess.Popen[bytes]) -> int | None:
    """Inspect a pipe without adding bytes beyond the capture bound."""
    if process.stdout is None:
        return None
    try:
        import fcntl
        import termios

        result = fcntl.ioctl(process.stdout.fileno(), termios.FIONREAD, struct.pack("I", 0))
        return struct.unpack("I", result)[0]
    except (ImportError, OSError, struct.error, AttributeError):
        return None


def _close_bounded_command_selector(
        selector: selectors.BaseSelector | None, failure: BaseException | None) -> None:
    if selector is not None:
        try:
            selector.close()
        except BaseException as close_error:
            if failure is not None:
                raise failure from close_error
            raise


def _bounded_command_bytes(
        process: subprocess.Popen[bytes], max_output: int, timeout: float) -> BoundedCommandResult:
    if process.stdout is None:
        raise BoundedCommandError(CommandFailure.CAPTURE_UNAVAILABLE)
    selector = None
    failure = None
    try:
        selector = selectors.DefaultSelector()
        selector.register(process.stdout, selectors.EVENT_READ)
        captured = bytearray()
        deadline = time.monotonic() + timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise BoundedCommandError(CommandFailure.TIMEOUT)
            events = selector.select(remaining)
            if not events:
                if process.poll() is None:
                    raise BoundedCommandError(CommandFailure.TIMEOUT)
                continue
            if len(captured) == max_output:
                pending = _bounded_command_pending_bytes(process)
                if pending is not None:
                    if pending > 0:
                        raise BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED)
                    chunk = b""
                    if process.poll() is None:
                        chunk = os.read(process.stdout.fileno(), 1)
                    if chunk:
                        raise BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED)
                    break
                read_size = 1
            else:
                read_size = min(8192, max_output - len(captured))
            chunk = os.read(process.stdout.fileno(), read_size)
            if not chunk:
                break
            if len(captured) + len(chunk) > max_output:
                raise BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED)
            captured.extend(chunk)
    except BaseException as error:
        failure = error
        raise
    finally:
        _close_bounded_command_selector(selector, failure)
    try:
        code = process.wait(timeout=max(0.0, deadline - time.monotonic()))
    except subprocess.TimeoutExpired:
        raise BoundedCommandError(CommandFailure.TIMEOUT) from None
    return BoundedCommandResult(code, bytes(captured))


def _bounded_command_error(error: BaseException, code: int | None, capture: bool) -> BaseException:
    if isinstance(error, BoundedCommandError):
        # This operation enriches its original failure with the exact Wait result.
        error.exit_code = code
        return error
    if capture and isinstance(error, OSError):
        return BoundedCommandError(CommandFailure.CAPTURE_UNAVAILABLE, code)
    return error


def _abort_bounded_command(process: subprocess.Popen[bytes], primary: BaseException, capture: bool) -> NoReturn:
    try:
        _terminate_bounded_command(process)
    except BaseException as cleanup_error:
        primary = _bounded_command_error(primary, process.returncode, capture)
        if process.returncode is None:
            reason = (cleanup_error.reason if isinstance(cleanup_error, BoundedCommandError)
                      else CommandFailure.TERMINATION_FAILED)
            raise BoundedCommandError(reason) from primary
        raise primary from cleanup_error
    error = _bounded_command_error(primary, process.returncode, capture)
    if error is not primary:
        raise error from None
    raise error


def _close_bounded_command_pipe(process: subprocess.Popen[bytes], failure: BaseException | None) -> None:
    if process.stdout is not None:
        try:
            process.stdout.close()
        except BaseException as close_error:
            error = _bounded_command_error(close_error, process.returncode, True)
            if failure is not None:
                raise error from failure
            raise error


def _validate_bounded_command_bounds(timeout: float, max_output: int) -> float:
    if not isinstance(timeout, (int, float)):
        raise ValueError("bounded command timeout must be finite and positive")
    try:
        timeout = float(timeout)
    except OverflowError:
        raise ValueError("bounded command timeout must be finite and positive") from None
    if not 0 < timeout < float("inf"):
        raise ValueError("bounded command timeout must be finite and positive")
    if not isinstance(max_output, int) or max_output < 0:
        raise ValueError("bounded command capture limit must be a nonnegative integer")
    return timeout


def run_bounded_command(
        argv: list[str], environment: dict[str, str], *, max_output: int,
        capture: bool = False, timeout: float = 30) -> BoundedCommandResult:
    """Own one finite direct control child through capture, failure cleanup and Wait.

    This operation alone may terminate its direct release-control child. The
    Process/Processes natural-only policy and provider/native scopes are separate.
    Cleanup failures retain the primary exception through ordinary chaining;
    unavailable Wait proof reports a typed closure failure with no exit code.
    """
    timeout = _validate_bounded_command_bounds(timeout, max_output)
    try:
        process = subprocess.Popen(
            argv, env=environment, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE if capture else subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except subprocess.TimeoutExpired:
        raise BoundedCommandError(CommandFailure.TIMEOUT) from None
    except OSError:
        raise BoundedCommandError(CommandFailure.START_FAILED) from None
    failure = None
    try:
        if capture:
            return _bounded_command_bytes(process, max_output, timeout)
        try:
            code = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            raise BoundedCommandError(CommandFailure.TIMEOUT) from None
        return BoundedCommandResult(code, b"")
    except BaseException as error:
        try:
            _abort_bounded_command(process, error, capture)
        except BaseException as final_error:
            failure = final_error
            raise
    finally:
        _close_bounded_command_pipe(process, failure)
