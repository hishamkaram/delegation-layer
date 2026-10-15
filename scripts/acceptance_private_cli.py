"""Exercise private supervisor reuse across two installed CLI layouts."""

import argparse
from concurrent.futures import ThreadPoolExecutor
import errno
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import resource
import shlex
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
import threading
import time
import uuid

from acceptance_provider_common import (
    AcceptanceFailure,
    ordinary_lifetime_owner_digest,
    validate_managed_lifetime_closure,
)
from acceptance_supervisor_common import (
    _close_parent_descriptor,
    _open_parent_directory,
    DirectoryOwner,
    OwnedDirectory,
    remove_exact_owned_fifo,
    remove_owned_directory,
    require,
    rollback_owned_directories,
    wait_until,
)


# Leave time for the two-minute task budget and supervisor stop observation.
TASK_BUDGET = "2m"
COLLECT_WATCH = "150s"
COLLECT_TIMEOUT_SECONDS = 165
# Capacity fixtures remain gated while the entire cohort is admitted and
# observed through the upgraded installation. That orchestration can exceed
# ten minutes on a loaded host; it is not the task-timeout acceptance case.
CAPACITY_TASK_BUDGET = "30m"
CAPACITY_TARGET_TASKS = 200
DEFAULT_STOP_SCENARIO = "default-stop"
DEFAULT_STOP_TASK_COUNT = 2
DEFAULT_STOP_TASK_BUDGET = "5s"
DEFAULT_STOP_BUDGET = "900ms"
DEFAULT_DISPATCH_WORKERS = 16
PRIVATE_ACCEPTANCE_PREFIX = ".dl-"
LEASE_HANDSHAKE_TIMEOUT_SECONDS = 5
STARTUP_CONTROL_COMMANDS = frozenset({
    "clean", "kill", "pause", "parallel", "remove", "reset", "restart",
    "shutdown", "start", "unpause",
})
PROTECTED_CALLER_PHASES = frozenset({
    "inspection-worker", "inspection-observation", "managed-runner",
    "managed-runner-observation", "managed-runner-collection",
})


PI_FIXTURE = r'''#!/bin/sh
set -eu

gate_path=__GATE_PATH_JSON__
waiter_directory=__WAITER_DIRECTORY_JSON__
lease_nonce=__LEASE_NONCE_JSON__
if [ "${1-}" = "--list-models" ]; then
  printf '%s\n' "provider model context max-out thinking images" "openai gpt-4.1 128000 8192 enabled enabled"
  exit 0
fi
if [ "${1-}" = "--version" ]; then
  printf '%s\n' "pi fixture 1.0.0"
  exit 0
fi
help_only=1
for argument in "$@"; do
  if [ "$argument" = "--help" ]; then
    printf '%s\n' "Options: --mode --tools --model --thinking --session --session-dir --no-extensions --list-models"
    exit 0
  fi
  case "$argument" in
    --no-extensions|--offline) ;;
    *) help_only=0 ;;
  esac
done
if [ "$help_only" = 1 ]; then
  printf '%s\n' "Options: --mode --tools --model --thinking --session --session-dir --no-extensions --list-models"
  exit 0
fi

prompt=$(/bin/cat)
case "$prompt" in
  *DELEGATE_PRIVATE_UPGRADE_WAIT*)
    exec __PYTHON_EXECUTABLE__ -c __PI_WAIT_PROGRAM__ \
      "$gate_path" "$waiter_directory" "$lease_nonce"
    ;;
esac
/bin/cat <<'EOF'
{"type":"session","id":"00000000-0000-4000-8000-000000000001"}
{"type":"agent_start"}
{"type":"turn_start"}
{"type":"message_start","message":{"role":"assistant","content":[]}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"}}
{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"},"toolResults":[]}
{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"}]}
EOF
'''
PI_WAIT_PROGRAM = r"""import fcntl
import os
import signal
import stat
import sys

gate_path, waiter_directory, lease_nonce = sys.argv[1:]
lease_fifo = os.path.join(waiter_directory, lease_nonce + "-" + str(os.getpid()) + ".fifo")
descriptor = -1
gate_descriptor = -1

def stop(signum, _frame):
    raise SystemExit(128 + signum)

for handled in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
    signal.signal(handled, stop)

try:
    os.mkfifo(lease_fifo, 0o600)
    # Linux and Darwin both allow one process to keep a FIFO's read and write
    # ends open. This preserves the live-writer lease while also letting failed
    # orchestration release the fixture before a lease reader has attached.
    flags = os.O_RDWR | os.O_NONBLOCK
    flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(lease_fifo, flags)
    handshake = lease_nonce + "\t" + str(os.getpid()) + "\n"
    os.write(descriptor, handshake.encode())
    gate_flags = os.O_RDONLY
    gate_flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    gate_descriptor = os.open(gate_path, gate_flags)
    gate_identity = os.fstat(gate_descriptor)
    if not stat.S_ISREG(gate_identity.st_mode) or gate_identity.st_uid != os.getuid():
        raise RuntimeError("capacity fixture gate is unsafe")
    fcntl.flock(gate_descriptor, fcntl.LOCK_SH)
    fcntl.flock(gate_descriptor, fcntl.LOCK_UN)
    sys.stdout.write('''{"type":"session","id":"00000000-0000-4000-8000-000000000001"}\n{"type":"agent_start"}\n{"type":"turn_start"}\n{"type":"message_start","message":{"role":"assistant","content":[]}}\n{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"}}\n{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"},"toolResults":[]}\n{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"}]}\n''')
finally:
    if gate_descriptor >= 0:
        os.close(gate_descriptor)
    if descriptor >= 0:
        os.close(descriptor)
    try:
        os.unlink(lease_fifo)
    except FileNotFoundError:
        pass
"""
REPOSITORY = Path(__file__).resolve().parent.parent
CAPTURE_START_LOCK = threading.Lock()


class AcceptanceBlocked(RuntimeError):
    """A required native prerequisite was unavailable or inconclusive."""

    def __init__(self, code):
        super().__init__(code)
        self.code = code


def file_fingerprint(path):
    """Return a compact, path-safe identity for one source or executable."""
    candidate = Path(path)
    try:
        resolved = candidate.resolve(strict=True)
        info = resolved.stat()
        if not resolved.is_file():
            raise OSError("not a regular file")
        return {
            "name": resolved.name,
            "bytes": info.st_size,
            "sha256": hashlib.sha256(resolved.read_bytes()).hexdigest(),
        }
    except OSError:
        return {"name": candidate.name, "status": "unavailable"}


def descriptor_evidence():
    try:
        soft, hard = resource.getrlimit(resource.RLIMIT_NOFILE)
    except (OSError, ValueError):
        soft, hard = None, None

    def bounded_limit(value):
        if value is None:
            return "unknown"
        return "infinity" if value == resource.RLIM_INFINITY else value

    open_count = None
    count_source = None
    for directory in (Path("/proc/self/fd"), Path("/dev/fd")):
        try:
            open_count = len(os.listdir(directory))
            count_source = str(directory)
            break
        except OSError:
            continue
    result = {
        "soft_limit": bounded_limit(soft),
        "hard_limit": bounded_limit(hard),
        "open_count": open_count,
    }
    if count_source is not None:
        result["count_source"] = count_source
    return result


class AcceptanceReceipt:
    """Own the compact allowlisted receipt for one harness invocation."""

    def __init__(self, output, requested_tasks, dispatch_workers, binaries):
        self.output = Path(output)
        self.started = time.monotonic()
        self.phase_started = self.started
        self.current_phase = "prerequisites"
        self.safe_stage = "invocation-validated"
        self.phase_seconds = {}
        self.counts = {
            "requested_task_count": requested_tasks,
            "dispatch_worker_count": dispatch_workers,
            "dispatch_completed_count": 0,
            "admitted_task_count": 0,
            "running_task_count": 0,
            "live_lease_count": 0,
            "collected_task_count": 0,
        }
        self.queue = {}
        self.details = {}
        self.lock = threading.Lock()
        self.fingerprints = {
            "source": {"acceptance_private_cli.py": file_fingerprint(Path(__file__))},
            "binaries": {
                role: file_fingerprint(path) for role, path in sorted(binaries.items())
            },
        }

    def phase(self, name):
        now = time.monotonic()
        with self.lock:
            elapsed = max(0.0, now - self.phase_started)
            self.phase_seconds[self.current_phase] = round(
                self.phase_seconds.get(self.current_phase, 0.0) + elapsed, 6
            )
            self.current_phase = name
            self.phase_started = now

    def stage(self, name):
        with self.lock:
            self.safe_stage = name

    def update_counts(self, **values):
        with self.lock:
            self.counts.update(values)

    def increment(self, name, amount=1):
        with self.lock:
            self.counts[name] = self.counts.get(name, 0) + amount

    def update_queue(self, *, task_count=None, running_count=None, status_bytes=None):
        with self.lock:
            if task_count is not None:
                self.queue["task_count"] = task_count
            if running_count is not None:
                self.queue["running_count"] = running_count
            if status_bytes is not None:
                self.queue["status_bytes"] = status_bytes

    def add_details(self, **values):
        with self.lock:
            self.details.update(values)

    def write(self, result, cleanup, error=None):
        now = time.monotonic()
        with self.lock:
            phase_seconds = dict(self.phase_seconds)
            phase_seconds[self.current_phase] = round(
                phase_seconds.get(self.current_phase, 0.0)
                + max(0.0, now - self.phase_started),
                6,
            )
            payload = {
                "schema_version": 2,
                "gate": "private-cli-acceptance",
                "result": result,
                "status": {"PASS": "passed", "FAIL": "failed", "BLOCKED": "blocked"}[result],
                "phase": self.current_phase,
                "safe_stage": self.safe_stage,
                "timings_seconds": {
                    "elapsed": round(max(0.0, now - self.started), 6),
                    "phases": phase_seconds,
                },
                "platform": {
                    "system": platform.system(),
                    "kernel": platform.release(),
                    "architecture": platform.machine(),
                },
                "file_descriptors": descriptor_evidence(),
                "counts": dict(self.counts),
                "fingerprints": self.fingerprints,
                "cleanup": cleanup,
            }
            if self.queue:
                payload["queue"] = dict(self.queue)
            payload.update(self.details)
        if error is not None:
            failure_phase = self.details.get("failure_phase", self.current_phase)
            code = error.code if isinstance(error, AcceptanceBlocked) else (
                "cleanup_failed" if cleanup.get("status") == "failed" else
                failure_phase.replace("_", "-") + "-failed"
            )
            payload["failure"] = {
                "code": code,
                "exception_type": type(error).__name__,
                "phase": failure_phase,
            }
        self.output.write_text(json.dumps(payload, sort_keys=True, separators=(",", ":")) + "\n")


def lease_fifo_path(waiter_directory, nonce, pid):
    if len(nonce) != 32 or any(char not in "0123456789abcdef" for char in nonce):
        raise RuntimeError("capacity lease nonce is invalid")
    if not isinstance(pid, int) or pid <= 0:
        raise RuntimeError("capacity lease PID is invalid")
    return Path(waiter_directory) / (nonce + "-" + str(pid) + ".fifo")


class LockedReleaseGate:
    """One owner lock that releases every blocked capacity fixture together."""

    def __init__(self, path, descriptor):
        self.path = Path(path)
        self.descriptor = descriptor

    @classmethod
    def acquire(cls, path):
        path = Path(path)
        flags = os.O_RDWR | os.O_CREAT | os.O_EXCL
        flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
        descriptor = os.open(path, flags, 0o600)
        try:
            identity = os.fstat(descriptor)
            if not stat.S_ISREG(identity.st_mode) or identity.st_uid != os.getuid():
                raise RuntimeError("capacity fixture gate is unsafe")
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return cls(path, descriptor)
        except BaseException:
            os.close(descriptor)
            raise

    def release(self):
        if self.descriptor < 0:
            return
        descriptor = self.descriptor
        self.descriptor = -1
        try:
            fcntl.flock(descriptor, fcntl.LOCK_UN)
        finally:
            os.close(descriptor)


def _validate_live_parent_pin(path, parent_descriptor, parent_identity, task_id):
    """Validate the path and descriptor still identify the original parent."""
    require(type(parent_descriptor) is int and parent_descriptor >= 0,
            "capacity fixture lease parent descriptor is closed: " + task_id)
    try:
        held_parent = os.fstat(parent_descriptor)
    except OSError as error:
        raise RuntimeError(
            "capacity fixture lease parent descriptor is closed: " + task_id
        ) from error
    require(stat.S_ISDIR(held_parent.st_mode)
            and (held_parent.st_dev, held_parent.st_ino) == parent_identity,
            "capacity fixture lease parent descriptor was replaced: " + task_id)
    try:
        current_parent = Path(path).parent.lstat()
    except OSError as error:
        raise RuntimeError(
            "capacity fixture lease parent was replaced: " + task_id
        ) from error
    require(stat.S_ISDIR(current_parent.st_mode) and not stat.S_ISLNK(current_parent.st_mode)
            and (current_parent.st_dev, current_parent.st_ino) == parent_identity,
            "capacity fixture lease parent was replaced: " + task_id)


class LiveFIFOLease:
    """An identity-pinned FIFO lease with a live entry and parent pin."""

    def __init__(self, task_id, pid, path, descriptor, parent_descriptor,
                 identity, parent_identity, owns_parent_descriptor):
        self.task_id = task_id
        self.pid = pid
        self.path = path
        self.descriptor = descriptor
        self.parent_descriptor = parent_descriptor
        self.identity = identity
        self.parent_identity = parent_identity
        self._owns_parent_descriptor = owns_parent_descriptor

    @classmethod
    def acquire(cls, task_id, pid, path, nonce, timeout=LEASE_HANDSHAKE_TIMEOUT_SECONDS):
        path = Path(path).absolute()
        parent_descriptor, parent_identity = _open_parent_directory(path.parent)
        try:
            return cls._acquire_with_parent(
                task_id, pid, path, nonce, parent_descriptor, parent_identity,
                timeout, owns_parent_descriptor=True,
            )
        except BaseException:
            _close_parent_descriptor(parent_descriptor)
            raise

    @classmethod
    def _acquire_with_parent(cls, task_id, pid, path, nonce, parent_descriptor,
                             parent_identity, timeout, owns_parent_descriptor):
        path = Path(path).absolute()
        descriptor = -1
        try:
            _validate_live_parent_pin(path, parent_descriptor, parent_identity, task_id)
            try:
                before = os.stat(path.name, dir_fd=parent_descriptor, follow_symlinks=False)
            except FileNotFoundError as error:
                raise RuntimeError("capacity fixture lease is missing: " + task_id) from error
            if not stat.S_ISFIFO(before.st_mode) or before.st_uid != os.getuid():
                raise RuntimeError("capacity fixture lease is unsafe: " + task_id)
            flags = os.O_RDONLY | os.O_NONBLOCK
            flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
            try:
                descriptor = os.open(path.name, flags, dir_fd=parent_descriptor)
            except OSError as error:
                if error.errno in (errno.EMFILE, errno.ENFILE):
                    raise AcceptanceBlocked("file-descriptor-capacity-unavailable") from error
                raise
            opened = os.fstat(descriptor)
            if (not stat.S_ISFIFO(opened.st_mode) or opened.st_uid != os.getuid()
                    or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino)):
                raise RuntimeError("capacity fixture lease identity changed: " + task_id)
            expected = (nonce + "\t" + str(pid) + "\n").encode()
            received = b""
            deadline = time.monotonic() + timeout
            while b"\n" not in received:
                try:
                    chunk = os.read(descriptor, 256 - len(received))
                except BlockingIOError:
                    chunk = None
                if chunk:
                    received += chunk
                    if len(received) >= 256:
                        raise RuntimeError("capacity fixture lease handshake is oversized: " + task_id)
                elif time.monotonic() >= deadline:
                    raise RuntimeError("capacity fixture lease has no live writer: " + task_id)
                else:
                    time.sleep(0.01)
            if received != expected:
                raise RuntimeError("capacity fixture lease handshake is invalid: " + task_id)
            lease = cls(
                task_id, pid, path, descriptor, parent_descriptor,
                (opened.st_dev, opened.st_ino), parent_identity, owns_parent_descriptor,
            )
            lease.assert_live()
            return lease
        except BaseException:
            if descriptor >= 0:
                os.close(descriptor)
            raise

    def assert_live(self):
        if self.descriptor < 0:
            raise RuntimeError("capacity fixture lease was already closed: " + self.task_id)
        _validate_live_parent_pin(
            self.path, self.parent_descriptor, self.parent_identity, self.task_id,
        )
        try:
            unexpected = os.read(self.descriptor, 1)
        except BlockingIOError:
            return
        if unexpected == b"":
            raise RuntimeError("capacity fixture died during lease cohort formation: " + self.task_id)
        raise RuntimeError("capacity fixture lease emitted unexpected data: " + self.task_id)

    def close(self):
        descriptor = self.descriptor
        self.descriptor = -1
        parent_descriptor = self.parent_descriptor
        self.parent_descriptor = -1
        owns_parent_descriptor = self._owns_parent_descriptor
        self._owns_parent_descriptor = False
        try:
            if descriptor >= 0:
                os.close(descriptor)
        finally:
            if owns_parent_descriptor and parent_descriptor >= 0:
                _close_parent_descriptor(parent_descriptor)

    def _remove_closed_fifo(self, parent_descriptor):
        """Remove this worker-owned FIFO while both original pins remain live."""
        require(self.descriptor >= 0,
                "capacity fixture lease entry descriptor is closed: " + self.task_id)
        require(parent_descriptor == self.parent_descriptor,
                "capacity fixture lease parent descriptor is not its pinned parent: " + self.task_id)
        _validate_live_parent_pin(
            self.path, parent_descriptor, self.parent_identity, self.task_id,
        )
        try:
            remove_exact_owned_fifo(
                parent_descriptor, self.descriptor, self.path.name,
                self.identity, self.parent_identity,
            )
        except FileNotFoundError:
            return {"path": str(self.path), "state": "absent"}
        return {
            "path": str(self.path),
            "state": "removed",
            "device": self.identity[0],
            "inode": self.identity[1],
        }

    def remove_closed_fifo(self):
        return self._remove_closed_fifo(self.parent_descriptor)


