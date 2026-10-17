"""Private CLI supervisor boundary."""

from pathlib import Path
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.lifetime.closure import validate_managed_lifetime_closure
from scripts.delegate_acceptance.private_cli.capture import run_captured_text_command
from scripts.delegate_acceptance.private_cli.environment import phase_environment
from scripts.delegate_acceptance.private_cli.queue_policy import pueue_state
import errno
import json
import os
import socket
import stat
import time


def pueue_snapshot(client, config, env, metrics=None, phase=None, *, capture_root):
    command_env = phase_environment(env, phase) if phase is not None else env
    result = run_captured_text_command(
        [str(client), "--config", str(config), "status", "--json"], command_env, 10, capture_root=capture_root
    )
    if result.returncode != 0:
        raise RuntimeError("isolated pueue status failed: " + result.stderr[-1000:])
    if metrics is not None:
        metrics["status_bytes"] = len(result.stdout.encode("utf-8"))
    return json.loads(result.stdout)


def wait_for_pueue_state(client, config, env, label, expected, phase="supervisor-observation", *, capture_root):
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


def wait_for_queued_label_prefix(
    client, config, env, group, prefix, excluded_labels, process, phase="inspection-observation", *, capture_root
):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(
                "models command exited before its inspection worker queued: exit=%d" % process.returncode
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
        [str(client), "--config", str(config), *args], command_env, timeout, capture_root=capture_root
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
    if config.name != "pueue.yml" or config.parent.name != ".supervisor" or config.resolve(strict=True) != config:
        raise RuntimeError("unsafe private supervisor shutdown config")
    runtime = config.parent / "run"
    if runtime.resolve(strict=True) != runtime:
        raise RuntimeError("unsafe private supervisor runtime directory")
    # These are the exact quoted fields written by privateConfigYAML, not a
    # general YAML parser or a fallback to the user's ambient Pueue config.
    lines = config.read_text(encoding="utf-8").splitlines()
    for key, path in (("runtime_directory", runtime), ("pid_path", runtime / "pueue.pid")):
        prefix = "  " + key + ": "
        values = [line[len(prefix) :] for line in lines if line.startswith(prefix)]
        if len(values) != 1 or json.loads(values[0]) != str(path):
            raise RuntimeError("private shutdown config does not bind its owned " + key)
    prefix = "  unix_socket_path: "
    values = [line[len(prefix) :] for line in lines if line.startswith(prefix)]
    if len(values) != 1:
        raise RuntimeError("private shutdown config does not bind its owned unix_socket_path")
    endpoint_value = json.loads(values[0])
    if not isinstance(endpoint_value, str) or "\0" in endpoint_value or not Path(endpoint_value).is_absolute():
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
    if (
        len(raw) > 128
        or not value.isdigit()
        or value != str(int(value)).encode()
        or int(value) <= 1
        or int(value) == os.getpid()
    ):
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
                "managed lifetime closure remained incomplete before supervisor shutdown: " + str(last_error)
            ) from last_error
        time.sleep(min(0.05, max(0.0, deadline - time.monotonic())))


def shutdown_private_supervisor(client, config, env, timeout=30, *, state, task_ids=(), capture_root):
    """Authorize lifetime closure, then observe PID and endpoint disappearance."""
    authorize_managed_lifetime_closure(state, task_ids)
    deadline = time.monotonic() + timeout
    pid, endpoint, socket_identity = private_shutdown_evidence(config)
    if private_daemon_process_gone(pid, deadline, capture_root=capture_root):
        raise RuntimeError("private supervisor PID disappeared before shutdown")
    pueue_command(
        client,
        config,
        env,
        ["shutdown"],
        timeout=shutdown_remaining(deadline),
        phase="supervisor-shutdown",
        capture_root=capture_root,
    )
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
