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
    BoundedCommandError, BoundedCommandResult, CommandFailure, OutputCapture, OutputSink,
)
from scripts.delegate_acceptance.processes.observation import inherited_environment, selected_environment
from pathlib import Path
from typing import IO, Callable, Mapping, NoReturn


class Process:
    """One started child whose capture and finalization progress it owns.

    ``on_started`` is the registry that receives the handle immediately after a
    successful Start, before any fallible setup.  A direct construction without
    it names no registry: a post-Start setup failure still retains the capture
    directory and output streams, but the raised error is the only report.

    Popen may create the child and still raise before returning its handle.
    Such an uncertain launch keeps its invocation and capture as the durable
    record and is handed to ``on_start_uncertain``; only faults before Popen is
    entered prove that no child exists and roll the capture back.
    """

    def __init__(self, directory, argv, cwd, environment, capture_root,
                 on_started=None, *,
                 stdin=None, stdout_capture: OutputCapture | None = None,
                 environment_keys_only=False, return_result=False,
                 failure_type=RuntimeError,
                 on_start_uncertain: Callable[[Process], None] | None = None):
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
        # Finalization progress survives a failed poll so a retry resumes it.
        self._ended_ns = None
        self._sanitized_stdout = None
        self._stdout_committed = 0
        ownership = DirectoryOwner()
        try:
            _require_capture_parent(capture_root=capture_root, directory=self.directory)
            ownership.create("process capture", self.directory, self.directory.parent)
            _record_process_invocation(cwd=cwd, environment_keys_only=environment_keys_only, process=self, stdin=stdin)
            self.stdout = self.stdout_path.open("xb")
            self.stderr = self.stderr_path.open("xb")
            child_stdout = self._stdout_capture.writer if self._stdout_capture is not None else self.stdout
        except BaseException as error:
            _rollback_unstarted_process(ownership=ownership, process=self, error=error)
            _raise_process_failure(error, self._failure_type)
        try:
            self.process = subprocess.Popen(
                self.argv,
                cwd=str(cwd),
                env=self.environment,
                stdin=subprocess.PIPE if stdin is not None else subprocess.DEVNULL,
                stdout=child_stdout,
                stderr=self.stderr,
                shell=False,
                close_fds=True,
            )
        except BaseException as error:
            try:
                if on_start_uncertain is not None:
                    on_start_uncertain(self)
            except BaseException as registration_error:
                _note_cleanup_failures(error, [("uncertain start registration", registration_error)])
            _retain_uncertain_process(ownership=ownership, process=self, error=error)
            _raise_process_failure(error, self._failure_type)
        try:
            if on_started is not None:
                on_started(self)
            if self._stdin_data is not None:
                os.set_blocking(self.process.stdin.fileno(), False)
            if self._stdout_capture is not None:
                self._stdout_capture.writer.close()
            ownership.transfer_child_to(capture_root)
        except BaseException as error:
            _retain_started_process(ownership=ownership, process=self, error=error)
            _raise_process_failure(error, self._failure_type)

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
        # Popen caches the exact Wait, so a retried finalization observes the
        # same exit; each step below resumes from its recorded progress.
        code = self.process.wait()
        if self._ended_ns is None:
            self._ended_ns = time.time_ns()
        _commit_sanitized_stdout(self, code)
        _close_finished_streams(self)
        self.result = _observed_result(self, code)
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
        # (argv, directory) of each start whose Popen raised without a handle
        # (see Process).  Each stays unresolved in every closure gate, keeps its
        # sequence number, and is read only as detached observation records.
        self._uncertain_starts = []

    def start(self, name, argv, cwd, stdin=None, on_spawned=None, *, environment: Mapping[str, str] | None = None):
        if callable(stdin) and on_spawned is None:
            on_spawned, stdin = stdin, None
        launch_environment = self.environment if environment is None else selected_environment(environment, self.failure_type)
        sequence = len(self.entries) + len(self._uncertain_starts) + 1
        directory = self.directory / ("%03d-%s" % (sequence, name))
        capture = self.stdout_capture_factory([str(arg) for arg in argv]) \
            if self.stdout_capture_factory is not None else None

        def register_started(process):
            # Process calls this immediately after Start, before any fallible
            # setup or receipt write.  ``entries`` is the shared owner; the
            # callback gives a higher-level owner the same exact handle.
            self.entries.append(process)
            if on_spawned is not None:
                on_spawned(process)

        def register_uncertain(process):
            self._uncertain_starts.append((tuple(process.argv), str(process.directory)))
        process = Process(directory, argv, cwd, launch_environment, self.capture_root,
                          stdin=stdin, stdout_capture=capture,
                          environment_keys_only=self.environment_keys_only,
                          return_result=self.return_result,
                          failure_type=self.failure_type,
                          on_started=register_started,
                          on_start_uncertain=register_uncertain)
        write_json(directory / "started.json", {
            "pid": process.pid, "started_ns": time.time_ns()})
        return process

    def run(self, name, argv, cwd, expected=0, timeout=15, stdin=None, *, environment: Mapping[str, str] | None = None):
        process = self.start(name, argv, cwd, stdin=stdin, environment=environment)
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
        return result + [{"pid": None, "argv": list(argv), "directory": directory}
                         for argv, directory in self._uncertain_starts]

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
        return ([{"pid": p.pid, "argv": p.argv, "directory": str(p.directory)} for p in active] +
                [{"pid": None, "argv": list(argv), "directory": directory}
                 for argv, directory in self._uncertain_starts])

    def finished_naturally(self, exclude=()):
        """Require positive natural exit evidence for every owned process."""
        excluded = tuple(exclude)
        for process in self.entries:
            if any(process is entry for entry in excluded):
                continue
            result = process.poll()
            if result is None or result.get("natural_wait") is not True:
                return False
        return not self._uncertain_starts


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