class LiveFIFOLeaseCohort:
    """Own one parent pin per directory and every live lease descriptor."""

    def __init__(self, leases, parent_pins):
        self.leases = leases
        self.parent_pins = parent_pins

    @classmethod
    def acquire(cls, specifications, nonce, timeout=LEASE_HANDSHAKE_TIMEOUT_SECONDS):
        leases = []
        parent_pins = {}
        try:
            for specification in specifications:
                path = Path(specification["path"]).absolute()
                parent_pin = parent_pins.get(path.parent)
                if parent_pin is None:
                    parent_pin = _open_parent_directory(path.parent)
                    parent_pins[path.parent] = parent_pin
                leases.append(LiveFIFOLease._acquire_with_parent(
                    specification["task_id"], specification["pid"], path, nonce,
                    parent_pin[0], parent_pin[1], timeout,
                    owns_parent_descriptor=False,
                ))
            cohort = cls(leases, parent_pins)
            cohort.assert_live()
            return cohort
        except BaseException as error:
            try:
                cls._close_resources(leases, parent_pins)
            except OSError as cleanup_error:
                raise cleanup_error from error
            raise

    @staticmethod
    def _close_resources(leases, parent_pins):
        first_error = None
        for lease in reversed(leases):
            try:
                lease.close()
            except OSError as error:
                if first_error is None:
                    first_error = error
        for parent_descriptor, _parent_identity in reversed(tuple(parent_pins.values())):
            _close_parent_descriptor(parent_descriptor)
        if first_error is not None:
            raise first_error

    def assert_live(self):
        for lease in self.leases:
            lease.assert_live()

    def close(self):
        leases = tuple(self.leases)
        parent_pins = dict(self.parent_pins)
        self.leases = []
        self.parent_pins = {}
        self._close_resources(leases, parent_pins)

    def remove_closed_fifos(self):
        """Remove exact worker-owned lease paths while their original pins remain live."""
        return [lease._remove_closed_fifo(lease.parent_descriptor)
                for lease in tuple(self.leases)]

    def __enter__(self):
        return self

    def __exit__(self, _error_type, _error, _traceback):
        self.close()


def phase_environment(environment, phase):
    value = dict(environment)
    value["DELEGATE_ACCEPTANCE_PHASE"] = phase
    return value


def _assert_capture_root(capture_root):
    require(isinstance(capture_root, OwnedDirectory),
            "private control capture root identity token is required")
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
    require(process.returncode is not None,
            "private control process returned without an exit status")
    return process.returncode


def _reap_private_control_process(process, primary_error, stdout_path, stderr_path):
    try:
        if process.poll() is None:
            # This process is the directly spawned private control CLI. It is
            # never a managed worker, provider, daemon, or native task scope.
            process.kill()
        _wait_private_control_process(
            process, 5, "private control command cleanup",
        )
    except BaseException as cleanup_error:
        raise RuntimeError(
            "private control command cleanup failed after %s: %s; retained capture files: %s, %s"
            % (primary_error, cleanup_error, stdout_path, stderr_path)
        ) from primary_error


def _retain_capture_failure(error, stdout_path, stderr_path):
    error.retained_capture_paths = (str(stdout_path), str(stderr_path))


def run_captured_command(command, env, timeout, *, capture_root):
    """Run one private control CLI with capture owned by the acceptance root."""
    _assert_capture_root(capture_root)
    capture_id = uuid.uuid4().hex
    stdout_name = "capture-" + capture_id + ".stdout"
    stderr_name = "capture-" + capture_id + ".stderr"
    stdout_path = _capture_path(capture_root, stdout_name)
    stderr_path = _capture_path(capture_root, stderr_name)
    process = None
    try:
        with CAPTURE_START_LOCK:
            _assert_capture_root(capture_root)
            with _open_capture_writer(capture_root, stdout_name) as output, \
                    _open_capture_writer(capture_root, stderr_name) as errors:
                process = subprocess.Popen(command, env=env, stdout=output, stderr=errors)
        code = _wait_private_control_process(
            process, timeout, "private control command",
        )
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
        raise subprocess.TimeoutExpired(
            command, timeout, output=captured_stdout, stderr=captured_stderr,
        ) from None
    except BaseException as primary_error:
        if process is not None:
            _reap_private_control_process(process, primary_error, stdout_path, stderr_path)
        raise


