"""Private CLI supervisor boundary."""

from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from io import BufferedRandom
from pathlib import Path
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.lifetime.closure import validate_managed_lifetime_closure
from scripts.delegate_acceptance.private_cli.capture import run_captured_text_command
from scripts.delegate_acceptance.private_cli.configuration import DEFAULT_DISPATCH_WORKERS
from scripts.delegate_acceptance.private_cli.environment import phase_environment
from scripts.delegate_acceptance.private_cli.leases import private_fixture_transition_until
from scripts.delegate_acceptance.private_cli.noexec_state import custody_release_pending
from scripts.delegate_acceptance.private_cli.noexec_state import describe_pending_custody
from scripts.delegate_acceptance.private_cli.queue_policy import pueue_state
import errno
import json
import os
import socket
import stat
import threading
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
    return private_supervisor_pid(runtime / "pueue.pid"), endpoint, private_socket_identity(endpoint)


def private_supervisor_pid(pid_path):
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
    return int(value)


def private_socket_identity(endpoint):
    info = endpoint.lstat()
    if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
        raise RuntimeError("unsafe private supervisor socket")
    return info.st_dev, info.st_ino


@dataclass(frozen=True)
class PredecessorShutdownTarget:
    """Shutdown authority captured for one generation before its config is retargeted.

    The ledger entry that carries this target owns backup_handle; the finalizer or
    the verified successor release closes it. The handle pins the exact backup
    file object, so a replaced path cannot inherit the captured authority.
    """

    config: Path
    state: Path
    backup: Path
    backup_handle: BufferedRandom
    backup_content: bytes
    pid: int
    endpoint: Path
    socket_identity: tuple[int, int]