def _require_capture_parent(*, capture_root: OwnedDirectory, directory: Path) -> None:
    """Validate the transfer destination before any capture side effect."""
    require(isinstance(capture_root, OwnedDirectory),
            "process capture root identity token is invalid")
    require(directory.parent == capture_root.directory,
            "process capture is not a direct child of its capture root: " + str(directory))
    capture_root.assert_live()


def _raise_process_failure(error: BaseException, failure_type) -> NoReturn:
    if (isinstance(error, RuntimeError) and failure_type is not RuntimeError and
            not isinstance(error, failure_type)):
        raise failure_type(str(error)) from error
    raise error


def _note_cleanup_failures(error: BaseException, failures: list[tuple[str, BaseException]]) -> None:
    for label, failure in failures:
        error.add_note("%s failed: %s: %s" % (label, type(failure).__name__, failure))


def _close_parent_handles(process: Process) -> list[tuple[str, BaseException]]:
    """Close the parent's capture and stream handles; return each close fault."""
    failures = []
    resources = (("stdout capture close", process._stdout_capture),
                 ("stdout close", process.stdout), ("stderr close", process.stderr))
    for label, resource in resources:
        if resource is None:
            continue
        try:
            resource.close()
        except (OSError, ValueError) as close_error:
            failures.append((label, close_error))
    return failures


def _rollback_unstarted_process(
        *, ownership: DirectoryOwner,
        process: Process,
        error: BaseException,
) -> None:
    """Before Start nothing escaped: close every handle and remove the capture.

    A failed rollback leaves the path uncertain, so the same owner releases its
    remaining authority: the directory is retained and its pins are closed.
    """
    failures = _close_parent_handles(process)
    try:
        ownership.rollback()
    except (OSError, RuntimeError) as rollback_error:
        failures.append(("capture directory rollback", rollback_error))
        try:
            ownership.release()
        except (OSError, RuntimeError) as release_error:
            failures.append(("capture directory release", release_error))
    _note_cleanup_failures(error, failures)


def _retain_uncertain_process(
        *, ownership: DirectoryOwner,
        process: Process,
        error: BaseException,
) -> None:
    """Popen raised after entry: a child may exist, but no handle was returned.

    CPython can create the child and then fail reading its error pipe or closing
    parent pipe ends.  Like Popen's own failure cleanup, only parent handles
    close; the invocation and capture stay as the durable record and the pins
    are released without rollback.  No PID is guessed, signalled, or waited: the
    missing started and completed receipts are the recorded uncertainty.
    """
    failures = _close_parent_handles(process)
    try:
        ownership.release()
    except (OSError, RuntimeError) as release_error:
        failures.append(("capture directory release", release_error))
    _note_cleanup_failures(error, failures)
    error.add_note("process launch outcome uncertain; capture retained: " + str(process.directory))