def run_captured_text_command(command, env, timeout, *, capture_root):
    """Run one control command through the shared file-backed capture owner."""
    captured = run_captured_command(command, env, timeout, capture_root=capture_root)
    return subprocess.CompletedProcess(
        captured.args,
        captured.returncode,
        captured.stdout.decode("utf-8"),
        captured.stderr.decode("utf-8"),
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
            return stdout, stderr, False, RuntimeError(
                "fixture process could not be reaped after timeout: %s; stdout=%r stderr=%r"
                % (cleanup_error, output_tail(stdout), output_tail(stderr))
            )
        return stdout, stderr, True, RuntimeError(
            "fixture process exceeded its %ss observation bound; stdout=%r stderr=%r"
            % (timeout, output_tail(stdout), output_tail(stderr))
        )
    except OSError as error:
        return "", "", False, RuntimeError("fixture process wait failed: " + str(error))
    if process.returncode is None:
        return stdout, stderr, False, RuntimeError("fixture process wait returned without an exit status")
    return stdout, stderr, True, None


def output_tail(value):
    if isinstance(value, bytes):
        value = value.decode("utf-8", errors="replace")
    return (value or "")[-2000:]


def record_invocation_error(receipt, args, error, captured):
    if receipt is None:
        return
    stdout = captured.stdout if captured is not None else getattr(error, "output", None) or b""
    stderr = captured.stderr if captured is not None else getattr(error, "stderr", None) or b""
    evidence = {
        "arguments": args, "invocation_error": str(error),
        "failure_class": "invocation_error",
        "exit_code": captured.returncode if captured is not None else None,
        "stdout_bytes": len(stdout), "stdout_sha256": hashlib.sha256(stdout).hexdigest(),
        "stdout_tail": output_tail(stdout),
        "stderr_bytes": len(stderr), "stderr_sha256": hashlib.sha256(stderr).hexdigest(),
        "stderr_tail": output_tail(stderr),
    }
    retained_paths = getattr(error, "retained_capture_paths", ())
    if retained_paths:
        evidence["capture_failure"] = {
            "retained_paths": list(retained_paths),
            "cause": (None if error.__cause__ is None else {
                "type": type(error.__cause__).__name__,
                "message": str(error.__cause__),
            }),
        }
    receipt.write_text(json.dumps(evidence, indent=2) + "\n")


def run_json(delegate, env, args, expected=(0,), timeout=45, receipt=None, phase=None,
             *, capture_root, capture_response=False):
    captured = None
    try:
        command_env = phase_environment(env, phase) if phase is not None else env
        captured = run_captured_command(
            [str(delegate), *args], command_env, timeout, capture_root=capture_root,
        )
        result = subprocess.CompletedProcess(
            captured.args, captured.returncode,
            captured.stdout.decode("utf-8"), captured.stderr.decode("utf-8"),
        )
    except (OSError, subprocess.TimeoutExpired, UnicodeDecodeError) as error:
        record_invocation_error(receipt, args, error, captured)
        raise
    if receipt is not None:
        evidence = {
            "arguments": args, "exit_code": result.returncode,
            "stdout_bytes": len(captured.stdout),
            "stdout_sha256": hashlib.sha256(captured.stdout).hexdigest(),
            "stderr_bytes": len(captured.stderr),
            "stderr_sha256": hashlib.sha256(captured.stderr).hexdigest(),
        }
        if result.returncode != 0:
            evidence.update({
                "failure_class": "nonzero_exit",
                "stdout_tail": output_tail(captured.stdout),
                "stderr_tail": output_tail(captured.stderr),
            })
        receipt.write_text(json.dumps(evidence, indent=2) + "\n")
    if result.returncode not in expected:
        raise RuntimeError(
            "delegate command failed: %r exit=%d stdout=%r stderr=%r"
            % (args, result.returncode, result.stdout[-2000:], result.stderr[-2000:])
        )
    try:
        response = json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise RuntimeError("delegate returned invalid JSON for %r: %s" % (args, error)) from error
    if receipt is not None and capture_response:
        evidence["response"] = response
        receipt.write_text(json.dumps(evidence, indent=2) + "\n")
    return response


def install_fixture(source, destination):
    if not source.is_file() or not os.access(source, os.X_OK):
        raise RuntimeError("required executable is missing: " + str(source))
    shutil.copyfile(source, destination)
    destination.chmod(0o700)


def observe_created_admission(delegate, state, env, response, receipt, timeout=60,
                              phase="admission-observation", *, capture_root):
    """Confirm one created task after uncertain or failed dispatch; never retry it."""
    failure = response.get("failure", {})
    uncertain = (failure.get("code") == "task_submission_uncertain"
                 and failure.get("next_action") == "check_status")
    created = response.get("task_record") == "created"
    admitted = response.get("admission") == "admitted"
    operational_failure = bool(failure or response.get("error"))
    needs_observation = uncertain or (created and admitted and operational_failure)
    if not created or (operational_failure and not needs_observation) or (
            not admitted and not uncertain):
        raise RuntimeError("upgrade probe was not durably created: %r" % response)
    if not needs_observation:
        return response
    task_id = response["task_id"]
    deadline = time.monotonic() + timeout
    attempt = 0
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("created fixture task admission remained uncertain: " + task_id)
        observed = run_json(delegate, env, ["--root", str(state), "status", task_id, "--json"],
                            timeout=min(45, remaining),
                            receipt=receipt.with_name("observation-" + receipt.stem + "-%03d.json" % attempt),
                            phase=phase, capture_root=capture_root)
        if observed.get("task_id") != task_id:
            raise RuntimeError("admission observation returned a different task")
        # status uses exit zero for a valid uncertain observation, including
        # its diagnostic error text. Nonzero operational failures were rejected
        # by run_json; the admission field decides whether observation is done.
        if observed.get("admission") == "admitted":
            return observed
        if observed.get("admission") != "unknown":
            raise RuntimeError("created fixture task admission could not be confirmed: %r" % observed)
        attempt += 1
        time.sleep(min(0.2, max(0, deadline - time.monotonic())))


def read_created_task_status(delegate, state, env, task_id, receipt,
                             phase="task-observation", timeout=45, *, capture_root):
    """Read one created task without changing or resubmitting its request."""
    observed = run_json(
        delegate, env, ["--root", str(state), "status", task_id, "--json"],
        timeout=timeout, receipt=receipt, phase=phase, capture_root=capture_root,
        capture_response=True,
    )
    if observed.get("task_id") != task_id:
        raise RuntimeError("task observation returned a different task")
    return observed


def await_created_task_ended(delegate, state, env, response, receipt,
                             timeout=30, phase="task-observation", *, capture_root):
    """Poll one created task until the queue reports ended, without resubmitting it."""
    task_id = response.get("task_id")
    root_id = response.get("root_id")
    if not isinstance(task_id, str) or not isinstance(root_id, str) or not root_id:
        raise RuntimeError("task observation lacks its created task/root identity")
    deadline = time.monotonic() + timeout
    attempt = 0
    poll_receipts = []
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("created task queue status remained non-ended: " + task_id)
        poll_receipt = receipt.with_name(
            "%s-%03d%s" % (receipt.stem, attempt, receipt.suffix)
        )
        status = read_created_task_status(
            delegate, state, env, task_id, poll_receipt,
            phase=phase, timeout=min(45, remaining), capture_root=capture_root,
        )
        poll_receipts.append(poll_receipt.name)
        if status.get("root_id") != root_id:
            raise RuntimeError("task observation returned a different root")
        failure = status.get("failure")
        if status.get("error") or failure:
            raise RuntimeError("task status reported an operational failure: %r" % status)
        liveness = status.get("liveness")
        if liveness == "ended":
            return status, {
                "kind": "queue-ended-observation",
                "final_receipt": poll_receipt.name,
                "poll_receipts": poll_receipts,
                "poll_count": len(poll_receipts),
            }
        if liveness != "running":
            raise RuntimeError("task status did not report running or ended: %r" % status)
        attempt += 1
        time.sleep(min(0.2, max(0, deadline - time.monotonic())))


def package_layout(root, symlinked):
    if not symlinked:
        install = root / "install"
        return install, install, root / "libexec"
    cellar = root / "Cellar" / "delegation-layer" / "0.1.0"
    return root / "bin", cellar / "bin", cellar / "libexec"


def install_package_layout(owner, delegate, runner, root, symlinked):
    if not symlinked:
        install = owner.create("package install", root / "install", root).directory
        canonical_bin = install
        libexec = owner.create("package libexec", root / "libexec", root).directory
    else:
        cellar = owner.create("package Cellar", root / "Cellar", root).directory
        product = owner.create(
            "package product", cellar / "delegation-layer", cellar).directory
        version = owner.create(
            "package version", product / "0.1.0", product).directory
        canonical_bin = owner.create("package bin", version / "bin", version).directory
        libexec = owner.create("package libexec", version / "libexec", version).directory
        install = owner.create("package install links", root / "bin", root).directory
    install_fixture(delegate, canonical_bin / "delegate")
    install_fixture(runner, canonical_bin / "delegate-run")
    if not symlinked:
        return install, install, libexec
    for name in ("delegate", "delegate-run"):
        link = install / name
        link.symlink_to(os.path.relpath(canonical_bin / name, install))
    return install, canonical_bin, libexec


def install_adjacent_package(owner, sources, root):
    """Copy the four release binaries into one archive-style install directory."""
    install = owner.create("packaged default install", root, root.parent).directory
    for name in ("delegate", "delegate-run", "pueue", "pueued"):
        install_fixture(sources[name], install / name)
        copied = install / name
        source_fingerprint = file_fingerprint(sources[name])
        copied_fingerprint = file_fingerprint(copied)
        if (copied.is_symlink()
                or copied_fingerprint.get("bytes") != source_fingerprint.get("bytes")
                or copied_fingerprint.get("sha256") != source_fingerprint.get("sha256")):
            raise RuntimeError("packaged binary copy changed identity: " + name)
    return install


def packaged_default_commands(brief, workspace, task_id):
    """Construct the exact public commands used without any global path override."""
    return {
        "dispatch": [
            "dispatch", "--provider", "pi:json", "--brief", str(brief),
            "--cwd", str(workspace), "--id", task_id, "--permission", "read-only",
            "--budget", TASK_BUDGET, "--json",
        ],
        "status": ["status", task_id, "--json"],
        "collect": ["collect", task_id, "--watch", COLLECT_WATCH, "--json"],
    }


def isolated_default_state_path(home):
    if platform.system() == "Darwin":
        return Path(home) / "Library" / "Application Support" / "delegation-layer"
    if platform.system() == "Linux":
        return Path(home) / ".config" / "delegation-layer"
    raise AcceptanceBlocked("unsupported-native-platform")


def verify_packaged_default_path(owner, base, sources, provider_bin, brief,
                                 primary_client, primary_config, primary_env,
                                 response_directory, supervisor_cleanups,
                                 run_receipt=None, managed_state_roots=None, *, capture_root):
    """Prove an adjacent release bundle bootstraps through only public defaults."""
    if run_receipt is not None:
        run_receipt.phase("packaged_default_setup")
    package = install_adjacent_package(owner, sources, base / "packaged-default")
    private_home = owner.create("packaged default home", base / "h", base).directory
    workspace = owner.create(
        "packaged default workspace", base / "packaged-workspace", base,
    ).directory
    default_state = isolated_default_state_path(private_home)
    config = default_state / ".supervisor" / "pueue.yml"
    env = {
        "HOME": str(private_home),
        "PATH": str(provider_bin),
        "LANG": "C.UTF-8",
        "LC_ALL": "C.UTF-8",
    }
    if shutil.which("pueue", path=env["PATH"]) is not None:
        raise RuntimeError("packaged default PATH unexpectedly contains pueue")
    if default_state.exists() or config.exists():
        raise RuntimeError("packaged default state was not fresh")
    cleanup = {
        "client": package / "pueue", "config": config, "env": env,
        "state": default_state, "task_ids": [],
    }
    supervisor_cleanups.append(cleanup)
    if managed_state_roots is not None:
        managed_state_roots.append((default_state, cleanup["task_ids"]))
    primary_pid, primary_endpoint, _ = private_shutdown_evidence(primary_config)
    task_id = uuid.uuid4().hex
    cleanup["task_ids"].append(task_id)
    commands = packaged_default_commands(brief, workspace, task_id)
    if run_receipt is not None:
        run_receipt.phase("packaged_default_dispatch")
    dispatched = run_json(
        package / "delegate", env, commands["dispatch"],
        receipt=Path(response_directory) / "packaged-dispatch.json",
        phase="packaged-default-dispatch", capture_root=capture_root,
    )
    if (dispatched.get("task_id") != task_id
            or dispatched.get("task_record") != "created"
            or dispatched.get("admission") != "admitted"):
        raise RuntimeError("packaged default dispatch was not durably admitted")
    if not config.is_file():
        raise RuntimeError("packaged default dispatch did not create its private config")
    pair = assert_state_supervisor_pair(default_state, package)
    packaged_pid, packaged_endpoint, _ = private_shutdown_evidence(config)
    if packaged_pid == primary_pid or packaged_endpoint == primary_endpoint:
        raise RuntimeError("packaged default root did not own a distinct private daemon")
    primary_metrics = {}
    pueue_snapshot(primary_client, primary_config, primary_env, primary_metrics,
                   phase="packaged-coexistence-observation", capture_root=capture_root)
    packaged_metrics = {}
    packaged_snapshot = pueue_snapshot(
        pair / "pueue", config, env, packaged_metrics,
        phase="packaged-coexistence-observation", capture_root=capture_root,
    )
    if run_receipt is not None:
        packaged_rows = packaged_snapshot.get("tasks", {})
        run_receipt.update_queue(
            task_count=len(packaged_rows),
            running_count=sum(
                1 for row in packaged_rows.values() if pueue_state(row) == "Running"
            ),
            status_bytes=packaged_metrics.get("status_bytes"),
        )
    if run_receipt is not None:
        run_receipt.phase("packaged_default_observation")
    observed = run_json(
        package / "delegate", env, commands["status"],
        receipt=Path(response_directory) / "packaged-status.json",
        phase="packaged-default-observation", capture_root=capture_root,
    )
    if observed.get("task_id") != task_id:
        raise RuntimeError("packaged default status observed a different task")
    collected = run_json(
        package / "delegate", env, commands["collect"],
        timeout=COLLECT_TIMEOUT_SECONDS,
        receipt=Path(response_directory) / "packaged-collect.json",
        phase="packaged-default-collection", capture_root=capture_root,
    )
    if collected.get("outcome", {}).get("verdict") != "committed":
        raise RuntimeError("packaged default collect did not publish success")
    meta = json.loads((default_state / "tasks" / task_id / "meta.json").read_text())
    runner_digest = file_fingerprint(sources["delegate-run"])["sha256"]
    expected_runner = str(default_state / ".supervisor" / ("delegate-run-" + runner_digest))
    if meta.get("runner_executable") != expected_runner:
        raise RuntimeError("packaged default dispatch did not resolve its adjacent runner")
    if run_receipt is not None:
        run_receipt.increment("collected_task_count")
        run_receipt.phase("packaged_default_shutdown")
    shutdown_private_supervisor(
        pair / "pueue", config, env, state=default_state, task_ids=(task_id,),
        capture_root=capture_root,
    )
    supervisor_cleanups.remove(cleanup)
    pueue_snapshot(primary_client, primary_config, primary_env, primary_metrics,
                   phase="packaged-coexistence-observation", capture_root=capture_root)
    if run_receipt is not None:
        run_receipt.stage("packaged-default-path-proved")
    return {
        "layout": "adjacent-release-binaries",
        "binary_fingerprints": {
            name: file_fingerprint(package / name)
            for name in ("delegate", "delegate-run", "pueue", "pueued")
        },
        "global_path_overrides": [],
        "ambient_pueue_on_path": False,
        "fresh_bootstrap": True,
        "dispatch_status_collect": True,
        "clean_shutdown": True,
        "coexisted_with_test_owned_daemon": True,
        "proved": [
            "copied adjacent release binaries retained source identities",
            "isolated HOME resolved the default state root",
            "dispatch status and collect used no root runner or supervisor override",
            "fresh bootstrap coexisted with a distinct test-owned private daemon",
            "the packaged default daemon and endpoint stopped cleanly",
        ],
        "not_proved_here": [
            "package removal and stopped-daemon restart are proved by the current-installation scenario",
            "legacy supervisor migration is outside this default-path scenario",
        ],
    }


def install_wrapper(destination, executable, marker, identity, role):
    arguments = marker.with_name(marker.name + "-arguments")
    calls = marker.with_name(marker.name + "-calls")
    if not destination.parent.is_dir() or destination.parent.is_symlink():
        raise RuntimeError("package wrapper parent is not an owned directory: " +
                           str(destination.parent))
    destination.write_text(
        "#!/bin/sh\n"
        "set -eu\n"
        "printf '%s\\n' started >> " + shlex.quote(str(marker)) + "\n"
        "printf '%s\\n' \"$*\" >> " + shlex.quote(str(arguments)) + "\n"
        "printf '%s\\t%s\\t%s\\n' \"${DELEGATE_ACCEPTANCE_PHASE:-unattributed}\" "
        + shlex.quote(role) + " \"$*\" >> " + shlex.quote(str(calls)) + "\n"
        "exec " + shlex.quote(str(executable)) + " \"$@\"\n"
        "# " + identity + "\n"
    )
    destination.chmod(0o700)


def wrapper_call_records(path, role, installation):
    path = Path(path)
    if not path.is_file() or path.is_symlink():
        return []
    records = []
    for line in path.read_text().splitlines():
        fields = line.split("\t", 2)
        if len(fields) != 3:
            raise RuntimeError("private supervisor wrapper call receipt is malformed: " + str(path))
        phase, logged_role, arguments = fields
        if logged_role != role or not phase:
            raise RuntimeError("private supervisor wrapper call identity is invalid: " + str(path))
        tokens = arguments.split()
        command = "startup" if role == "daemon" else (
            tokens[2] if len(tokens) > 2 and tokens[0] in ("-c", "--config") else
            tokens[0] if tokens else "empty")
        raw = arguments.encode()
        records.append({
            "installation": installation,
            "phase": phase,
            "role": role,
            "command": command,
            "arguments_bytes": len(raw),
            "arguments_sha256": hashlib.sha256(raw).hexdigest(),
        })
    return records


def protected_wrapper_calls(records):
    return [record for record in records
            if record["phase"] in PROTECTED_CALLER_PHASES and
            (record["role"] == "daemon" or record["command"] in STARTUP_CONTROL_COMMANDS)]


def require_no_startup_control_calls(records):
    forbidden = protected_wrapper_calls(records)
    if forbidden:
        raise RuntimeError("managed-runner or inspection startup control command observed: "
                           + json.dumps(forbidden, sort_keys=True))


def compact_sequence_evidence(values):
    encoded = json.dumps(values, sort_keys=True, separators=(",", ":")).encode()
    return {"count": len(values), "sha256": hashlib.sha256(encoded).hexdigest()}


def wrapper_command_counts(path):
    counts = {}
    for record in wrapper_call_records(path, "client", "unknown"):
        command = record["command"]
        counts[command] = counts.get(command, 0) + 1
    return dict(sorted(counts.items()))


def install_pi_fixture(install_dir, gate_file, waiter_directory, lease_nonce):
    pi = install_dir / "pi"
    script = PI_FIXTURE.replace("__GATE_PATH_JSON__", shlex.quote(str(gate_file)))
    script = script.replace("__WAITER_DIRECTORY_JSON__", shlex.quote(str(waiter_directory)))
    script = script.replace("__LEASE_NONCE_JSON__", shlex.quote(lease_nonce))
    script = script.replace("__PYTHON_EXECUTABLE__", shlex.quote(sys.executable))
    script = script.replace("__PI_WAIT_PROGRAM__", shlex.quote(PI_WAIT_PROGRAM))
    pi.write_text(script)
    pi.chmod(0o700)


def pueue_snapshot(client, config, env, metrics=None, phase=None, *, capture_root):
    command_env = phase_environment(env, phase) if phase is not None else env
    result = run_captured_text_command(
        [str(client), "--config", str(config), "status", "--json"],
        command_env, 10,
        capture_root=capture_root,
    )
    if result.returncode != 0:
        raise RuntimeError("isolated pueue status failed: " + result.stderr[-1000:])
    if metrics is not None:
        metrics["status_bytes"] = len(result.stdout.encode("utf-8"))
    return json.loads(result.stdout)


def wait_for_pueue_state(client, config, env, label, expected,
                         phase="supervisor-observation", *, capture_root):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        snapshot = pueue_snapshot(client, config, env, phase=phase, capture_root=capture_root)
        for row in snapshot.get("tasks", {}).values():
            if row.get("label") == label:
                status = row.get("status", {})
                state = next(iter(status), "unknown")
                if state == expected:
                    return row
                if state == "Done":
                    raise RuntimeError("fixture task ended before reaching " + expected)
        time.sleep(0.05)
    raise RuntimeError("timed out waiting for isolated pueue task state " + expected)


def wait_for_queued_label_prefix(client, config, env, group, prefix, excluded_labels, process,
                                phase="inspection-observation", *, capture_root):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(
                "models command exited before its inspection worker queued: exit=%d"
                % process.returncode
            )
        snapshot = pueue_snapshot(client, config, env, phase=phase, capture_root=capture_root)
        for row in snapshot.get("tasks", {}).values():
            label = row.get("label") or ""
            if label not in excluded_labels and label.startswith(prefix) and row.get("group") == group:
                state = pueue_state(row)
                if state == "Queued":
                    return row
                if state == "Done":
                    raise RuntimeError("inspection worker completed despite its queue blocker")
        time.sleep(0.05)
    raise RuntimeError("timed out waiting for queued inspection worker")


def pueue_command(client, config, env, args, timeout=15, phase=None, *, capture_root):
    command_env = phase_environment(env, phase) if phase is not None else env
    result = run_captured_text_command(
        [str(client), "--config", str(config), *args],
        command_env, timeout,
        capture_root=capture_root,
    )
    if result.returncode != 0:
        raise RuntimeError("isolated pueue command failed: %r: %s" % (args, result.stderr[-1000:]))
    return result.stdout


def shutdown_remaining(deadline):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError("private supervisor shutdown observation expired")
    return remaining


def private_daemon_process_gone(pid, deadline, *, capture_root):
    # No signal is sent. A zombie or a reused live PID remains unavailable as
    # exit proof; timeout is safer than treating either as a stopped daemon.
    try:
        os.getpgid(pid)
    except ProcessLookupError:
        # Darwin getpgid excludes zombies. Check PID presence without using
        # command names, which can change while a process becomes a zombie.
        result = run_captured_text_command(
            ["/bin/ps", "-p", str(pid), "-o", "pid="],
            {"PATH": "/usr/bin:/bin", "LC_ALL": "C"},
            shutdown_remaining(deadline),
            capture_root=capture_root,
        )
        if result.returncode == 1 and not result.stdout.strip() and not result.stderr.strip():
            return True
        if result.returncode == 0 and result.stdout.strip() == str(pid):
            return False
        raise RuntimeError("private supervisor PID presence is uncertain")
    return False


def private_shutdown_evidence(config):
    config = Path(config)
    if (config.name != "pueue.yml" or config.parent.name != ".supervisor"
            or config.resolve(strict=True) != config):
        raise RuntimeError("unsafe private supervisor shutdown config")
    runtime = config.parent / "run"
    if runtime.resolve(strict=True) != runtime:
        raise RuntimeError("unsafe private supervisor runtime directory")
    # These are the exact quoted fields written by privateConfigYAML, not a
    # general YAML parser or a fallback to the user's ambient Pueue config.
    lines = config.read_text(encoding="utf-8").splitlines()
    for key, path in (("runtime_directory", runtime),
                      ("pid_path", runtime / "pueue.pid")):
        prefix = "  " + key + ": "
        values = [line[len(prefix):] for line in lines if line.startswith(prefix)]
        if len(values) != 1 or json.loads(values[0]) != str(path):
            raise RuntimeError("private shutdown config does not bind its owned " + key)
    prefix = "  unix_socket_path: "
    values = [line[len(prefix):] for line in lines if line.startswith(prefix)]
    if len(values) != 1:
        raise RuntimeError("private shutdown config does not bind its owned unix_socket_path")
    endpoint_value = json.loads(values[0])
    if (not isinstance(endpoint_value, str) or "\0" in endpoint_value
            or not Path(endpoint_value).is_absolute()):
        raise RuntimeError("private supervisor socket path is invalid")
    endpoint = Path(endpoint_value)
    if Path(os.path.normpath(endpoint_value)) != endpoint:
        raise RuntimeError("private supervisor socket path is not canonical")
    try:
        endpoint.relative_to(config.parent)
    except ValueError as error:
        raise RuntimeError("private supervisor socket escapes its owned directory") from error
    if endpoint == config.parent or endpoint.parent.resolve(strict=True) != endpoint.parent:
        raise RuntimeError("private supervisor socket parent is unsafe")
    pid_path = runtime / "pueue.pid"
    with os.fdopen(os.open(pid_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid():
            raise RuntimeError("unsafe private supervisor PID file")
        raw = stream.read(129)
    value = raw.strip()
    if (len(raw) > 128 or not value.isdigit() or value != str(int(value)).encode()
            or int(value) <= 1 or int(value) == os.getpid()):
        raise RuntimeError("invalid private supervisor PID")
    info = endpoint.lstat()
    if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
        raise RuntimeError("unsafe private supervisor socket")
    return int(value), endpoint, (info.st_dev, info.st_ino)


def private_shutdown_endpoint_closed(endpoint, identity, deadline):
    try:
        info = endpoint.lstat()
    except FileNotFoundError:
        return True
    if not stat.S_ISSOCK(info.st_mode) or (info.st_dev, info.st_ino) != identity:
        raise RuntimeError("private supervisor socket identity changed during shutdown")
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(min(0.25, shutdown_remaining(deadline)))
        try:
            connection.connect(str(endpoint))
        except TimeoutError:
            return False
        except OSError as error:
            if error.errno in (errno.ENOENT, errno.ECONNREFUSED):
                return True
            if error.errno in (errno.EAGAIN, errno.EWOULDBLOCK):
                return False
            raise
    return False


def authorize_managed_lifetime_closure(state, task_ids=()):
    """Authorize private supervisor cleanup from the root-level validator."""
    if state is None:
        raise RuntimeError("managed lifetime state root is required before supervisor shutdown")
    return validate_managed_lifetime_closure(Path(state), task_ids)


def await_managed_lifetime_closure(state, task_ids=(), timeout=30):
    """Wait for naturally exiting managed workers before supervisor shutdown."""
    deadline = time.monotonic() + timeout
    last_error = None
    while True:
        try:
            return authorize_managed_lifetime_closure(state, task_ids)
        except AcceptanceFailure as error:
            last_error = error
        if time.monotonic() >= deadline:
            raise RuntimeError(
                "managed lifetime closure remained incomplete before supervisor shutdown"
            ) from last_error
        time.sleep(min(0.05, max(0.0, deadline - time.monotonic())))


def shutdown_private_supervisor(client, config, env, timeout=30, *, state, task_ids=(), capture_root):
    """Authorize lifetime closure, then observe PID and endpoint disappearance."""
    authorize_managed_lifetime_closure(state, task_ids)
    deadline = time.monotonic() + timeout
    pid, endpoint, socket_identity = private_shutdown_evidence(config)
    if private_daemon_process_gone(pid, deadline, capture_root=capture_root):
        raise RuntimeError("private supervisor PID disappeared before shutdown")
    pueue_command(client, config, env, ["shutdown"], timeout=shutdown_remaining(deadline),
                  phase="supervisor-shutdown", capture_root=capture_root)
    disappeared = False
    while True:
        gone = private_daemon_process_gone(pid, deadline, capture_root=capture_root)
        if disappeared and not gone:
            raise RuntimeError("private supervisor PID identity changed during shutdown")
        disappeared = gone
        closed = private_shutdown_endpoint_closed(endpoint, socket_identity, deadline)
        remaining = shutdown_remaining(deadline)
        if disappeared and closed:
            return
        time.sleep(min(0.05, remaining))


def task_rows_by_label(snapshot, expected_labels):
    rows_by_label = {}
    duplicate_labels = set()
    for row in snapshot.get("tasks", {}).values():
        label = row.get("label")
        if not isinstance(label, str) or label not in expected_labels:
            continue
        if label in rows_by_label:
            duplicate_labels.add(label)
        else:
            rows_by_label[label] = row
    if duplicate_labels:
        label = sorted(duplicate_labels)[0]
        raise RuntimeError("queue snapshot contains duplicate task label: " + label)
    return rows_by_label


def task_row_by_label(snapshot, label):
    return task_rows_by_label(snapshot, {label}).get(label)


def assert_state_supervisor_pair(state, bundled_directory):
    supervisor = state / ".supervisor"
    manifest_path = supervisor / "pueue-pair.json"
    if not manifest_path.is_file() or manifest_path.is_symlink():
        raise RuntimeError("state-root supervisor pair manifest is missing or unsafe")
    try:
        manifest = json.loads(manifest_path.read_text())
    except (OSError, json.JSONDecodeError) as error:
        raise RuntimeError("state-root supervisor pair manifest is unreadable: " + str(error)) from error
    client_digest = manifest.get("client_sha256", "")
    daemon_digest = manifest.get("daemon_sha256", "")
    pair_digest = hashlib.sha256((client_digest + "\n" + daemon_digest).encode()).hexdigest()
    if (
        manifest.get("schema_version") != 1
        or manifest.get("pair_sha256") != pair_digest
        or len(client_digest) != 64
        or len(daemon_digest) != 64
    ):
        raise RuntimeError("state-root supervisor pair manifest has an invalid identity")
    pair_directory = supervisor / ("pueue-pair-" + pair_digest)
    for name, expected_digest in (("pueue", client_digest), ("pueued", daemon_digest)):
        installed = pair_directory / name
        bundled = bundled_directory / name
        if (
            installed.is_symlink()
            or not installed.is_file()
            or hashlib.sha256(installed.read_bytes()).hexdigest() != expected_digest
            or hashlib.sha256(bundled.read_bytes()).hexdigest() != expected_digest
        ):
            raise RuntimeError("state-root pair does not match its packaged supervisor member " + name)
    return pair_directory


def pueue_state(row):
    return next(iter(row.get("status", {})), "unknown")


def capacity_lease_specifications(state, responses, waiter_directory, lease_nonce):
    """Return exact fixture leases backed by consistent nonterminal launch evidence."""
    specifications = []
    for response in responses:
        task_id = response["task_id"]
        root_id = response["root_id"]
        lifetime = state / "tasks" / task_id / "lifetime"
        task_path = state / "tasks" / task_id / "task.json"
        meta_path = state / "tasks" / task_id / "meta.json"
        intent_path = lifetime / "intent.json"
        registration_path = lifetime / "registration.json"
        worker_path = lifetime / "worker.json"
        owner_lock_path = lifetime / "owner.lock"
        termination_path = lifetime / "terminated.json"
        required = (
            task_path, meta_path, intent_path, registration_path, worker_path,
            owner_lock_path,
        )
        if not all(path.exists() for path in required):
            continue
        if any(path.is_symlink() or not path.is_file() for path in required):
            raise RuntimeError("task lifetime control record is unsafe: " + task_id)
        launch_paths = sorted(lifetime.glob("*.launch.json"))
        if not launch_paths:
            continue
        if len(launch_paths) > 64 or any(
                path.is_symlink() or not path.is_file() for path in launch_paths):
            raise RuntimeError("task has an invalid lifetime launch set: " + task_id)
        if termination_path.exists():
            raise RuntimeError("gated task terminated before the capacity proof: " + task_id)
        try:
            task_digest = hashlib.sha256(task_path.read_bytes()).hexdigest()
            meta_digest = hashlib.sha256(meta_path.read_bytes()).hexdigest()
            owner_digest = ordinary_lifetime_owner_digest(task_digest, meta_digest)
            intent = json.loads(intent_path.read_text())
            registration_bytes = registration_path.read_bytes()
            registration = json.loads(registration_bytes)
            worker = json.loads(worker_path.read_text())
            launches = [json.loads(path.read_text()) for path in launch_paths]
        except (OSError, json.JSONDecodeError) as error:
            raise RuntimeError("task lifetime evidence is unreadable: " + task_id) from error
        records = (intent, registration, worker, *launches)
        if any(
                record.get("schema_version") != 1
                or record.get("root_id") != root_id
                or record.get("task_id") != task_id
                for record in records):
            raise RuntimeError("task lifetime evidence changed identity: " + task_id)
        registration_digest = hashlib.sha256(registration_bytes).hexdigest()
        if (
            intent.get("owner_kind") != "task"
            or registration.get("owner_kind") != "task"
            or intent.get("owner_sha256") != owner_digest
            or registration.get("owner_sha256") != owner_digest
            or intent.get("scope_kind") != registration.get("scope_kind")
            or intent.get("assurance") != registration.get("assurance")
            or registration.get("scope_kind") not in {"cgroup-v2", "process-group"}
            or registration.get("assurance") not in {"exact", "cooperative"}
            or (registration.get("scope_kind") == "cgroup-v2"
                and registration.get("assurance") != "exact")
            or (registration.get("scope_kind") == "process-group"
                and registration.get("assurance") != "cooperative")
            or not isinstance(intent.get("scope_plan"), str)
            or not intent["scope_plan"]
            or not isinstance(registration.get("scope_id"), str)
            or not registration["scope_id"]
            or intent.get("boot_id") != registration.get("boot_id")
            or worker.get("boot_id") != registration.get("boot_id")
            or worker.get("registration_sha256") != registration_digest
            or not isinstance(worker.get("pid"), int) or worker["pid"] <= 0
            or not worker.get("process_start")
        ):
            raise RuntimeError("task lifetime evidence is inconsistent: " + task_id)
        for sequence, launch in enumerate(launches, start=1):
            if (
                launch.get("registration_sha256") != registration_digest
                or launch.get("sequence") != sequence
                or not isinstance(launch.get("pid"), int) or launch["pid"] <= 0
                or not launch.get("process_start")
            ):
                raise RuntimeError("task lifetime launch evidence is inconsistent: " + task_id)
        pid = launches[-1]["pid"]
        specifications.append({
            "root_id": root_id,
            "task_id": task_id,
            "pid": pid,
            "path": lease_fifo_path(waiter_directory, lease_nonce, pid),
            "owner_lock": owner_lock_path,
            "owner_sha256": owner_digest,
            "registration_sha256": registration_digest,
            "scope_kind": registration["scope_kind"],
            "assurance": registration["assurance"],
            "scope_plan_sha256": hashlib.sha256(intent["scope_plan"].encode()).hexdigest(),
            "scope_id_sha256": hashlib.sha256(registration["scope_id"].encode()).hexdigest(),
        })
    pids = [specification["pid"] for specification in specifications]
    if len(pids) != len(set(pids)):
        raise RuntimeError("capacity fixture launch identities are not unique")
    return specifications


def prove_lifetime_owner_cohort(specifications, lease_cohort):
    """Prove every gated task retains its exclusive lifetime-owner lease."""
    started_unix_ns = time.time_ns()
    started_monotonic_ns = time.monotonic_ns()
    evidence = []
    lease_cohort.assert_live()
    for index, specification in enumerate(sorted(specifications, key=lambda item: item["task_id"])):
        lock_path = specification["owner_lock"]
        descriptor = os.open(
            lock_path,
            os.O_RDWR | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK,
        )
        try:
            info = os.fstat(descriptor)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid():
                raise RuntimeError(
                    "capacity lifetime-owner lock is unsafe: " + specification["task_id"]
                )
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as error:
                if error.errno not in (errno.EACCES, errno.EAGAIN):
                    raise
            else:
                fcntl.flock(descriptor, fcntl.LOCK_UN)
                raise RuntimeError(
                    "capacity task has no live lifetime owner: " + specification["task_id"]
                )
        finally:
            os.close(descriptor)
        evidence.append({
            "root_id": specification["root_id"],
            "task_id": specification["task_id"],
            "owner_sha256": specification["owner_sha256"],
            "registration_sha256": specification["registration_sha256"],
            "scope_kind": specification["scope_kind"],
            "assurance": specification["assurance"],
            "scope_plan_sha256": specification["scope_plan_sha256"],
            "scope_id_sha256": specification["scope_id_sha256"],
            "owner_lock": {"device": info.st_dev, "inode": info.st_ino},
        })
        if (index + 1) % 16 == 0:
            lease_cohort.assert_live()
    lease_cohort.assert_live()
    ended_monotonic_ns = time.monotonic_ns()
    ended_unix_ns = time.time_ns()
    return {
        "started_unix_ns": started_unix_ns,
        "ended_unix_ns": ended_unix_ns,
        "duration_ns": max(0, ended_monotonic_ns - started_monotonic_ns),
        "lease_probe": "exclusive-flock-contended",
        "counted_task_count": len(evidence),
        "counted_tasks": evidence,
    }


def verify_current_installation_upgrade(delegate, state, workspace, brief, release_gate, waiter_directory,
                                        env, old_layout,
                                        old_layout_owned,
                                        client, config, parallel_tasks, dispatch_workers,
                                        response_directory, evidence=None, *, lease_nonce,
                                        client_call_log, attempted_task_ids,
                                        capture_root, run_receipt=None):
    """Remove disposable package files while a state-owned task is running."""
    waiting_brief = brief.with_name("waiting-brief.md")
    waiting_brief.write_text("DELEGATE_PRIVATE_UPGRADE_WAIT\n")
    receipts = Path(response_directory)
    receipts.mkdir(mode=0o700, exist_ok=True)
    evidence = evidence if evidence is not None else {}
    dispatch_lock = threading.Lock()
    dispatch_in_flight = 0
    peak_dispatch_in_flight = 0
    dispatch_calls_completed = 0
    admitted_task_count = 0
    dispatch_started = time.monotonic()
    if run_receipt is not None:
        run_receipt.phase("capacity_dispatch")

    def dispatch_waiting(index):
        nonlocal dispatch_in_flight, peak_dispatch_in_flight
        nonlocal dispatch_calls_completed
        receipt = receipts / ("dispatch-%03d.json" % index)
        with dispatch_lock:
            dispatch_in_flight += 1
            peak_dispatch_in_flight = max(peak_dispatch_in_flight, dispatch_in_flight)
        try:
            response = run_json(delegate, env, [
                "--root", str(state), "dispatch", "--provider", "pi:json",
                "--brief", str(waiting_brief), "--cwd", str(workspace),
                "--permission", "read-only", "--budget", CAPACITY_TASK_BUDGET, "--json",
            ], expected=(0, 1), timeout=600, receipt=receipt, phase="managed-runner",
                        capture_root=capture_root)
            if response.get("task_record") == "created":
                task_id = response.get("task_id")
                if not isinstance(task_id, str) or not task_id:
                    raise RuntimeError("created dispatch response omitted its task identity")
                with dispatch_lock:
                    if task_id not in attempted_task_ids:
                        attempted_task_ids.append(task_id)
        finally:
            with dispatch_lock:
                dispatch_in_flight -= 1
                dispatch_calls_completed += 1
                if run_receipt is not None:
                    run_receipt.update_counts(
                        dispatch_completed_count=dispatch_calls_completed,
                    )
        return response

    print("Dispatching %d isolated fixture tasks with %d dispatch workers" % (parallel_tasks, dispatch_workers), flush=True)
    with ThreadPoolExecutor(max_workers=dispatch_workers) as pool:
        responses = list(pool.map(dispatch_waiting, range(parallel_tasks)))
    dispatch_finished = time.monotonic()
    if peak_dispatch_in_flight <= 0:
        raise RuntimeError("dispatch batch did not complete its dispatch calls")
    dispatch_elapsed = round(max(0.0, dispatch_finished - dispatch_started), 6)

    def confirm_admission(index_response):
        nonlocal admitted_task_count
        index, response = index_response
        observe_created_admission(
            delegate, state, env, response, receipts / ("dispatch-%03d.json" % index),
            phase="managed-runner-observation", capture_root=capture_root,
        )
        with dispatch_lock:
            admitted_task_count += 1
            if run_receipt is not None:
                run_receipt.update_counts(admitted_task_count=admitted_task_count)

    if run_receipt is not None:
        run_receipt.phase("capacity_admission")
    with ThreadPoolExecutor(max_workers=dispatch_workers) as pool:
        list(pool.map(confirm_admission, enumerate(responses)))
    admission_elapsed = round(max(0.0, time.monotonic() - dispatch_started), 6)
    task_ids = [response["task_id"] for response in responses]
    if len(task_ids) != len(set(task_ids)):
        raise RuntimeError("dispatch batch returned duplicate task identities")
    if any(not response.get("root_id") for response in responses):
        raise RuntimeError("dispatch batch response omitted its supervisor root identity")
    expected_labels = {"delegate:" + response["root_id"] + ":" + response["task_id"] for response in responses}
    if len(expected_labels) != parallel_tasks:
        raise RuntimeError("dispatch batch did not produce one unique task label per request")
    dispatch_batch = {
        "requested_caller_concurrency": dispatch_workers,
        "observed_peak_in_flight_dispatch_calls": peak_dispatch_in_flight,
        "completed_dispatch_call_count": dispatch_calls_completed,
        "dispatch_elapsed_seconds": dispatch_elapsed,
        "admission_elapsed_seconds": admission_elapsed,
        "admitted_task_count": admitted_task_count,
    }
    if (dispatch_batch["completed_dispatch_call_count"] != parallel_tasks
            or dispatch_batch["admitted_task_count"] != parallel_tasks):
        raise RuntimeError("dispatch batch did not admit every requested fixture task: %r" % dispatch_batch)
    (receipts / "dispatch-batch.json").write_text(json.dumps(dispatch_batch, sort_keys=True) + "\n")
    evidence["dispatch_batch"] = dispatch_batch
    deadline = time.monotonic() + 120
    capacity_proof = {
        "requested_task_count": parallel_tasks,
        "observed_running_task_count": 0,
        "peak_observed_running_task_count": 0,
        "observed_lease_candidate_count": 0,
        "peak_observed_lease_candidate_count": 0,
        "observed_live_lease_count": 0,
        "observed_lifetime_owner_lease_count": 0,
        "lease_protocol": "fifo-nonce-v1",
        "lease_nonce_sha256": hashlib.sha256(lease_nonce.encode()).hexdigest(),
        "queue_confirmed_while_leases_held": False,
        "proof_established_before_gate": False,
        "gate_opened_after_proof": False,
    }
    lease_cohort = None
    try:
        if run_receipt is not None:
            run_receipt.phase("capacity_observation")
        while True:
            snapshot_metrics = {}
            snapshot = pueue_snapshot(client, config, env, snapshot_metrics,
                                      phase="capacity-observation", capture_root=capture_root)
            rows_by_label = task_rows_by_label(snapshot, expected_labels)
            observed_running = sum(
                1 for row in rows_by_label.values() if pueue_state(row) == "Running"
            )
            specifications = capacity_lease_specifications(
                state, responses, waiter_directory, lease_nonce,
            )
            lease_candidates = sum(
                1 for specification in specifications if specification["path"].exists()
            )
            capacity_proof["observed_running_task_count"] = observed_running
            capacity_proof["peak_observed_running_task_count"] = max(
                capacity_proof["peak_observed_running_task_count"], observed_running,
            )
            capacity_proof["observed_lease_candidate_count"] = lease_candidates
            capacity_proof["peak_observed_lease_candidate_count"] = max(
                capacity_proof["peak_observed_lease_candidate_count"], lease_candidates,
            )
            if "status_bytes" in snapshot_metrics:
                capacity_proof["status_bytes"] = max(
                    capacity_proof.get("status_bytes", 0), snapshot_metrics["status_bytes"],
                )
            if run_receipt is not None:
                run_receipt.update_counts(
                    running_task_count=observed_running,
                    live_lease_count=0,
                )
                run_receipt.update_queue(
                    task_count=len(snapshot.get("tasks", {})),
                    running_count=observed_running,
                    status_bytes=snapshot_metrics.get("status_bytes"),
                )
            if (observed_running == parallel_tasks
                    and len(specifications) == parallel_tasks
                    and lease_candidates == parallel_tasks):
                lease_cohort = LiveFIFOLeaseCohort.acquire(specifications, lease_nonce)
                confirmation_metrics = {}
                confirmation = pueue_snapshot(
                    client, config, env, confirmation_metrics, phase="capacity-observation",
                    capture_root=capture_root,
                )
                confirmed_rows = task_rows_by_label(confirmation, expected_labels)
                confirmed_running = sum(
                    1 for row in confirmed_rows.values() if pueue_state(row) == "Running"
                )
                lease_cohort.assert_live()
                if confirmed_running != parallel_tasks:
                    raise RuntimeError(
                        "capacity fixture left Running while the live lease cohort formed"
                    )
                owner_cohort = prove_lifetime_owner_cohort(specifications, lease_cohort)
                counted_tasks = owner_cohort.get("counted_tasks")
                if (
                    not isinstance(counted_tasks, list)
                    or owner_cohort.get("counted_task_count") != parallel_tasks
                    or len(counted_tasks) != parallel_tasks
                    or {item.get("task_id") for item in counted_tasks
                        if isinstance(item, dict)} != set(task_ids)
                ):
                    raise RuntimeError(
                        "capacity lifetime-owner proof did not bind every admitted task"
                    )
                final_confirmation_metrics = {}
                final_confirmation = pueue_snapshot(
                    client, config, env, final_confirmation_metrics,
                    phase="capacity-observation", capture_root=capture_root,
                )
                final_rows = task_rows_by_label(final_confirmation, expected_labels)
                final_running = sum(
                    1 for row in final_rows.values() if pueue_state(row) == "Running"
                )
                lease_cohort.assert_live()
                if final_running != parallel_tasks:
                    raise RuntimeError(
                        "capacity fixture left Running during lifetime-owner proof"
                    )
                capacity_proof["observed_running_task_count"] = confirmed_running
                capacity_proof["observed_live_lease_count"] = len(lease_cohort.leases)
                capacity_proof["observed_lifetime_owner_lease_count"] = len(
                    owner_cohort["counted_tasks"]
                )
                capacity_proof["queue_confirmed_while_leases_held"] = True
                capacity_proof["proof_established_before_gate"] = True
                capacity_proof["file_descriptors_while_leases_held"] = descriptor_evidence()
                capacity_proof["lifetime_owner_overlap"] = owner_cohort
                scope_kind_counts = {}
                assurance_counts = {}
                for counted in counted_tasks:
                    scope_kind = counted["scope_kind"]
                    assurance = counted["assurance"]
                    scope_kind_counts[scope_kind] = scope_kind_counts.get(scope_kind, 0) + 1
                    assurance_counts[assurance] = assurance_counts.get(assurance, 0) + 1
                capacity_proof["scope_kind_counts"] = dict(sorted(scope_kind_counts.items()))
                capacity_proof["assurance_counts"] = dict(sorted(assurance_counts.items()))
                for metrics in (confirmation_metrics, final_confirmation_metrics):
                    if "status_bytes" not in metrics:
                        continue
                    capacity_proof["status_bytes"] = max(
                        capacity_proof.get("status_bytes", 0),
                        metrics["status_bytes"],
                    )
                if run_receipt is not None:
                    run_receipt.update_counts(
                        running_task_count=final_running,
                        live_lease_count=len(lease_cohort.leases),
                    )
                    run_receipt.update_queue(
                        task_count=len(final_confirmation.get("tasks", {})),
                        running_count=final_running,
                        status_bytes=final_confirmation_metrics.get("status_bytes"),
                    )
                break
            if time.monotonic() >= deadline:
                raise RuntimeError(
                    "fixture tasks did not all reach concurrent leased Running state: %r"
                    % capacity_proof
                )
            time.sleep(0.1)
        print("Observed %d concurrent leased running tasks" % parallel_tasks, flush=True)
        if "status_bytes" not in capacity_proof:
            raise RuntimeError("capacity proof did not retain its Pueue status size")
        (receipts / "capacity-proof.json").write_text(
            json.dumps(capacity_proof, sort_keys=True) + "\n"
        )
        if parallel_tasks >= 200 and capacity_proof["status_bytes"] <= 1024 * 1024:
            raise RuntimeError("large-queue acceptance did not exceed the former 1 MiB status limit")
        if run_receipt is not None:
            run_receipt.stage("live-capacity-cohort-proved")
            run_receipt.phase("package_removal")
        rollback_owned_directories(old_layout_owned)
        if old_layout.exists() or old_layout.is_symlink():
            raise RuntimeError("old package layout deletion was not verified: " + str(old_layout))
        # Observation now runs from another package layout while all tasks remain
        # active. Each dispatch response is retained and never submitted twice.
        status_probe_responses = responses[:min(parallel_tasks, 16)]
        status_probe_barrier = threading.Barrier(len(status_probe_responses))
        status_snapshots_before = wrapper_command_counts(client_call_log).get("status", 0)

        def observe(response):
            task_id = response["task_id"]
            status_probe_barrier.wait(timeout=30)
            observed = read_created_task_status(
                delegate, state, env, task_id,
                receipts / ("status-after-upgrade-" + task_id + ".json"),
                phase="managed-runner-observation", capture_root=capture_root)
            supervisor = observed.get("supervisor", {})
            if (observed.get("admission") != "admitted" or observed.get("error")
                    or observed.get("status") == "timed_out"
                    or supervisor.get("matched") is not True
                    or supervisor.get("state") != "running"):
                raise RuntimeError(
                    "upgraded installation did not observe its gated task running: %r"
                    % observed
                )

        if run_receipt is not None:
            run_receipt.phase("upgraded_observation")
        with ThreadPoolExecutor(max_workers=len(status_probe_responses)) as pool:
            list(pool.map(observe, status_probe_responses))
        status_snapshot_count = (
            wrapper_command_counts(client_call_log).get("status", 0) - status_snapshots_before
        )
        status_coalescing = {
            "concurrent_status_call_count": len(status_probe_responses),
            "pueue_status_snapshot_count": status_snapshot_count,
            "shared_status_call_count": len(status_probe_responses) - status_snapshot_count,
        }
        if status_snapshot_count <= 0 or status_snapshot_count >= len(status_probe_responses):
            raise RuntimeError(
                "concurrent public status calls did not coalesce queue snapshots: %r"
                % status_coalescing
            )
        (receipts / "status-coalescing.json").write_text(
            json.dumps(status_coalescing, sort_keys=True) + "\n"
        )
        evidence["status_coalescing"] = status_coalescing
        lease_cohort.assert_live()
        release_gate.release()
        capacity_proof["gate_opened_after_proof"] = True
        (receipts / "capacity-proof.json").write_text(
            json.dumps(capacity_proof, sort_keys=True) + "\n"
        )
        evidence["capacity_proof"] = capacity_proof
    finally:
        if lease_cohort is not None:
            lease_cohort.close()

    def collect(task_id):
        collected = run_json(delegate, env, [
            "--root", str(state), "collect", task_id, "--watch", COLLECT_WATCH, "--json",
        ], timeout=COLLECT_TIMEOUT_SECONDS, receipt=receipts / ("collect-" + task_id + ".json"),
                           phase="managed-runner-collection", capture_root=capture_root)
        if collected.get("outcome", {}).get("verdict") != "committed":
            raise RuntimeError("removing old package files disrupted the state-owned task")
        if run_receipt is not None:
            run_receipt.increment("collected_task_count")

    if run_receipt is not None:
        run_receipt.phase("capacity_collection")
    with ThreadPoolExecutor(max_workers=min(parallel_tasks, 16)) as pool:
        list(pool.map(collect, task_ids))
    shutdown_private_supervisor(
        client, config, env, state=state, task_ids=tuple(task_ids),
        capture_root=capture_root)
    # A subsequent dispatch must bootstrap from durable state without the old
    # package. This is a new requested turn, never a retry of uncertain work.
    response = run_json(delegate, env, [
        "--root", str(state), "dispatch", "--provider", "pi:json",
        "--brief", str(brief), "--cwd", str(workspace),
        "--permission", "read-only", "--budget", TASK_BUDGET, "--json",
    ], receipt=receipts / "dispatch-restarted.json", phase="restart-dispatch",
                    capture_root=capture_root)
    if response.get("task_record") != "created" or response.get("admission") != "admitted":
        raise RuntimeError("restart probe was not durably admitted")
    restarted_id = response["task_id"]
    with dispatch_lock:
        if restarted_id not in attempted_task_ids:
            attempted_task_ids.append(restarted_id)
    run_json(delegate, env, ["--root", str(state), "status", restarted_id, "--json"],
             receipt=receipts / "status-restarted.json", phase="restart-observation",
             capture_root=capture_root)
    collected = run_json(delegate, env, [
        "--root", str(state), "collect", restarted_id, "--watch", COLLECT_WATCH, "--json",
    ], timeout=COLLECT_TIMEOUT_SECONDS, receipt=receipts / "collect-restarted.json",
                           phase="restart-collection", capture_root=capture_root)
    if collected.get("outcome", {}).get("verdict") != "committed":
        raise RuntimeError("private supervisor restart did not complete its task")
    if run_receipt is not None:
        run_receipt.increment("collected_task_count")
        run_receipt.stage("current-installation-restart-proved")
    return [*task_ids, restarted_id], capacity_proof["status_bytes"]


def remove_private_acceptance_root(root_token: OwnedDirectory,
                                  owned_directories: tuple[OwnedDirectory, ...],
                                  managed_state_roots=()):
    """Authorize managed task closure, then remove the exact owned root."""
    if not isinstance(root_token, OwnedDirectory):
        raise RuntimeError("private acceptance root identity token is required")
    if (root_token.directory.parent != root_token.parent or
            not root_token.directory.name.startswith(PRIVATE_ACCEPTANCE_PREFIX)):
        raise RuntimeError("private acceptance root is outside its exact owned parent child: " +
                           str(root_token.directory))
    entries = tuple(owned_directories)
    if not any(entry is root_token for entry in entries):
        raise RuntimeError("private acceptance root token is absent from ownership sequence")
    for entry in managed_state_roots:
        if isinstance(entry, tuple) and len(entry) == 2:
            state, task_ids = entry
        else:
            state, task_ids = entry, ()
        authorize_managed_lifetime_closure(state, tuple(task_ids))
    rollback_owned_directories(entries)
    if root_token.directory.exists() or root_token.directory.is_symlink():
        raise RuntimeError("private acceptance root deletion was not verified: " +
                           str(root_token.directory))


def default_stop_live_lease(client, config, env, state, response, waiter_directory,
                            lease_nonce, *, capture_root):
    """Prove one fixture provider is live before exercising its stop path."""
    task_id = response["task_id"]
    root_id = response["root_id"]
    label = "delegate:" + root_id + ":" + task_id
    wait_for_pueue_state(
        client, config, env, label, "Running", phase="default-stop-observation",
        capture_root=capture_root,
    )
    deadline = time.monotonic() + 30
    while True:
        specifications = capacity_lease_specifications(
            state, [response], waiter_directory, lease_nonce,
        )
        if len(specifications) == 1 and specifications[0]["path"].exists():
            lease_started = time.time_ns()
            cohort = LiveFIFOLeaseCohort.acquire(specifications, lease_nonce)
            cohort.assert_live()
            lease_observed = time.time_ns()
            return specifications[0], cohort, {
                "observed_at_unix_ns": lease_observed,
                "acquire_elapsed_ns": max(0, lease_observed - lease_started),
            }
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise RuntimeError(
                "fixture provider did not reach a live lease before its bounded observation deadline: "
                + task_id
            )
        time.sleep(min(0.05, remaining))


def default_stop_status_row(status, request_id):
    rows = status.get("stops") or []
    for row in rows:
        if isinstance(row, dict) and row.get("request_id") == request_id:
            return row
    raise RuntimeError("status did not retain the exact stop request: " + request_id)


def default_stop_dispatch_evidence(response):
    failure = response.get("failure") or {}
    return {
        "task_id": response.get("task_id"),
        "root_id": response.get("root_id"),
        "task_record": response.get("task_record"),
        "admission": response.get("admission"),
        "failure": {
            key: failure.get(key)
            for key in ("code", "stage", "next_action")
            if failure.get(key) is not None
        },
    }


def default_stop_lease_evidence(specification, lease_observation):
    return {
        "pid": specification["pid"],
        "scope_kind": specification["scope_kind"],
        "assurance": specification["assurance"],
        "owner_sha256": specification["owner_sha256"],
        "registration_sha256": specification["registration_sha256"],
        "scope_plan_sha256": specification["scope_plan_sha256"],
        "scope_id_sha256": specification["scope_id_sha256"],
        **lease_observation,
    }


def default_stop_stop_evidence(stop):
    if not isinstance(stop, dict):
        raise RuntimeError("stop response omitted its durable stop facts")
    return {
        key: stop.get(key)
        for key in (
            "request_id", "cause", "requested", "matched", "attempted",
            "action", "numeric_task_id", "acknowledged", "in_flight",
            "observed_state", "terminated",
        )
        if key in stop
    }


def default_stop_closure_evidence(closure, task_id, cause):
    if not isinstance(closure, dict):
        raise RuntimeError("canonical lifetime closure omitted task evidence: " + task_id)
    termination = closure.get("termination")
    registration = closure.get("registration")
    worker_wait = closure.get("worker_wait")
    launches = closure.get("launches")
    if not isinstance(termination, dict) or termination.get("cause") != cause:
        raise RuntimeError("canonical lifetime closure recorded the wrong termination cause: " + task_id)
    if termination.get("scope_empty") is not True:
        raise RuntimeError("canonical lifetime closure did not prove an empty native scope: " + task_id)
    intent = closure.get("intent")
    if not isinstance(intent, dict) or not isinstance(registration, dict) or not isinstance(worker_wait, dict):
        raise RuntimeError("canonical lifetime closure omitted registration or worker Wait evidence: " + task_id)
    if not isinstance(launches, list) or not launches:
        raise RuntimeError("canonical lifetime closure omitted provider launch evidence: " + task_id)
    return {
        "task_id": task_id,
        "termination": {
            "cause": termination.get("cause"),
            "scope_empty": termination.get("scope_empty"),
            "observed_at": termination.get("observed_at"),
        },
        "lifetime": {
            "deadline": intent.get("deadline"),
            "created_at": intent.get("created_at"),
            "registered_at": registration.get("created_at"),
        },
        "worker_wait": {
            key: worker_wait.get(key)
            for key in ("child_pid", "result", "exit_code", "observed_at")
            if key in worker_wait
        },
        "launch_count": len(launches),
        "scope": {
            "kind": registration.get("scope_kind"),
            "assurance": registration.get("assurance"),
        },
    }


def require_empty_fixture_waiters(waiter_directory):
    entries = tuple(Path(waiter_directory).iterdir())
    if entries:
        raise RuntimeError(
            "fixture provider lease entries remained after canonical lifetime closure: "
            + str(len(entries))
        )


def run_default_stop_acceptance(delegate, client, state, workspace, brief, gate,
                                waiter_directory, config, env, response_directory,
                                provider_fixture, lease_nonce, run_receipt,
                                attempted_task_ids, *, capture_root):
    """Exercise default user-stop and sub-second budget termination on live fixtures."""
    waiting_brief = brief.with_name("default-stop-waiting-brief.md")
    waiting_brief.write_text("DELEGATE_PRIVATE_UPGRADE_WAIT\n")
    waiting_brief.chmod(0o600)
    receipts = Path(response_directory)
    release_gate = LockedReleaseGate.acquire(gate)
    task_evidence = {}
    acceptance_evidence = {
        "scenario": DEFAULT_STOP_SCENARIO,
        "requested_task_count": DEFAULT_STOP_TASK_COUNT,
        "provider": "pi:json",
        "fixture": "fake-pi-live-fifo",
        "supervisor": "real-pueue-private",
        "stop_poll_policy": {
            "override": False,
            "mode": "production-default",
            "timing_claim": "observed native lifecycle timestamps; no fixed latency guarantee",
        },
        "fingerprints": {"provider_fixture": file_fingerprint(provider_fixture)},
        "tasks": task_evidence,
        "cleanup": {
            "fixture_waiter_entries": "pending",
            "lifetime_closure": "canonical-managed-validator",
            "supervisor_shutdown": "pending",
        },
    }
    # Keep completed task evidence in the receipt even when fixture cleanup fails later.
    run_receipt.add_details(default_stop=acceptance_evidence)

    def dispatch_fixture(task_id, budget, receipt_name):
        response = run_json(
            delegate, env, [
                "--root", str(state), "dispatch", "--provider", "pi:json",
                "--brief", str(waiting_brief), "--cwd", str(workspace),
                "--id", task_id, "--permission", "read-only", "--budget", budget,
                "--json",
            ], expected=(0, 1), timeout=45,
            receipt=receipts / receipt_name, phase="default-stop-dispatch",
            capture_root=capture_root,
        )
        if response.get("task_id") != task_id or response.get("task_record") != "created":
            raise RuntimeError("default-stop dispatch did not create the requested task")
        if not response.get("root_id") or response.get("admission") != "admitted":
            raise RuntimeError("default-stop dispatch did not admit the requested task")
        observed = observe_created_admission(
            delegate, state, env, response, receipts / receipt_name,
            phase="default-stop-admission-observation", capture_root=capture_root,
        )
        if observed.get("task_id") != task_id or observed.get("admission") != "admitted":
            raise RuntimeError("default-stop admission observation did not confirm the same task")
        run_receipt.increment("dispatch_completed_count")
        run_receipt.increment("admitted_task_count")
        return response

    def close_task(response, cause, status_receipt):
        task_id = response["task_id"]
        closure = await_managed_lifetime_closure(state, (task_id,), timeout=30)
        task_closure = closure.get("tasks", {}).get(task_id)
        closure_evidence = default_stop_closure_evidence(task_closure, task_id, cause)
        status, queue_observation = await_created_task_ended(
            delegate, state, env, response, receipts / status_receipt,
            phase="default-stop-observation", capture_root=capture_root,
        )
        return status, closure_evidence, queue_observation

    try:
        run_receipt.phase("default_stop_user")
        user_task_id = uuid.uuid4().hex
        attempted_task_ids.append(user_task_id)
        user_dispatch = dispatch_fixture(
            user_task_id, DEFAULT_STOP_TASK_BUDGET, "dispatch-default-stop-user.json",
        )
        user_specification, user_cohort, user_lease = default_stop_live_lease(
            client, config, env, state, user_dispatch, waiter_directory, lease_nonce,
            capture_root=capture_root,
        )
        run_receipt.update_counts(running_task_count=1, live_lease_count=1)
        with user_cohort:
            cancel_started = time.time_ns()
            cancel = run_json(
                delegate, env, ["--root", str(state), "cancel", user_task_id, "--json"],
                receipt=receipts / "cancel-default-stop-user.json",
                phase="default-stop-user-request", capture_root=capture_root,
            )
            cancel_finished = time.time_ns()
            if cancel.get("task_id") != user_task_id or cancel.get("root_id") != user_dispatch.get("root_id"):
                raise RuntimeError("default-stop cancel response changed task identity")
            user_stop = cancel.get("stop")
            if not isinstance(user_stop, dict) or (
                    user_stop.get("requested") is not True
                    or user_stop.get("cause") != "user"
                    or user_stop.get("in_flight") is not True
                    or user_stop.get("attempted") is not True
                    or not isinstance(user_stop.get("request_id"), str)):
                raise RuntimeError("default-stop cancel did not record a live user stop request")
            user_status, user_closure, user_queue_observation = close_task(
                user_dispatch, "requested", "status-default-stop-user.json",
            )
            user_row = default_stop_status_row(user_status, user_stop["request_id"])
            if user_row.get("cause") != "user" or user_row.get("terminated") is not True:
                raise RuntimeError("default-stop status did not retain the completed user stop")
            task_evidence["user_stop"] = {
                "budget": DEFAULT_STOP_TASK_BUDGET,
                "dispatch": default_stop_dispatch_evidence(user_dispatch),
                "lease": default_stop_lease_evidence(user_specification, user_lease),
                "cancel": {
                    "receipt": "cancel-default-stop-user.json",
                    "started_at_unix_ns": cancel_started,
                    "finished_at_unix_ns": cancel_finished,
                    "stop": default_stop_stop_evidence(user_stop),
                },
                "status": {
                    "receipt": user_queue_observation["final_receipt"],
                    "liveness": user_status.get("liveness"),
                    "stop": default_stop_stop_evidence(user_row),
                    "queue_ended_observation": user_queue_observation,
                },
                "closure": user_closure,
                "fixture_lease_cleanup": {"state": "pending"},
            }
            task_evidence["user_stop"]["fixture_lease_cleanup"] = user_cohort.remove_closed_fifos()
            run_receipt.update_counts(running_task_count=0, live_lease_count=0)

        run_receipt.phase("default_stop_budget")
        budget_task_id = uuid.uuid4().hex
        attempted_task_ids.append(budget_task_id)
        budget_dispatch = dispatch_fixture(
            budget_task_id, DEFAULT_STOP_BUDGET, "dispatch-default-stop-budget.json",
        )
        budget_specification, budget_cohort, budget_lease = default_stop_live_lease(
            client, config, env, state, budget_dispatch, waiter_directory, lease_nonce,
            capture_root=capture_root,
        )
        run_receipt.update_counts(running_task_count=1, live_lease_count=1)
        with budget_cohort:
            budget_status, budget_closure, budget_queue_observation = close_task(
                budget_dispatch, "budget", "status-default-stop-budget.json",
            )
            budget_row = default_stop_status_row(budget_status, "budget")
            if budget_row.get("cause") != "budget" or budget_row.get("terminated") is not True:
                raise RuntimeError("default-stop status did not retain the budget termination")
            if any(row.get("cause") == "user" for row in (budget_status.get("stops") or [])):
                raise RuntimeError("budget fixture unexpectedly acquired a user stop request")
            task_evidence["budget_stop"] = {
                "budget": DEFAULT_STOP_BUDGET,
                "dispatch": default_stop_dispatch_evidence(budget_dispatch),
                "lease": default_stop_lease_evidence(budget_specification, budget_lease),
                "status": {
                    "receipt": budget_queue_observation["final_receipt"],
                    "liveness": budget_status.get("liveness"),
                    "stop": default_stop_stop_evidence(budget_row),
                    "queue_ended_observation": budget_queue_observation,
                },
                "closure": budget_closure,
                "fixture_lease_cleanup": {"state": "pending"},
            }
            task_evidence["budget_stop"]["fixture_lease_cleanup"] = budget_cohort.remove_closed_fifos()
            run_receipt.update_counts(running_task_count=0, live_lease_count=0)
        require_empty_fixture_waiters(waiter_directory)
        acceptance_evidence["cleanup"]["fixture_waiter_entries"] = 0
        release_gate.release()
        release_gate = None
        shutdown_private_supervisor(
            client, config, env, state=state, task_ids=tuple(attempted_task_ids),
            capture_root=capture_root,
        )
        if run_receipt is not None:
            run_receipt.update_counts(
                dispatch_completed_count=DEFAULT_STOP_TASK_COUNT,
                admitted_task_count=DEFAULT_STOP_TASK_COUNT,
            )
        acceptance_evidence["cleanup"]["supervisor_shutdown"] = "verified"
        return acceptance_evidence
    finally:
        if release_gate is not None:
            release_gate.release()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--delegate", type=Path, required=True)
    parser.add_argument("--runner", type=Path, required=True)
    parser.add_argument("--upgraded-delegate", type=Path)
    parser.add_argument("--upgraded-runner", type=Path)
    parser.add_argument("--pueue", type=Path, required=True)
    parser.add_argument("--pueued", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--scenario", choices=("capacity", DEFAULT_STOP_SCENARIO), default="capacity")
    parser.add_argument("--parallel-tasks", type=int, default=CAPACITY_TARGET_TASKS)
    parser.add_argument("--dispatch-workers", type=int,
                        help="concurrent dispatch callers (defaults to 16; capped at --parallel-tasks; this load-generator setting is not a product task limit)")
    parser.add_argument("--runtime-only", action="store_true",
                        help="exercise a root without a legacy model-discovery group journal")
    args = parser.parse_args()
    if args.scenario == DEFAULT_STOP_SCENARIO:
        if args.parallel_tasks != DEFAULT_STOP_TASK_COUNT:
            parser.error("--parallel-tasks must be exactly %d for the %s scenario" %
                         (DEFAULT_STOP_TASK_COUNT, DEFAULT_STOP_SCENARIO))
        if not args.runtime_only:
            parser.error("--scenario %s requires --runtime-only" % DEFAULT_STOP_SCENARIO)
        if args.upgraded_delegate is not None or args.upgraded_runner is not None:
            parser.error("--scenario %s does not accept upgraded installation arguments" %
                         DEFAULT_STOP_SCENARIO)
    elif args.parallel_tasks != CAPACITY_TARGET_TASKS:
        parser.error("--parallel-tasks must be exactly %d for the capacity proof" % CAPACITY_TARGET_TASKS)
    if args.dispatch_workers is not None and args.dispatch_workers <= 0:
        parser.error("--dispatch-workers must be positive")
    dispatch_workers = min(args.parallel_tasks, args.dispatch_workers or DEFAULT_DISPATCH_WORKERS)

    output = args.output.resolve()
    response_directory = output.with_name(output.name + ".responses")
    if output.exists() or output.is_symlink():
        raise RuntimeError("private CLI acceptance output already exists: " + str(output))
    if response_directory.exists() or response_directory.is_symlink():
        raise RuntimeError("private CLI response destination already exists: " + str(response_directory))
    output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)

    legacy_mode = args.upgraded_delegate is not None and args.upgraded_runner is not None
    if legacy_mode and args.runtime_only:
        parser.error("--runtime-only requires the current-installation acceptance scenario")
    queued_runner_check = (
        "the legacy CLI queued a task whose runner path pointed into its install"
        if legacy_mode else
        "the CLI queued a task whose runner path pointed to the durable state root"
    )
    inspection_runner_check = (
        "the upgraded CLI preserved an ambiguous legacy inspection runner and completed it before cleanup"
        if legacy_mode else
        "the upgraded CLI retained and completed an explicitly managed state-root inspection runner"
    )
    binary_sources = {
        "delegate": args.delegate,
        "delegate-run": args.runner,
        "pueue": args.pueue,
        "pueued": args.pueued,
    }
    if args.upgraded_delegate is not None:
        binary_sources["upgraded-delegate"] = args.upgraded_delegate
    if args.upgraded_runner is not None:
        binary_sources["upgraded-delegate-run"] = args.upgraded_runner
    run_receipt = AcceptanceReceipt(
        output, args.parallel_tasks, dispatch_workers, binary_sources,
    )
    run_receipt.add_details(scenario=args.scenario)
    ownership = DirectoryOwner()
    old_layout_ownership = DirectoryOwner()
    old_layout_owned = ()
    base = None
    base_token = None
    success = False
    failure = None
    inspection_process = None
    env = {}
    private_config = None
    gate = None
    release_gate = None
    waiter_directory = None
    old_install = None
    old_libexec = None
    upgraded_libexec = None
    supervisor_cleanups = []
    managed_state_roots = []
    attempted_task_ids = []
    lease_nonce = uuid.uuid4().hex
    try:
        if not os.access("/bin/ps", os.X_OK):
            raise AcceptanceBlocked("shutdown-probe-unavailable")
        try:
            delegate_binary = args.delegate.resolve(strict=True)
            runner_binary = args.runner.resolve(strict=True)
            upgraded_delegate_binary = (args.upgraded_delegate or args.delegate).resolve(strict=True)
            upgraded_runner_binary = (args.upgraded_runner or args.runner).resolve(strict=True)
            pueue_binary = args.pueue.resolve(strict=True)
            pueued_binary = args.pueued.resolve(strict=True)
        except OSError as error:
            raise AcceptanceBlocked("required-binary-unavailable") from error
        run_receipt.phase("fixture_setup")
        acceptance_parent = Path.home().resolve(strict=True)
        base_path = Path(tempfile.mkdtemp(
            prefix=PRIVATE_ACCEPTANCE_PREFIX, dir=str(acceptance_parent))).resolve(strict=True)
        base_token = ownership.adopt_created(
            "private CLI acceptance root", base_path, acceptance_parent)
        base = base_token.directory
        private_config = base / "state" / ".supervisor" / "pueue.yml"
        gate = base / "blocker-gate"
        waiter_directory = base / "provider-waiters"
        old_root = old_layout_ownership.create(
            "old package root", base / "old", base).directory
        old_install, old_bin, old_libexec = install_package_layout(
            old_layout_ownership, delegate_binary,
            runner_binary, old_root, not legacy_mode)
        old_layout_owned = old_layout_ownership.transfer()
        upgraded_root = ownership.create(
            "upgraded package root", base / "upgraded", base).directory
        provider_bin = ownership.create("provider binaries", base / "provider-bin", base).directory
        home = ownership.create("private CLI home", base / "home", base).directory
        workspace = ownership.create("private CLI workspace", base / "workspace", base).directory
        state = ownership.create("private CLI state", base / "state", base).directory
        waiter_directory = ownership.create(
            "provider waiter directory", base / "provider-waiters", base).directory
        managed_state_roots.append((state, attempted_task_ids))
        upgraded_install, upgraded_bin, upgraded_libexec = install_package_layout(
            ownership, upgraded_delegate_binary,
            upgraded_runner_binary,
            upgraded_root, True)
        response_directory.mkdir(mode=0o700)
        old_daemon_marker = base / "old-daemon-starts"
        upgraded_daemon_marker = base / "upgraded-daemon-starts"
        install_wrapper(old_libexec / "pueue", pueue_binary, base / "old-client-starts", "old-install", "client")
        install_wrapper(old_libexec / "pueued", pueued_binary, old_daemon_marker, "old-install", "daemon")
        install_wrapper(upgraded_libexec / "pueue", pueue_binary, base / "upgraded-client-starts", "upgraded-install", "client")
        install_wrapper(upgraded_libexec / "pueued", pueued_binary, upgraded_daemon_marker, "upgraded-install", "daemon")
        install_pi_fixture(provider_bin, gate, waiter_directory, lease_nonce)

        brief = base / "brief.md"
        brief.write_text("Reply with the requested acceptance sentence.\n")
        brief.chmod(0o600)
        env.update({
            "HOME": str(home),
            "PATH": str(provider_bin) + os.pathsep + str(old_install) + os.pathsep + str(old_libexec) + os.pathsep + "/usr/bin:/bin",
            "XDG_CONFIG_HOME": str(home / ".config"),
            "XDG_DATA_HOME": str(home / ".local" / "share"),
            "LANG": "C.UTF-8",
            "LC_ALL": "C.UTF-8",
        })

        if args.scenario == DEFAULT_STOP_SCENARIO:
            run_receipt.phase("default_stop")
            default_stop = run_default_stop_acceptance(
                old_install / "delegate", old_libexec / "pueue", state, workspace,
                brief, gate, waiter_directory, private_config, env, response_directory,
                provider_bin / "pi", lease_nonce, run_receipt, attempted_task_ids,
                capture_root=base_token,
            )
            wrapper_calls = (
                wrapper_call_records(base / "old-client-starts-calls", "client", "old")
                + wrapper_call_records(base / "old-daemon-starts-calls", "daemon", "old")
            )
            require_no_startup_control_calls(wrapper_calls)
            if not old_daemon_marker.is_file() or old_daemon_marker.read_text().splitlines() != ["started"]:
                raise RuntimeError("default-stop scenario did not start exactly one private daemon")
            if upgraded_daemon_marker.exists():
                raise RuntimeError("default-stop scenario started an unexpected upgraded daemon")
            run_receipt.add_details(
                default_stop=default_stop,
                pueue_wrapper_calls=compact_sequence_evidence(wrapper_calls),
                checks=[
                    "a user cancellation was issued only after the exact fake provider lease was live",
                    "a sub-second budget termination was issued only after the exact fake provider lease was live",
                    "both tasks reached canonical worker Wait and empty native-scope closure",
                    "the private supervisor shutdown was verified after both closures",
                ],
            )
            run_receipt.stage("default-stop-proved")
            success = True
            return

        if args.parallel_tasks > 1:
            # PATH is intentionally retained by the control environment;
            # arbitrary ambient keys are correctly filtered out by Delegate.
            env["PATH"] += os.pathsep + os.pathsep.join(["/delegate-acceptance-unused"] * 256)

        run_receipt.phase("initial_dispatch")
        task_id = uuid.uuid4().hex
        attempted_task_ids.append(task_id)
        dispatch = run_json(old_install / "delegate", env, [
            "--root", str(state), "dispatch", "--provider", "pi:json",
            "--brief", str(brief), "--cwd", str(workspace),
            "--id", task_id, "--permission", "read-only", "--budget", TASK_BUDGET,
            "--json",
        ], receipt=response_directory / "dispatch-initial.json", phase="ordinary-dispatch",
                           capture_root=base_token)
        if dispatch.get("task_id") != task_id or (
            "task_record" in dispatch and dispatch.get("task_record") != "created"
        ):
            raise RuntimeError(
                "dispatch did not durably create the requested task: response=%r expected_id=%s"
                % (dispatch, task_id)
            )
        if dispatch.get("admission") != "admitted":
            raise RuntimeError("dispatch did not admit the bounded task")

        first_status = run_json(old_install / "delegate", env, [
            "--root", str(state), "status", task_id, "--json",
        ], receipt=response_directory / "status-initial.json", phase="ordinary-observation",
                           capture_root=base_token)
        if first_status.get("task_id") != task_id:
            raise RuntimeError("status did not observe the dispatched task")

        collected = run_json(old_install / "delegate", env, [
            "--root", str(state), "collect", task_id, "--watch", COLLECT_WATCH, "--json",
        ], timeout=COLLECT_TIMEOUT_SECONDS, receipt=response_directory / "collect-initial.json",
                           phase="ordinary-collection", capture_root=base_token)
        if collected.get("outcome", {}).get("verdict") != "committed":
            raise RuntimeError("delegate did not publish the successful fixture result")
        run_receipt.stage("initial-private-task-completed")

        task_dir = state / "tasks" / task_id
        if not private_config.is_file():
            raise RuntimeError("delegate did not create its private supervisor config")
        first_meta = json.loads((task_dir / "meta.json").read_text())
        if legacy_mode:
            initial_daemon = old_libexec / "pueued"
            if str(old_libexec / "pueue") not in json.dumps(first_meta) or str(initial_daemon) not in json.dumps(first_meta):
                raise RuntimeError("legacy task did not bind the supervisor pair shipped in private libexec")
        else:
            initial_pair_directory = assert_state_supervisor_pair(state, old_libexec)
            initial_daemon = initial_pair_directory / "pueued"
            if str(initial_pair_directory / "pueue") not in json.dumps(first_meta) or str(initial_daemon) not in json.dumps(first_meta):
                raise RuntimeError("task did not bind the verified state-root supervisor pair")
        outcome = json.loads((task_dir / "outcome.json").read_text())
        if outcome.get("verdict") != "committed":
            raise RuntimeError("durable outcome does not confirm success")

        identity_path = state / ".supervisor" / "daemon.identity.json"
        old_identity = json.loads(identity_path.read_text())
        upgraded_daemon = upgraded_libexec / "pueued"
        upgraded_daemon_digest = hashlib.sha256(upgraded_daemon.read_bytes()).hexdigest()
        if old_identity.get("executable") != str(initial_daemon):
            raise RuntimeError("initial launch provenance does not name its selected private daemon")
        old_digest = hashlib.sha256((old_libexec / "pueued").read_bytes()).hexdigest()
        if old_identity.get("sha256") != old_digest:
            raise RuntimeError("initial launch provenance has the wrong daemon hash")
        if old_identity.get("executable") == str(upgraded_daemon) or old_identity.get("sha256") == upgraded_daemon_digest:
            raise RuntimeError("acceptance layouts do not reproduce daemon executable hash drift")
        if not old_daemon_marker.is_file() or old_daemon_marker.read_text().splitlines() != ["started"]:
            raise RuntimeError("initial CLI did not start exactly one old-layout daemon")

        primary_client = old_libexec / "pueue" if legacy_mode else initial_pair_directory / "pueue"
        packaged_evidence = verify_packaged_default_path(
            ownership,
            base,
            {
                "delegate": upgraded_delegate_binary,
                "delegate-run": upgraded_runner_binary,
                "pueue": pueue_binary,
                "pueued": pueued_binary,
            },
            provider_bin,
            brief,
            primary_client,
            private_config,
            env,
            response_directory,
            supervisor_cleanups,
            run_receipt,
            managed_state_roots,
            capture_root=base_token,
        )

        inspection_group = "delegation-inspection-" + dispatch["root_id"]
        inspection_prefix = inspection_group + "-"
        models_args = [
            "--root", str(state), "models", "--provider", "pi:json",
            "--cwd", str(workspace), "--json",
        ]
        if args.runtime_only:
            if (state / "inspection-group").exists():
                raise RuntimeError("runtime-only dispatch created a legacy group journal")
        else:
            initial_models = run_json(old_install / "delegate", env, models_args,
                                      receipt=response_directory / "models-initial.json",
                                      phase="inspection-worker", capture_root=base_token)
            if initial_models.get("status") != "available" or [
                model.get("id") for model in initial_models.get("models", [])
            ] != ["openai/gpt-4.1"]:
                raise RuntimeError("legacy CLI could not complete its initial model inspection")
        if not legacy_mode:
            release_gate = LockedReleaseGate.acquire(gate)
            default_group = pueue_snapshot(
                old_libexec / "pueue", private_config, env,
                phase="supervisor-observation", capture_root=base_token).get("groups", {}).get("default", {})
            if default_group.get("parallel_tasks") != 0:
                raise RuntimeError("private ordinary supervisor did not enable unlimited parallelism")
            acceptance_evidence = {}
            upgraded_tasks, status_bytes = verify_current_installation_upgrade(
                upgraded_install / "delegate", state, workspace, brief, release_gate, waiter_directory, env,
                base / "old", old_layout_owned, initial_pair_directory / "pueue", private_config,
                args.parallel_tasks,
                dispatch_workers, response_directory, acceptance_evidence,
                lease_nonce=lease_nonce,
                client_call_log=base / "old-client-starts-calls",
                attempted_task_ids=attempted_task_ids,
                capture_root=base_token,
                run_receipt=run_receipt,
            )
            # The upgrade probe has already removed every old-layout token;
            # never present those consumed handles to the final rollback.
            old_layout_owned = ()
            shutdown_private_supervisor(
                initial_pair_directory / "pueue", private_config, env,
                state=state, task_ids=tuple(attempted_task_ids), capture_root=base_token)
            wrapper_calls = (
                wrapper_call_records(base / "old-client-starts-calls", "client", "old")
                + wrapper_call_records(base / "old-daemon-starts-calls", "daemon", "old")
                + wrapper_call_records(base / "upgraded-client-starts-calls", "client", "upgraded")
                + wrapper_call_records(base / "upgraded-daemon-starts-calls", "daemon", "upgraded")
            )
            require_no_startup_control_calls(wrapper_calls)
            run_receipt.add_details(**{
                "task_identities": compact_sequence_evidence([task_id, *upgraded_tasks]),
                "concurrent_running_tasks": args.parallel_tasks,
                "dispatch_workers": dispatch_workers,
                "concurrent_status_bytes": status_bytes,
                "capacity_proof": acceptance_evidence["capacity_proof"],
                "dispatch_batch": acceptance_evidence["dispatch_batch"],
                "status_coalescing": acceptance_evidence["status_coalescing"],
                "capacity_proof_receipt": "capacity-proof.json",
                "dispatch_batch_receipt": "dispatch-batch.json",
                "status_coalescing_receipt": "status-coalescing.json",
                "pueue_client_commands": {
                    "initial": wrapper_command_counts(base / "old-client-starts-calls"),
                    "upgraded": wrapper_command_counts(base / "upgraded-client-starts-calls"),
                },
                "pueue_wrapper_calls": compact_sequence_evidence(wrapper_calls),
                "packaged_default_path": packaged_evidence,
                "checks": [
                    "the CLI completed dispatch/status/collect against an isolated private supervisor",
                    "the adjacent release package completed default-path dispatch status collect and shutdown",
                    "private ordinary admission enabled unlimited parallelism",
                    ("runtime-only root required no legacy inspection group journal" if args.runtime_only else
                     "private model discovery completed without changing ordinary supervisor ownership"),
                    "task completion survived removal of the original package layout",
                    "dispatch restarted the private supervisor after package removal",
                    "all requested fixture tasks retained live provider leases and lifetime-owner locks with bound native-scope registrations while Running",
                    "dispatch caller concurrency and burst metrics were recorded separately from capacity proof",
                    "sixteen concurrent public status calls coalesced into fewer authoritative queue snapshots",
                ],
            })
            run_receipt.stage("all-current-installation-proofs-complete")
            success = True
            return
        # Ordinary dispatch deliberately leaves the private default group
        # unlimited. Pause that group for this migration fixture so the
        # queued-runner repair scenario remains deterministic; unlike changing
        # parallelism, pausing is not undone by the next dispatch.
        old_pueue = old_libexec / "pueue"
        pueue_command(old_pueue, private_config, env, ["pause", "--group", "default"],
                      phase="supervisor-control", capture_root=base_token)
        paused_snapshot = pueue_snapshot(old_pueue, private_config, env,
                                         phase="supervisor-observation", capture_root=base_token)
        if paused_snapshot.get("groups", {}).get("default", {}).get("status") != "Paused":
            raise RuntimeError("private default group did not pause for the migration fixture")

        queued_task_id = uuid.uuid4().hex
        attempted_task_ids.append(queued_task_id)
        queued_dispatch = run_json(old_install / "delegate", env, [
            "--root", str(state), "dispatch", "--provider", "pi:json",
            "--brief", str(brief), "--cwd", str(workspace),
            "--id", queued_task_id, "--permission", "read-only", "--budget", TASK_BUDGET,
            "--json",
        ], receipt=response_directory / "dispatch-queued.json", phase="ordinary-dispatch",
                           capture_root=base_token)
        if queued_dispatch.get("task_id") != queued_task_id or queued_dispatch.get("admission") != "admitted":
            raise RuntimeError("legacy CLI did not admit the task that should remain queued")
        queued_label = "delegate:" + dispatch["root_id"] + ":" + queued_task_id
        queued_row = task_row_by_label(
            pueue_snapshot(old_pueue, private_config, env, phase="supervisor-observation",
                           capture_root=base_token), queued_label)
        if queued_row is None or pueue_state(queued_row) != "Queued":
            raise RuntimeError("legacy task did not remain queued in the paused default group")
        old_queued_command = queued_row.get("original_command", "")
        old_runner_path = str(old_install / "delegate-run")
        initial_runner_digest = hashlib.sha256(runner_binary.read_bytes()).hexdigest()
        stable_runner_path = str(state / ".supervisor" / ("delegate-run-" + initial_runner_digest))
        if legacy_mode and old_runner_path not in old_queued_command:
            raise RuntimeError("legacy queued command does not reference the removable install path")
        if not legacy_mode and stable_runner_path not in old_queued_command:
            raise RuntimeError("current CLI did not submit the durable state-root runner path")
        previous_inspection_labels = {
            row.get("label")
            for row in pueue_snapshot(
                old_libexec / "pueue", private_config, env,
                phase="inspection-observation", capture_root=base_token).get("tasks", {}).values()
            if row.get("group") == inspection_group and (row.get("label") or "").startswith(inspection_prefix)
        }

        # Queue an old-release inspection worker behind its private group
        # blocker. Start this after the ordinary dispatch has been admitted so
        # the two foreground CLI operations never race over supervisor calls.
        inspection_blocker = base / "inspection-queue-blocker"
        inspection_blocker.write_text(
            "#!/bin/sh\nset -eu\nwhile [ ! -e " + shlex.quote(str(gate)) + " ]; do sleep 0.05; done\n"
        )
        inspection_blocker.chmod(0o700)
        pueue_command(old_libexec / "pueue", private_config, env, [
            "add", "--escape", "--group", inspection_group, "--label",
            "inspection-upgrade-blocker", "--print-task-id", "--", str(inspection_blocker),
        ], phase="inspection-worker", capture_root=base_token)
        wait_for_pueue_state(
            old_libexec / "pueue", private_config, env, "inspection-upgrade-blocker", "Running",
            phase="inspection-observation", capture_root=base_token,
        )
        inspection_process = subprocess.Popen(
            [str(old_install / "delegate"), *models_args],
            env=phase_environment(env, "inspection-worker"), stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True,
        )
        inspection_row = wait_for_queued_label_prefix(
            old_libexec / "pueue", private_config, env, inspection_group,
            inspection_prefix, previous_inspection_labels, inspection_process,
            phase="inspection-observation", capture_root=base_token,
        )
        inspection_label = inspection_row["label"]
        inspection_task_id = inspection_label[len(inspection_prefix):]
        if len(inspection_task_id) != 32 or any(char not in "0123456789abcdef" for char in inspection_task_id):
            raise RuntimeError("queued inspection label does not contain a valid task identity")

        # Let the upgraded public CLI recover the queued task while the legacy
        # install is still present, so it can prove the saved runner is managed
        # and move the command to the new immutable state-root runner.
        env["PATH"] = (
            str(provider_bin) + os.pathsep + str(upgraded_install) + os.pathsep
            + str(upgraded_libexec) + os.pathsep + str(old_install) + os.pathsep
            + str(old_libexec) + os.pathsep + "/usr/bin:/bin"
        )
        repaired_status = run_json(upgraded_install / "delegate", env, [
            "--root", str(state), "status", queued_task_id, "--json",
        ], receipt=response_directory / "status-queued-after-upgrade.json",
                           phase="managed-runner-observation", capture_root=base_token)
        if repaired_status.get("task_id") != queued_task_id:
            raise RuntimeError("upgraded status did not observe the queued legacy task")
        migrated_row = task_row_by_label(
            pueue_snapshot(upgraded_libexec / "pueue", private_config, env,
                           phase="supervisor-observation", capture_root=base_token), queued_label)
        if migrated_row is None or pueue_state(migrated_row) != "Queued":
            raise RuntimeError("upgraded CLI lost the legacy queued task during status recovery")
        migrated_command = migrated_row.get("original_command", "")
        upgraded_digest = hashlib.sha256(upgraded_runner_binary.read_bytes()).hexdigest()
        state_runner_path = str(state / ".supervisor" / ("delegate-run-" + upgraded_digest))
        if state_runner_path not in migrated_command:
            queued_meta = json.loads((state / "tasks" / queued_task_id / "meta.json").read_text())
            raise RuntimeError(
                "upgraded status did not migrate the queue to its immutable state-root runner: "
                "status=%r runner=%r ownership=%r expected=%r command=%r"
                % (
                    repaired_status,
                    queued_meta.get("runner_executable"),
                    queued_meta.get("runner_ownership"),
                    state_runner_path,
                    migrated_command,
                )
            )
        if old_runner_path in migrated_command:
            raise RuntimeError("migrated queue command still references the old installation")

        migrated_inspection = task_row_by_label(
            pueue_snapshot(upgraded_libexec / "pueue", private_config, env,
                           phase="inspection-observation", capture_root=base_token), inspection_label,
        )
        if migrated_inspection is None or pueue_state(migrated_inspection) != "Queued":
            raise RuntimeError("upgraded status lost the queued inspection worker")
        migrated_inspection_command = migrated_inspection.get("original_command", "")
        inspection_request_path = state / "inspections" / inspection_task_id / "request.json"
        inspection_request = json.loads(inspection_request_path.read_text())
        upgrade_record_path = state / "inspections" / inspection_task_id / "worker-upgrade.json"
        if legacy_mode:
            if old_runner_path not in migrated_inspection_command or state_runner_path in migrated_inspection_command:
                raise RuntimeError(
                    "upgraded status changed the runner for a legacy inspection with unknown ownership: "
                    + migrated_inspection_command
                )
            if inspection_request.get("binding", {}).get("runner_ownership"):
                raise RuntimeError("legacy inspection unexpectedly recorded runner ownership")
            if upgrade_record_path.exists():
                raise RuntimeError("ambiguous legacy inspection was incorrectly authorized for worker migration")
        else:
            if state_runner_path not in migrated_inspection_command or inspection_request.get("binding", {}).get("runner_ownership") != "managed":
                raise RuntimeError("managed inspection did not retain its recorded state-root runner")
        assert_state_supervisor_pair(state, upgraded_libexec)

        # Keep the old Pueue client layout until the running models command
        # finishes polling. For legacy records, retain only its pinned worker;
        # for new records, the state-root copy is sufficient.
        if legacy_mode:
            old_install_token = next(
                entry for entry in old_layout_owned if entry.directory == old_install)
            remove_owned_directory(old_install_token)
            old_layout_owned = tuple(
                entry for entry in old_layout_owned if entry is not old_install_token)
            if (old_install / "delegate").exists():
                raise RuntimeError("old CLI remained after simulated package cleanup")

        upgraded_pueue = upgraded_libexec / "pueue"
        migrated_row = task_row_by_label(
            pueue_snapshot(upgraded_pueue, private_config, env,
                           phase="supervisor-observation", capture_root=base_token), queued_label)
        if migrated_row is None or pueue_state(migrated_row) != "Queued":
            raise RuntimeError("upgraded CLI lost the legacy queued task")
        migrated_command = migrated_row.get("original_command", "")
        if state_runner_path not in migrated_command:
            raise RuntimeError("upgraded CLI did not retain the migrated content-addressed runner")
        if old_runner_path in migrated_command:
            raise RuntimeError("migrated queue command still references the removed install")

        pueue_command(upgraded_pueue, private_config, env, ["start", "--group", "default"],
                      phase="supervisor-control", capture_root=base_token)
        gate.touch(mode=0o600)
        queued_collected = run_json(upgraded_install / "delegate", env, [
            "--root", str(state), "collect", queued_task_id, "--watch", COLLECT_WATCH, "--json",
        ], timeout=COLLECT_TIMEOUT_SECONDS, receipt=response_directory / "collect-queued.json",
                           phase="managed-runner-collection", capture_root=base_token)
        if queued_collected.get("outcome", {}).get("verdict") != "committed":
            raise RuntimeError("upgraded CLI did not run and collect the migrated legacy queued task")
        upgraded_status = run_json(upgraded_install / "delegate", env, [
            "--root", str(state), "status", task_id, "--json",
        ], receipt=response_directory / "status-upgraded.json", phase="ordinary-observation",
                           capture_root=base_token)
        if upgraded_status.get("task_id") != task_id:
            raise RuntimeError("upgraded CLI could not recover the earlier task supervisor binding")
        if inspection_process is None:
            raise RuntimeError("queued inspection process was not started")
        process = inspection_process
        inspection_stdout, inspection_stderr, reaped, reap_error = reap_captured_process(process, 45)
        if reaped:
            inspection_process = None
        if reap_error is not None:
            raise reap_error
        if process.returncode != 0:
            raise RuntimeError(
                "legacy CLI did not finish the queued inspection using its preserved pinned worker: "
                "exit=%d stdout=%r stderr=%r"
                % (process.returncode, output_tail(inspection_stdout), output_tail(inspection_stderr))
            )
        try:
            migrated_models = json.loads(inspection_stdout)
        except json.JSONDecodeError as error:
            raise RuntimeError("migrated models command returned invalid JSON: " + str(error)) from error
        if migrated_models.get("status") != "available" or [
            model.get("id") for model in migrated_models.get("models", [])
        ] != ["openai/gpt-4.1"]:
            raise RuntimeError("legacy pinned worker did not complete the queued inspection")

        # Once the worker has finished, remove the remaining old package and
        # prove the upgraded CLI uses only its state-root supervisor pair.
        if (base / "old").exists() or (base / "old").is_symlink():
            rollback_owned_directories(old_layout_owned)
            old_layout_owned = ()
        if old_install.exists() or old_libexec.exists():
            raise RuntimeError("old package paths remained after simulated package cleanup")
        state_root_models = run_json(upgraded_install / "delegate", env, models_args,
                                     receipt=response_directory / "models-upgraded.json",
                                     phase="inspection-worker", capture_root=base_token)
        if state_root_models.get("status") != "available" or [
            model.get("id") for model in state_root_models.get("models", [])
        ] != ["openai/gpt-4.1"]:
            raise RuntimeError("upgraded CLI could not inspect models after old package removal")

        upgraded_task_id = uuid.uuid4().hex
        attempted_task_ids.append(upgraded_task_id)
        env["PATH"] = str(provider_bin) + os.pathsep + str(upgraded_install) + os.pathsep + "/usr/bin:/bin"
        upgraded_dispatch = run_json(upgraded_install / "delegate", env, [
            "--root", str(state), "dispatch", "--provider", "pi:json",
            "--brief", str(brief), "--cwd", str(workspace),
            "--id", upgraded_task_id, "--permission", "read-only", "--budget", TASK_BUDGET,
            "--json",
        ], receipt=response_directory / "dispatch-upgraded.json", phase="ordinary-dispatch",
                           capture_root=base_token)
        if upgraded_dispatch.get("task_id") != upgraded_task_id or upgraded_dispatch.get("admission") != "admitted":
            raise RuntimeError("upgraded CLI did not dispatch through the existing private supervisor")
        upgraded_snapshot = pueue_snapshot(upgraded_libexec / "pueue", private_config, env,
                                            phase="supervisor-observation", capture_root=base_token)
        default_group = upgraded_snapshot.get("groups", {}).get("default", {})
        if default_group.get("parallel_tasks") != 0:
            raise RuntimeError("private ordinary supervisor did not enable unlimited parallelism")
        upgraded_meta = json.loads((state / "tasks" / upgraded_task_id / "meta.json").read_text())
        if upgraded_meta.get("runner_executable") != state_runner_path:
            raise RuntimeError("upgraded task metadata did not retain the state-root runner")
        upgraded_collected = run_json(upgraded_install / "delegate", env, [
            "--root", str(state), "collect", upgraded_task_id, "--watch", COLLECT_WATCH, "--json",
        ], timeout=COLLECT_TIMEOUT_SECONDS, receipt=response_directory / "collect-upgraded.json",
                           phase="ordinary-collection", capture_root=base_token)
        if upgraded_collected.get("outcome", {}).get("verdict") != "committed":
            raise RuntimeError("upgraded CLI did not collect the successful second task")
        if upgraded_daemon_marker.exists():
            raise RuntimeError("compatible live supervisor was restarted during ordinary upgraded dispatch")
        if old_daemon_marker.read_text().splitlines() != ["started"]:
            raise RuntimeError("ordinary upgraded dispatch restarted the original private daemon")
        adopted_identity = json.loads(identity_path.read_text())
        if adopted_identity.get("executable") != str(initial_daemon) or adopted_identity.get("sha256") != old_digest:
            raise RuntimeError("ordinary upgrade adoption rewrote the live daemon identity")

        # A stopped endpoint is the only ordinary path that starts the current
        # bundled pair. This proves the successor is launched only after the
        # old daemon is actually unavailable.
        shutdown_private_supervisor(
            upgraded_libexec / "pueue", private_config, env,
            state=state, task_ids=tuple(attempted_task_ids), capture_root=base_token)
        restarted_task_id = uuid.uuid4().hex
        attempted_task_ids.append(restarted_task_id)
        restarted_dispatch = run_json(upgraded_install / "delegate", env, [
            "--root", str(state), "dispatch", "--provider", "pi:json",
            "--brief", str(brief), "--cwd", str(workspace),
            "--id", restarted_task_id, "--permission", "read-only", "--budget", TASK_BUDGET,
            "--json",
        ], receipt=response_directory / "dispatch-restarted.json", phase="restart-dispatch",
                           capture_root=base_token)
        if restarted_dispatch.get("task_id") != restarted_task_id or restarted_dispatch.get("admission") != "admitted":
            raise RuntimeError("upgraded CLI did not restart the private supervisor after it stopped")
        restarted_collected = run_json(upgraded_install / "delegate", env, [
            "--root", str(state), "collect", restarted_task_id, "--watch", COLLECT_WATCH, "--json",
        ], timeout=COLLECT_TIMEOUT_SECONDS, receipt=response_directory / "collect-restarted.json",
                           phase="restart-collection", capture_root=base_token)
        if restarted_collected.get("outcome", {}).get("verdict") != "committed":
            raise RuntimeError("upgraded CLI did not collect the task after restarting its supervisor")
        if not upgraded_daemon_marker.is_file() or upgraded_daemon_marker.read_text().splitlines() != ["started"]:
            raise RuntimeError("stopped supervisor did not start exactly one upgraded daemon")
        upgraded_identity = json.loads(identity_path.read_text())
        if upgraded_identity.get("executable") != str(upgraded_daemon) or upgraded_identity.get("sha256") != upgraded_daemon_digest:
            raise RuntimeError("restarted supervisor did not publish the upgraded daemon identity")

        shutdown_private_supervisor(
            upgraded_libexec / "pueue", private_config, env,
            state=state, task_ids=tuple(attempted_task_ids), capture_root=base_token)
        wrapper_calls = (
            wrapper_call_records(base / "old-client-starts-calls", "client", "old")
            + wrapper_call_records(base / "old-daemon-starts-calls", "daemon", "old")
            + wrapper_call_records(base / "upgraded-client-starts-calls", "client", "upgraded")
            + wrapper_call_records(base / "upgraded-daemon-starts-calls", "daemon", "upgraded")
        )
        require_no_startup_control_calls(wrapper_calls)
        run_receipt.add_details(**{
            "task_identities": compact_sequence_evidence(
                [task_id, queued_task_id, upgraded_task_id, restarted_task_id]
            ),
            "inspection_identity": compact_sequence_evidence([inspection_task_id]),
            "pueue_wrapper_calls": compact_sequence_evidence(wrapper_calls),
            "packaged_default_path": packaged_evidence,
            "checks": [
                "two installed CLI layouts resolved separate private Pueue pairs",
                "the initial CLI completed dispatch/status/collect against an isolated private supervisor",
                "the adjacent release package completed default-path dispatch status collect and shutdown",
                queued_runner_check,
                "the upgraded status repaired the queued runner before old package files were removed",
                "the upgraded CLI completed the migrated queued task after old package paths were removed",
                inspection_runner_check,
                "the upgraded CLI completed another bounded dispatch and collected its result",
                "adoption preserved old daemon launch provenance and did not start a second daemon",
                "a stopped endpoint restarted only the current packaged daemon and republished its identity",
            ],
        })
        run_receipt.stage("all-migration-proofs-complete")
        success = True
    except BaseException as error:
        failure = error
        run_receipt.add_details(failure_phase=run_receipt.current_phase)
        raise
    finally:
        cleanup_errors = []
        primary_closure_ready = True
        run_receipt.phase("cleanup")
        if release_gate is not None:
            try:
                release_gate.release()
            except BaseException as cleanup_error:
                cleanup_errors.append(RuntimeError(
                    "could not release the locked fixture gate: " + str(cleanup_error)
                ))
        elif gate is not None and base is not None:
            try:
                gate.touch(mode=0o600, exist_ok=True)
            except BaseException as cleanup_error:
                cleanup_errors.append(RuntimeError(
                    "could not release the fixture gate: " + str(cleanup_error)
                ))
        if (not success and private_config is not None and private_config.is_file()
                and managed_state_roots):
            try:
                await_managed_lifetime_closure(
                    managed_state_roots[0][0], tuple(managed_state_roots[0][1]), timeout=45,
                )
            except BaseException as cleanup_error:
                primary_closure_ready = False
                cleanup_errors.append(RuntimeError(
                    "managed workers did not close after fixture release: "
                    + str(cleanup_error)
                ))
        if inspection_process is not None:
            _, _, reaped, process_cleanup_error = reap_captured_process(inspection_process, 45)
            if reaped:
                inspection_process = None
            if process_cleanup_error is not None:
                cleanup_errors.append(process_cleanup_error)
        for pending in tuple(supervisor_cleanups):
            if pending["config"].is_file():
                try:
                    shutdown_private_supervisor(
                        pending["client"], pending["config"], pending["env"],
                        state=pending["state"],
                        task_ids=tuple(pending.get("task_ids", ())),
                        capture_root=base_token,
                    )
                    supervisor_cleanups.remove(pending)
                except BaseException as cleanup_error:
                    cleanup_errors.append(RuntimeError(
                        "packaged default supervisor shutdown remained unverified: "
                        + str(cleanup_error)
                    ))
        if ((not success or cleanup_errors) and primary_closure_ready
                and private_config is not None and private_config.is_file()):
            client = next((candidate for candidate in
                           ((upgraded_libexec / "pueue" if upgraded_libexec is not None else None),
                            (old_libexec / "pueue" if old_libexec is not None else None),
                            args.pueue)
                           if candidate is not None and candidate.is_file()), None)
            if client is not None:
                try:
                    shutdown_private_supervisor(
                        client, private_config, env, state=state,
                        task_ids=tuple(attempted_task_ids), capture_root=base_token)
                except BaseException as cleanup_error:
                    cleanup_errors.append(RuntimeError(
                        "primary private supervisor shutdown remained unverified: "
                        + str(cleanup_error)
                    ))
                    print("Retained private acceptance state after unproven shutdown: "
                          + str(cleanup_error), flush=True)
        if inspection_process is not None:
            _, _, reaped, retry_error = reap_captured_process(inspection_process, 5)
            if reaped:
                inspection_process = None
            if retry_error is not None:
                cleanup_errors.append(retry_error)
        if inspection_process is not None:
            cleanup_errors.append(RuntimeError("fixture process remained live after bounded cleanup"))
        if cleanup_errors:
            success = False
            print("Retained private acceptance state after fixture process cleanup failure: "
                  + "; ".join(str(error) for error in cleanup_errors), flush=True)
        root_cleanup = {"status": "not-created", "verified": base is None}
        if success and failure is None and not cleanup_errors and base is not None:
            try:
                main_owned = ownership.transfer()
                require(any(entry is base_token for entry in main_owned),
                        "private acceptance root token was lost from ownership sequence")
                owned_directories = tuple(
                    [base_token, *old_layout_owned]
                    + [entry for entry in main_owned if entry is not base_token]
                )
                remove_private_acceptance_root(
                    base_token, owned_directories,
                    managed_state_roots=tuple(managed_state_roots),
                )
                root_cleanup = {"status": "removed", "verified": True}
            except BaseException as cleanup_error:
                cleanup_errors.append(RuntimeError(
                    "private acceptance root removal failed: " + str(cleanup_error)
                ))
                success = False
        if base is not None and root_cleanup["status"] != "removed":
            retained = base.is_dir() and not base.is_symlink()
            root_cleanup = {"status": "retained" if retained else "missing", "verified": retained}
            if not retained:
                cleanup_errors.append(RuntimeError(
                    "private acceptance root was not retained after failure"
                ))
        cleanup_evidence = {
            "status": (
                "failed" if cleanup_errors else
                "passed" if root_cleanup["status"] == "removed" else
                "not-required" if base is None else
                "retained"
            ),
            "private_root": root_cleanup,
            "pending_supervisor_count": len(supervisor_cleanups),
        }
        cleanup_failure = None
        if cleanup_errors and failure is None:
            cleanup_failure = RuntimeError(
                "private acceptance cleanup failed: "
                + "; ".join(str(error) for error in cleanup_errors)
            )
        receipt_error = failure or cleanup_failure
        result = (
            "PASS" if success and receipt_error is None and not cleanup_errors else
            "BLOCKED" if isinstance(receipt_error, AcceptanceBlocked) and not cleanup_errors else
            "FAIL"
        )
        run_receipt.write(result, cleanup_evidence, receipt_error)
        if result == "PASS":
            print("PASS private CLI acceptance: " + str(args.output))
        elif base is not None and root_cleanup["status"] == "retained":
            print("Retained private acceptance state for diagnosis: " + str(base))
        if cleanup_failure is not None:
            raise cleanup_failure


if __name__ == "__main__":
    main()
