from scripts.delegate_acceptance.private_cli.environment import phase_environment

"""File-backed capture and exact reaping of directly spawned control commands."""

from scripts.delegate_acceptance.directories.operations import OwnedDirectory
from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.processes.observation import wait_until
from contextlib import contextmanager
import io
import os
import subprocess
import threading
import time
import uuid


CAPTURE_START_LOCK = threading.Lock()


def _assert_capture_root(capture_root):
    require(isinstance(capture_root, OwnedDirectory), "private control capture root identity token is required")
    capture_root.assert_live()


def _capture_path(capture_root, name):
    return capture_root.directory / name


def _capture_opener(capture_root, name, read_only):
    def open_at(_path, flags):
        _assert_capture_root(capture_root)
        flags |= getattr(os, "O_CLOEXEC", 0)
        if read_only:
            flags |= getattr(os, "O_NOFOLLOW", 0)
            return os.open(name, flags, dir_fd=capture_root.directory_fd)
        return os.open(name, flags, 0o600, dir_fd=capture_root.directory_fd)

    return open_at


def _open_capture_writer(capture_root, name):
    _assert_capture_root(capture_root)
    return io.open(name, "xb", opener=_capture_opener(capture_root, name, False))


def _read_capture(capture_root, name):
    _assert_capture_root(capture_root)
    with io.open(name, "rb", opener=_capture_opener(capture_root, name, True)) as stream:
        return stream.read()


def _wait_private_control_process(process, timeout, description):
    wait_until(
        lambda: process.poll() is not None,
        timeout=timeout,
        description=description,
        timeout_error=subprocess.TimeoutExpired(process.args, timeout),
    )
    require(process.returncode is not None, "private control process returned without an exit status")
    return process.returncode


def _reap_private_control_process(process, primary_error, stdout_path, stderr_path):
    try:
        if process.poll() is None:
            # This process is the directly spawned private control CLI. It is
            # never a managed worker, provider, daemon, or native task scope.
            process.kill()
        _wait_private_control_process(process, 5, "private control command cleanup")
    except BaseException as cleanup_error:
        raise RuntimeError(
            "private control command cleanup failed after %s: %s; retained capture files: %s, %s"
            % (primary_error, cleanup_error, stdout_path, stderr_path)
        ) from primary_error


def _retain_capture_failure(error, stdout_path, stderr_path):
    error.retained_capture_paths = (str(stdout_path), str(stderr_path))


@contextmanager
def _launch_lock(deadline):
    """Hold the capture start lock; with an absolute deadline, bound the wait and refuse an expired launch."""
    if deadline is None:
        with CAPTURE_START_LOCK:
            yield
        return
    if not CAPTURE_START_LOCK.acquire(timeout=max(0.0, deadline - time.monotonic())):
        raise TimeoutError("private control command deadline expired while waiting to launch")
    try:
        if time.monotonic() >= deadline:
            raise TimeoutError("private control command deadline expired before launch")
        yield
    finally:
        CAPTURE_START_LOCK.release()