def capture_predecessor_shutdown_target(config, state, backup):
    """Bind the canonical generation and an exclusive owned config backup.

    The caller holds private_fixture_transition, so the canonical config cannot
    change between the ordinary evidence gate and the backup copy.
    """
    config = Path(config)
    state = Path(state)
    if config != state / ".supervisor" / "pueue.yml":
        raise RuntimeError("predecessor shutdown config is not its state root's canonical config")
    content = config.read_bytes()
    pid, endpoint, socket_identity = private_shutdown_evidence(config)
    if config.read_bytes() != content:
        raise RuntimeError("canonical supervisor config changed during predecessor capture")
    descriptor = os.open(backup, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
    handle = os.fdopen(descriptor, "r+b")
    try:
        handle.write(content)
        handle.flush()
        os.fsync(handle.fileno())
        target = PredecessorShutdownTarget(
            config=config,
            state=state,
            backup=Path(backup),
            backup_handle=handle,
            backup_content=content,
            pid=pid,
            endpoint=endpoint,
            socket_identity=socket_identity,
        )
        require_predecessor_backup(target)
    except BaseException:
        handle.close()
        raise
    return target


def require_predecessor_backup(target):
    """Require the backup path to still name the pinned, unchanged owned file."""
    pinned = os.fstat(target.backup_handle.fileno())
    current = target.backup.lstat()
    if not stat.S_ISREG(current.st_mode) or (current.st_dev, current.st_ino) != (pinned.st_dev, pinned.st_ino):
        raise RuntimeError("predecessor shutdown backup was replaced")
    if pinned.st_uid != os.getuid() or pinned.st_nlink != 1 or stat.S_IMODE(pinned.st_mode) & 0o077:
        raise RuntimeError("predecessor shutdown backup ownership changed")
    content = os.pread(target.backup_handle.fileno(), pinned.st_size + 1, 0)
    if content != target.backup_content:
        raise RuntimeError("predecessor shutdown backup bytes changed")


def predecessor_shutdown_evidence(target):
    """Revalidate the captured generation; any successor progress is ambiguous.

    The caller has already validated the pinned backup under the same lease.
    """
    runtime = target.config.parent / "run"
    if runtime.resolve(strict=True) != runtime or target.endpoint.parent.resolve(strict=True) != target.endpoint.parent:
        raise RuntimeError("unsafe private supervisor runtime directory")
    if private_supervisor_pid(runtime / "pueue.pid") != target.pid:
        raise RuntimeError("private supervisor generation changed after predecessor capture")
    if private_socket_identity(target.endpoint) != target.socket_identity:
        raise RuntimeError("predecessor supervisor socket was replaced")
    return target.pid, target.endpoint, target.socket_identity


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
    return await_managed_lifetime_closure_until(state, task_ids, time.monotonic() + timeout)


def await_managed_lifetime_closure_until(state, task_ids, deadline):
    """Wait for the canonical closure until one caller-owned monotonic deadline."""
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


def shutdown_custody_roots(state, env):
    """Return the roots whose recorded custody pools this shutdown may read: its state and its TMPDIR."""
    temporary = env.get("TMPDIR")
    return (Path(state),) if not temporary else (Path(state), Path(temporary))


def await_custody_release(closure, owned_roots, deadline):
    """Outside every lease, wait for the recorded pools of a closed scope to release their views.

    Holding the transition lease here would stall the shared bootstrap
    maintenance that releases inspection views. A concrete pool fault raises at
    once; views still held at the deadline name their owner namespaces.
    """
    while True:
        pending = custody_release_pending(closure, owned_roots)
        if not pending:
            return
        require_custody_release_budget(pending, deadline)
        time.sleep(min(0.05, max(0.0, deadline - time.monotonic())))


def require_custody_release_budget(pending, deadline):
    """Refuse once the one shutdown deadline has passed with owner namespaces still holding views."""
    if time.monotonic() >= deadline:
        raise RuntimeError("custody release remained pending before supervisor shutdown: "
                           + describe_pending_custody(pending))


def disposal_ready_now(state, task_ids, owned_roots):
    """Return the closure when it validates and its recorded pools hold no views; otherwise None.

    Only an incomplete closure reads as not ready. A concrete pool identity or
    storage fault raises, so the caller retains instead of observing.
    """
    try:
        closure = authorize_managed_lifetime_closure(state, task_ids)
    except AcceptanceFailure:
        return None
    return None if custody_release_pending(closure, owned_roots) else closure


def sweep_disposal_observer(task_ids, deadline, observe):
    """Run observe(task_id, deadline) once per explicit task and return the reported root identities.

    The absolute deadline itself is passed on, never a recomputed budget. The
    sweep reuses the fixture's bounded caller concurrency and joins every
    started command before it returns or raises. No command starts once the
    deadline has passed or any worker's observation has failed.
    """
    stopped = threading.Event()

    def observe_one(task_id):
        if stopped.is_set():
            raise RuntimeError("disposal observation stopped after an earlier failure before task " + task_id)
        if time.monotonic() >= deadline:
            raise TimeoutError("disposal observation budget expired before task " + task_id)
        try:
            return observe(task_id, deadline)
        except BaseException:
            stopped.set()
            raise

    with ThreadPoolExecutor(max_workers=max(1, min(len(task_ids), DEFAULT_DISPATCH_WORKERS))) as pool:
        futures = [pool.submit(observe_one, task_id) for task_id in task_ids]
        try:
            return frozenset(future.result() for future in futures)
        except BaseException:
            stopped.set()
            pool.shutdown(wait=True, cancel_futures=True)
            raise


def await_observed_disposal_readiness(state, task_ids, owned_roots, deadline, observe):
    """Outside every lease, await closure and custody release with at most one public observer sweep.

    Already-ready scopes run no observation. Otherwise one sweep of the
    explicit created tasks gives the public recovery path its documented
    next-observer turn; the canonical closure and recorded pools are then
    reread under the same deadline. The observer outcome is never authority.
    """
    closure = disposal_ready_now(state, task_ids, owned_roots)
    if closure is not None:
        return closure
    observed_roots = sweep_disposal_observer(tuple(task_ids), deadline, observe)
    closure = await_managed_lifetime_closure_until(state, task_ids, deadline)
    if not observed_roots <= {closure["root_id"]}:
        raise RuntimeError("disposal status observation reported a different root than the canonical closure")
    await_custody_release(closure, owned_roots, deadline)
    return closure


def run_when_disposal_ready(state, task_ids, owned_roots, deadline, action, observe=None, *, disposal=None):
    """Run action(closure) under the transition lease once closure and custody release hold.

    Closure is authorized and custody release awaited outside the lease, then
    rechecked read-only once under it. Pending release leaves the lease, then
    the same deadline is checked before the next bounded wait, so a recheck
    that keeps finding views cannot outlive it. The one deadline covers every
    wait, observation and lock attempt. Without an observer an incomplete
    closure refuses at once; with one, readiness may first sweep it once.

    With a ``disposal`` owner the lease is its disposal lease, which also
    excludes elected maintenance and stays held after the action through root
    deletion. Every wait runs outside this root's lease: a held lease for this
    root is released first, while other roots the owner already shut down stay
    held.
    """
    if disposal is not None and disposal.holds(state) and disposal_ready_now(state, task_ids, owned_roots) is None:
        disposal.release(state)
    if observe is None:
        closure = authorize_managed_lifetime_closure(state, task_ids)
        await_custody_release(closure, owned_roots, deadline)
    else:
        closure = await_observed_disposal_readiness(state, task_ids, owned_roots, deadline, observe)
    while True:
        if disposal is None:
            with private_fixture_transition_until(Path(state), deadline):
                closure = authorize_managed_lifetime_closure(state, task_ids)
                pending = custody_release_pending(closure, owned_roots)
                if not pending:
                    return action(closure)
        else:
            disposal.hold(Path(state), deadline)
            closure = authorize_managed_lifetime_closure(state, task_ids)
            pending = custody_release_pending(closure, owned_roots)
            if not pending:
                return action(closure)
            disposal.release(state)
        require_custody_release_budget(pending, deadline)
        await_custody_release(closure, owned_roots, deadline)


def shutdown_private_supervisor(client, config, env, timeout=30, *, state, task_ids=(), capture_root, disposal=None):
    """Authorize closure and custody release, then shut down and observe PID and endpoint disappearance.

    The caller must not hold the transition lease. Returns the closure that
    authorized this shutdown, after the daemon exited and its endpoint closed.
    A final-disposal caller passes its ``disposal`` owner to keep the lease held.
    """
    return shutdown_observed_private_supervisor(
        client, config, env, timeout, state=state, task_ids=task_ids, capture_root=capture_root, observe=None,
        disposal=disposal,
    )


def shutdown_observed_private_supervisor(
    client, config, env, timeout=30, *, state, task_ids, capture_root, observe, disposal=None
):
    """Run shutdown_private_supervisor with an optional caller-bound public observer of ``task_ids``.

    ``observe`` is None or a callback bound to the selected Delegate CLI and
    owned root; ``task_ids`` must be known created records. See
    run_when_disposal_ready for when it runs.
    """
    deadline = time.monotonic() + timeout

    def shut_down(closure):
        shutdown_observed_supervisor(
            client, config, env, private_shutdown_evidence(config), deadline, capture_root=capture_root
        )
        return closure

    return run_when_disposal_ready(
        state, task_ids, shutdown_custody_roots(state, env), deadline, shut_down, observe, disposal=disposal
    )


def shutdown_pending_private_supervisor(pending, supervisor_cleanups, *, capture_root, disposal=None):
    """Shut down one ordinary supervisor ledger entry, then drop it from the ledger.

    Returns the authorizing closure, or None while the entry's config does not
    exist: the entry stays pending because absence alone proves no daemon closure.
    A failed shutdown raises and keeps the entry.
    """
    if not pending["config"].is_file():
        return None
    closure = shutdown_private_supervisor(
        pending["client"],
        pending["config"],
        pending["env"],
        state=pending["state"],
        task_ids=tuple(pending.get("task_ids", ())),
        capture_root=capture_root,
        disposal=disposal,
    )
    supervisor_cleanups.remove(pending)
    return closure


PREDECESSOR_SHUT_DOWN = "shut_down"
PREDECESSOR_ALREADY_CLOSED = "already_closed"


def predecessor_closed(target, deadline, *, capture_root):
    """Observe that the captured PID exited and the captured endpoint closed; send nothing."""
    return private_daemon_process_gone(
        target.pid, deadline, capture_root=capture_root
    ) and private_shutdown_endpoint_closed(target.endpoint, target.socket_identity, deadline)


def shutdown_predecessor_supervisor(client, target, env, timeout=30, *, task_ids=(), capture_root, disposal=None):
    """Shut down a captured pre-retarget generation through its pinned backup config.

    The production transition lease is held from the read-only readiness
    recheck through the final backup check, so cooperating lifecycle writers
    cannot change the captured root's config, PID file, or socket while the
    command runs. Closure and custody release are awaited before that lease.

    Returns PREDECESSOR_ALREADY_CLOSED only when the captured generation was
    proven closed before this cleanup sent anything; otherwise
    PREDECESSOR_SHUT_DOWN after this cleanup's own verified shutdown.
    """
    deadline = time.monotonic() + timeout

    def shut_down(_closure):
        require_predecessor_backup(target)
        # A migration may already have stopped this generation before it failed.
        if predecessor_closed(target, deadline, capture_root=capture_root):
            return PREDECESSOR_ALREADY_CLOSED
        shutdown_observed_supervisor(
            client, target.backup, env, predecessor_shutdown_evidence(target), deadline, capture_root=capture_root
        )
        # The command reads the backup by path. This detects a replaced backup;
        # it cannot prove that no transient substitution happened during the command.
        require_predecessor_backup(target)
        return PREDECESSOR_SHUT_DOWN

    return run_when_disposal_ready(
        target.state, task_ids, shutdown_custody_roots(target.state, env), deadline, shut_down, disposal=disposal
    )


def shutdown_observed_supervisor(client, config, env, evidence, deadline, *, capture_root):
    pid, endpoint, socket_identity = evidence
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