def _retain_started_process(
        *, ownership: DirectoryOwner,
        process: Process,
        error: BaseException,
) -> None:
    """After Start the child keeps its streams and capture for natural poll/Wait.

    Only the parent's child stdin and duplicate capture writer close, so the
    child sees EOF and the capture reader can reach EOF.
    """
    failures = []
    if (process._stdin_data is not None and process._stdin_error is None and
            process._stdin_offset < len(process._stdin_data)):
        process._stdin_error = "stdin closed after post-start setup failure"
    process._close_stdin()
    capture = process._stdout_capture
    if capture is not None and not capture.writer.closed:
        try:
            capture.writer.close()
        except (OSError, ValueError) as close_error:
            failures.append(("stdout capture writer close", close_error))
    try:
        ownership.release()
    except (OSError, RuntimeError) as release_error:
        failures.append(("capture directory release", release_error))
    _note_cleanup_failures(error, failures)


def _commit_sanitized_stdout(process: Process, code: int) -> None:
    """Write sanitized capture once; unbuffered writes make progress the checkpoint."""
    capture = process._stdout_capture
    if capture is None:
        return
    if process._sanitized_stdout is None:
        process._sanitized_stdout = capture.sanitized(code)
    data = memoryview(process._sanitized_stdout)
    if process._stdout_committed == len(data):
        return
    if process.stdout.closed:
        raise process._failure_type(
            "stdout closed before sanitized capture was committed: " + str(process.directory))
    descriptor = process.stdout.fileno()
    while process._stdout_committed < len(data):
        written = os.write(descriptor, data[process._stdout_committed:])
        if written <= 0:
            raise OSError("sanitized stdout write made no progress")
        process._stdout_committed += written


def _close_finished_streams(process: Process) -> None:
    """Close each remaining stream; a retry closes only what is still open."""
    failures = []
    if process._stdout_capture is not None:
        try:
            # Capture close is idempotent for its writer and reader.
            process._stdout_capture.close()
        except (OSError, ValueError) as close_error:
            failures.append(("stdout capture close", close_error))
    for label, stream in (("stdout close", process.stdout), ("stderr close", process.stderr)):
        if stream.closed:
            continue
        try:
            stream.close()
        except (OSError, ValueError) as close_error:
            failures.append((label, close_error))
    if failures:
        first = failures[0][1]
        _note_cleanup_failures(first, failures[1:])
        raise first


def _observed_result(process: Process, code: int) -> dict[str, object]:
    result = {
        "pid": process.pid, "exit_code": code, "natural_wait": code >= 0,
        "ended_ns": process._ended_ns,
        "stdout_sha256": digest(process.stdout_path),
        "stderr_sha256": digest(process.stderr_path),
        "stdout_bytes": process.stdout_path.stat().st_size,
        "stderr_bytes": process.stderr_path.stat().st_size,
        "stdin_bytes_sent": process._stdin_offset,
        "stdin_complete": (process._stdin_data is None or
                           process._stdin_offset == len(process._stdin_data)),
        "stdin_error": process._stdin_error,
    }
    if process._stdout_capture is not None:
        result["stdout_redaction"] = "pueue-status-envs"
        result["capture_error"] = process._stdout_capture.error
    return result


_BOUNDED_COMMAND_READ_SIZE = 64 * 1024


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


def _bounded_command_pending_bytes(pipe: IO[bytes]) -> int | None:
    """Inspect a pipe without adding bytes beyond the capture bound."""
    try:
        import fcntl
        import termios

        result = fcntl.ioctl(pipe.fileno(), termios.FIONREAD, struct.pack("I", 0))
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


def _read_bounded_command_chunk(
        process: subprocess.Popen[bytes], pipe: IO[bytes], room: int) -> bytes:
    """Read one ready chunk of at most ``room`` bytes; ``b""`` is this pipe's EOF.

    With no room left, any pending or further byte is an overflow.
    """
    if room == 0:
        pending = _bounded_command_pending_bytes(pipe)
        if pending is not None:
            if pending > 0:
                raise BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED)
            chunk = b""
            if process.poll() is None:
                chunk = os.read(pipe.fileno(), 1)
            if chunk:
                raise BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED)
            return b""
        read_size = 1
    else:
        read_size = min(_BOUNDED_COMMAND_READ_SIZE, room)
    chunk = os.read(pipe.fileno(), read_size)
    if len(chunk) > room:
        raise BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED)
    return chunk