def run_captured_command(command, env, timeout, *, capture_root, deadline=None):
    """Run one private control CLI with capture owned by the acceptance root.

    Pass exactly one budget. ``timeout`` is relative and starts at launch.
    ``deadline`` is an absolute monotonic deadline: it bounds the launch-lock
    wait and the wait gets only what remains. An already-expired deadline is
    refused under the lock before any capture file exists. Expiry during
    capture preparation is refused by a final cooperative check immediately
    before the spawn; that refusal retains its empty capture files in the
    owned capture root and reports them as retained capture paths.
    """
    _assert_capture_root(capture_root)
    require((timeout is None) != (deadline is None), "private control command needs exactly one of timeout or deadline")
    wait_budget = timeout
    capture_id = uuid.uuid4().hex
    stdout_name = "capture-" + capture_id + ".stdout"
    stderr_name = "capture-" + capture_id + ".stderr"
    stdout_path = _capture_path(capture_root, stdout_name)
    stderr_path = _capture_path(capture_root, stderr_name)
    process = None
    try:
        with _launch_lock(deadline):
            _assert_capture_root(capture_root)
            with (
                _open_capture_writer(capture_root, stdout_name) as output,
                _open_capture_writer(capture_root, stderr_name) as errors,
            ):
                if deadline is not None and time.monotonic() >= deadline:
                    expired = TimeoutError("private control command deadline expired during capture preparation")
                    _retain_capture_failure(expired, stdout_path, stderr_path)
                    raise expired
                process = subprocess.Popen(command, env=env, stdout=output, stderr=errors)
        if deadline is not None:
            wait_budget = deadline - time.monotonic()
        code = _wait_private_control_process(process, wait_budget, "private control command")
        with CAPTURE_START_LOCK:
            captured_stdout = _read_capture(capture_root, stdout_name)
            captured_stderr = _read_capture(capture_root, stderr_name)
        return subprocess.CompletedProcess(command, code, captured_stdout, captured_stderr)
    except subprocess.TimeoutExpired as primary_error:
        if process is not None:
            _reap_private_control_process(process, primary_error, stdout_path, stderr_path)
        with CAPTURE_START_LOCK:
            try:
                captured_stdout = _read_capture(capture_root, stdout_name)
                captured_stderr = _read_capture(capture_root, stderr_name)
            except BaseException as read_error:
                _retain_capture_failure(primary_error, stdout_path, stderr_path)
                raise primary_error from read_error
        raise subprocess.TimeoutExpired(command, wait_budget, output=captured_stdout, stderr=captured_stderr) from None
    except BaseException as primary_error:
        if process is not None:
            _reap_private_control_process(process, primary_error, stdout_path, stderr_path)
        raise


def run_captured_text_command(command, env, timeout, *, capture_root):
    """Run one control command through the shared file-backed capture owner."""
    captured = run_captured_command(command, env, timeout, capture_root=capture_root)
    return subprocess.CompletedProcess(
        captured.args, captured.returncode, captured.stdout.decode("utf-8"), captured.stderr.decode("utf-8")
    )


def reap_captured_process(process, timeout):
    """Own one bounded communicate/Wait and force-reap this fixture child on timeout."""
    try:
        stdout, stderr = process.communicate(timeout=timeout)
    except subprocess.TimeoutExpired as timeout_error:
        stdout = timeout_error.output or ""
        stderr = timeout_error.stderr or ""
        try:
            # The cooperative gate is released before production calls this
            # reaper. Force-reaping is limited to the harness-owned control CLI.
            process.kill()
            stdout, stderr = process.communicate(timeout=5)
        except (OSError, subprocess.TimeoutExpired) as cleanup_error:
            return (
                stdout,
                stderr,
                False,
                RuntimeError(
                    "fixture process could not be reaped after timeout: %s; stdout=%r stderr=%r"
                    % (cleanup_error, output_tail(stdout), output_tail(stderr))
                ),
            )
        return (
            stdout,
            stderr,
            True,
            RuntimeError(
                "fixture process exceeded its %ss observation bound; stdout=%r stderr=%r"
                % (timeout, output_tail(stdout), output_tail(stderr))
            ),
        )
    except OSError as error:
        return "", "", False, RuntimeError("fixture process wait failed: " + str(error))
    if process.returncode is None:
        return (stdout, stderr, False, RuntimeError("fixture process wait returned without an exit status"))
    return stdout, stderr, True, None


def output_tail(value):
    if isinstance(value, bytes):
        value = value.decode("utf-8", errors="replace")
    return (value or "")[-2000:]


def start_legacy_inspection(delegate, arguments, environment):
    """Start the historical finite models control command; caller owns reaping."""
    return subprocess.Popen(
        [str(delegate), *arguments],
        env=phase_environment(environment, "inspection-worker"),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