def _bounded_command_bytes(
        process: subprocess.Popen[bytes], max_output: int, timeout: float,
        sink: OutputSink | None = None, *, capture_stderr: bool = False) -> BoundedCommandResult:
    """Drain every captured pipe to its EOF under one shared bound and deadline, then Wait."""
    stdout, stderr = process.stdout, (process.stderr if capture_stderr else None)
    if stdout is None or (capture_stderr and stderr is None):
        raise BoundedCommandError(CommandFailure.CAPTURE_UNAVAILABLE)
    captured, errors = bytearray(), bytearray()
    # Each still-open pipe and its receiver; a pipe leaves at its own EOF.
    receivers: dict[IO[bytes], Callable[[bytes], None]] = {
        stdout: captured.extend if sink is None else sink}
    if stderr is not None:
        receivers[stderr] = errors.extend
    selector = None
    failure = None
    try:
        selector = selectors.DefaultSelector()
        for pipe in receivers:
            selector.register(pipe, selectors.EVENT_READ)
        total = 0
        deadline = time.monotonic() + timeout
        while receivers:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise BoundedCommandError(CommandFailure.TIMEOUT)
            events = selector.select(remaining)
            if not events:
                if process.poll() is None:
                    raise BoundedCommandError(CommandFailure.TIMEOUT)
                continue
            for key, _mask in events:
                chunk = _read_bounded_command_chunk(process, key.fileobj, max_output - total)
                if chunk:
                    receivers[key.fileobj](chunk)
                    total += len(chunk)
                else:
                    selector.unregister(key.fileobj)
                    del receivers[key.fileobj]
    except BaseException as error:
        failure = error
        raise
    finally:
        _close_bounded_command_selector(selector, failure)
    try:
        code = process.wait(timeout=max(0.0, deadline - time.monotonic()))
    except subprocess.TimeoutExpired:
        raise BoundedCommandError(CommandFailure.TIMEOUT) from None
    return BoundedCommandResult(code, bytes(captured), bytes(errors))


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


def _close_bounded_command_pipes(
        process: subprocess.Popen[bytes], failure: BaseException | None, *, capture_stderr: bool) -> None:
    """Attempt every owned pipe close; report the first fault and note any later one."""
    pipes = (("stdout", process.stdout),)
    if capture_stderr:
        pipes += (("stderr", process.stderr),)
    faults: list[tuple[str, BaseException]] = []
    for label, pipe in pipes:
        if pipe is not None:
            try:
                pipe.close()
            except BaseException as close_error:
                faults.append((f"{label} pipe close", close_error))
    if not faults:
        return
    (_label, first), *later = faults
    error = _bounded_command_error(first, process.returncode, True)
    _note_cleanup_failures(error, later)
    if failure is not None:
        raise error from failure
    if error is not first:
        raise error from first
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
        capture: bool = False, capture_stderr: bool = False, timeout: float = 30,
        cwd: Path | None = None, sink: OutputSink | None = None) -> BoundedCommandResult:
    """Own one finite direct control child through capture, failure cleanup and Wait.

    This operation alone may terminate its direct release-control child. The
    Process/Processes natural-only policy and provider/native scopes are separate.
    Cleanup failures retain the primary exception through ordinary chaining;
    unavailable Wait proof reports a typed closure failure with no exit code.
    A ``sink`` receives the bounded stdout stream instead of an in-memory copy
    and requires ``capture``; the bound is still enforced before delivery.
    ``capture_stderr`` also requires ``capture`` and returns stderr separately in
    ``BoundedCommandResult.stderr``; the streams are never merged and share the
    one ``max_output`` bound (sink-delivered bytes included) and one deadline.
    """
    timeout = _validate_bounded_command_bounds(timeout, max_output)
    if sink is not None and not capture:
        raise ValueError("bounded command sink requires capture")
    if capture_stderr and not capture:
        raise ValueError("bounded command stderr capture requires capture")
    try:
        process = subprocess.Popen(
            argv, env=environment, cwd=cwd, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE if capture else subprocess.DEVNULL,
            stderr=subprocess.PIPE if capture_stderr else subprocess.DEVNULL,
        )
    except subprocess.TimeoutExpired:
        raise BoundedCommandError(CommandFailure.TIMEOUT) from None
    except OSError:
        raise BoundedCommandError(CommandFailure.START_FAILED) from None
    failure = None
    try:
        if capture:
            return _bounded_command_bytes(process, max_output, timeout, sink, capture_stderr=capture_stderr)
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
        _close_bounded_command_pipes(process, failure, capture_stderr=capture_stderr)
