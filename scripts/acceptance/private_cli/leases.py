"""Single owners of fixture gate locks and live FIFO lease descriptors."""

from contextlib import ExitStack
from contextlib import contextmanager
from pathlib import Path
from scripts.acceptance.directories.operations import _close_parent_descriptor
from scripts.acceptance.directories.operations import _DIRECTORY_OPEN_FLAGS
from scripts.acceptance.directories.operations import _open_parent_directory
from scripts.acceptance.directories.operations import remove_exact_owned_fifo
from scripts.acceptance.directories.policy import require
from scripts.acceptance.directories.policy import require_private_directory
from scripts.acceptance.private_cli.configuration import LEASE_HANDSHAKE_TIMEOUT_SECONDS
from scripts.acceptance.private_cli.errors import AcceptanceBlocked
from scripts.acceptance.processes.observation import wait_until
import errno
import fcntl
import os
import stat
import time


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


def ordinary_lifetime_owner_live(path):
    """Probe an ordinary guardian lease without assuming ownership or signaling."""
    descriptor = os.open(path, os.O_RDWR | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK)
    try:
        opened = os.fstat(descriptor)
        visible = os.stat(path, follow_symlinks=False)
        require(stat.S_ISREG(opened.st_mode) and opened.st_uid == os.getuid()
                and (opened.st_dev, opened.st_ino) == (visible.st_dev, visible.st_ino),
                "ordinary lifetime owner lock is unsafe or replaced")
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return True
        fcntl.flock(descriptor, fcntl.LOCK_UN)
        return False
    finally:
        os.close(descriptor)


TRANSITION_LOCK_TIMEOUT_SECONDS = 30


@contextmanager
def private_fixture_transition(state):
    """Serialize historical fixture seeds with the production transition owner.

    Each lock waits up to TRANSITION_LOCK_TIMEOUT_SECONDS.
    """
    with _transition_locks(state, lambda: TRANSITION_LOCK_TIMEOUT_SECONDS) as supervisor:
        yield supervisor


@contextmanager
def private_fixture_transition_until(state, deadline):
    """Take the same transition lease with every lock wait bounded by one monotonic deadline.

    An expired deadline still makes one acquisition attempt per lock.
    """
    with _transition_locks(state, lambda: max(0.0, deadline - time.monotonic())) as supervisor:
        yield supervisor


@contextmanager
def _transition_locks(state, lock_timeout):
    supervisor = state / ".supervisor"
    with ExitStack() as owners:
        for name in ("transition.lock", "bootstrap.lock"):
            descriptor = os.open(supervisor / name, os.O_RDWR | os.O_NOFOLLOW)
            owners.callback(os.close, descriptor)

            def acquire():
                try:
                    fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    return True
                except BlockingIOError:
                    return False

            wait_until(acquire, timeout=lock_timeout(), description="historical seed lifecycle lock")
        yield supervisor


DISPOSAL_ELECTION_LOCK = "queue-maintenance.lock"
# The election lock comes first: an elected maintenance owner takes bootstrap SH
# while it holds the election, so waiting for the election while holding
# bootstrap EX could deadlock against it.
DISPOSAL_LOCK_ORDER = (DISPOSAL_ELECTION_LOCK, "transition.lock", "bootstrap.lock")
_DISPOSAL_LOCK_FLAGS = os.O_RDWR | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)


class _HeldDisposal:
    """Exclusive disposal locks, with live pins for the state and supervisor directories."""

    def __init__(self, state, state_fd, state_identity, supervisor_fd, supervisor_identity):
        self.state = state
        self.state_fd = state_fd
        self.state_identity = state_identity
        self.supervisor_fd = supervisor_fd
        self.supervisor_identity = supervisor_identity
        # Every opened lock descriptor is owned here from its open; ``acquired`` names the flocked ones.
        self.locks = {}
        self.acquired = []

    def require_current(self):
        """Require the pinned directory and every held lock inode to remain at their paths."""
        require(tuple(self.acquired) == DISPOSAL_LOCK_ORDER, "disposal lease is incomplete: " + str(self.state))
        try:
            root = os.stat(self.state, follow_symlinks=False)
        except OSError as error:
            raise RuntimeError("disposal lease state root is missing or replaced: " + str(self.state)) from error
        retained = os.fstat(self.state_fd)
        require(stat.S_ISDIR(root.st_mode)
                and (root.st_dev, root.st_ino) == self.state_identity == (retained.st_dev, retained.st_ino),
                "disposal lease state root was replaced: " + str(self.state))
        try:
            visible = os.stat(".supervisor", dir_fd=self.state_fd, follow_symlinks=False)
        except OSError as error:
            raise RuntimeError("disposal lease supervisor directory is missing: " + str(self.state)) from error
        retained = os.fstat(self.supervisor_fd)
        require(
            stat.S_ISDIR(visible.st_mode) and (visible.st_dev, visible.st_ino)
            == self.supervisor_identity == (retained.st_dev, retained.st_ino),
            "disposal lease supervisor directory was replaced: " + str(self.state),
        )
        for name, descriptor in self.locks.items():
            held = os.fstat(descriptor)
            try:
                entry = os.stat(name, dir_fd=self.supervisor_fd, follow_symlinks=False)
            except OSError as error:
                raise RuntimeError("disposal lease lock is missing: " + name) from error
            require(
                (entry.st_dev, entry.st_ino) == (held.st_dev, held.st_ino),
                "disposal lease lock was replaced: " + name,
            )

    def close(self):
        descriptors = [*reversed(tuple(self.locks.values())), self.supervisor_fd, self.state_fd]
        self.locks = {}
        self.acquired = []
        self.supervisor_fd = -1
        self.state_fd = -1
        first_error = None
        for descriptor in descriptors:
            try:
                os.close(descriptor)
            except OSError as error:
                first_error = first_error or error
        if first_error is not None:
            raise first_error


class DisposalLeases:
    """Own every acceptance disposal lease from authorized shutdown through exact root deletion.

    One instance belongs to one final-disposal consumer. Holding a root's
    election, transition and bootstrap locks proves only that no elected
    maintenance owner runs and none can start; it does not prove that late
    callback helpers exited. A root already held is never reopened, because a
    second open of a held flock conflicts with this process's own lease.
    """

    def __init__(self):
        self._held = {}

    def __enter__(self):
        return self

    def __exit__(self, _error_type, _error, _traceback):
        self.release_all()

    def holds(self, state):
        return Path(state) in self._held

    def hold(self, state, deadline):
        """Hold the disposal locks of an existing private root, each polled until the one deadline.

        An expired deadline still makes one acquisition attempt per lock. A root
        already held is revalidated, not reacquired.
        """
        state = Path(state)
        held = self._held.get(state)
        if held is not None:
            held.require_current()
            return
        held = _open_held_disposal(state)
        try:
            # Validate the existing lifecycle locks before the optional election lock may
            # be created, so a refused root is left exactly as found.
            for name in (*DISPOSAL_LOCK_ORDER[1:], DISPOSAL_ELECTION_LOCK):
                held.locks[name] = _open_disposal_lock(held.supervisor_fd, name)
            for name in DISPOSAL_LOCK_ORDER:
                _acquire_disposal_lock(held.locks[name], name, deadline)
                held.acquired.append(name)
            held.require_current()
        except BaseException:
            held.close()
            raise
        self._held[state] = held

    def require_held(self, state):
        held = self._held.get(Path(state))
        if held is None:
            raise RuntimeError("private state root has no held disposal lease: " + str(state))
        held.require_current()
        return held

    def release(self, state):
        """Release one held root so its elected owner can progress; other held roots stay held."""
        held = self._held.pop(Path(state), None)
        if held is not None:
            held.close()

    def release_all(self):
        """Release every held root when the disposal owner exits."""
        held = tuple(self._held.values())
        self._held = {}
        first_error = None
        for entry in reversed(held):
            try:
                entry.close()
            except OSError as error:
                first_error = first_error or error
        if first_error is not None:
            raise first_error


def _open_held_disposal(state):
    state_fd, state_identity = _open_parent_directory(state)
    supervisor_fd = -1
    try:
        supervisor_fd = os.open(".supervisor", _DIRECTORY_OPEN_FLAGS, dir_fd=state_fd)
        supervisor = os.fstat(supervisor_fd)
        require_private_directory(
            supervisor,
            "disposal lease supervisor directory is unsafe: " + str(state),
            owner_uid=os.geteuid(),
        )
    except BaseException:
        _close_parent_descriptor(supervisor_fd)
        _close_parent_descriptor(state_fd)
        raise
    return _HeldDisposal(state, state_fd, state_identity, supervisor_fd, (supervisor.st_dev, supervisor.st_ino))


def _acquire_disposal_lock(descriptor, name, deadline):
    def acquire():
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return True
        except BlockingIOError:
            return False

    wait_until(acquire, timeout=max(0.0, deadline - time.monotonic()), description="disposal lease " + name)


def _open_disposal_lock(supervisor_fd, name):
    """Open an existing lock, creating only a missing election lock inside the pinned directory."""
    try:
        descriptor = os.open(name, _DISPOSAL_LOCK_FLAGS, dir_fd=supervisor_fd)
    except FileNotFoundError as error:
        if name != DISPOSAL_ELECTION_LOCK:
            raise RuntimeError("disposal lease lock is missing: " + name) from error
        descriptor = _create_election_lock(supervisor_fd)
    try:
        opened = os.fstat(descriptor)
        require(
            stat.S_ISREG(opened.st_mode)
            and opened.st_uid == os.geteuid()
            and not stat.S_IMODE(opened.st_mode) & 0o077,
            "disposal lease lock is not a protected regular file: " + name,
        )
        return descriptor
    except BaseException:
        os.close(descriptor)
        raise


def _create_election_lock(supervisor_fd):
    """Create the election lock once, as the Go owner would, then barrier its directory entry."""
    try:
        descriptor = os.open(DISPOSAL_ELECTION_LOCK, _DISPOSAL_LOCK_FLAGS | os.O_CREAT | os.O_EXCL, 0o600, dir_fd=supervisor_fd)
    except FileExistsError:
        # A concurrent Go owner won the creation; verify its winner instead.
        return os.open(DISPOSAL_ELECTION_LOCK, _DISPOSAL_LOCK_FLAGS, dir_fd=supervisor_fd)
    try:
        os.fsync(supervisor_fd)
        return descriptor
    except BaseException:
        os.close(descriptor)
        raise


def require_empty_fixture_waiters(waiter_directory):
    entries = tuple(Path(waiter_directory).iterdir())
    if entries:
        raise RuntimeError(
            "fixture provider lease entries remained after canonical lifetime closure: " + str(len(entries))
        )


def _validate_live_parent_pin(path, parent_descriptor, parent_identity, task_id):
    """Validate the path and descriptor still identify the original parent."""
    require(
        type(parent_descriptor) is int and parent_descriptor >= 0,
        "capacity fixture lease parent descriptor is closed: " + task_id,
    )
    try:
        held_parent = os.fstat(parent_descriptor)
    except OSError as error:
        raise RuntimeError("capacity fixture lease parent descriptor is closed: " + task_id) from error
    require(
        stat.S_ISDIR(held_parent.st_mode) and (held_parent.st_dev, held_parent.st_ino) == parent_identity,
        "capacity fixture lease parent descriptor was replaced: " + task_id,
    )
    try:
        current_parent = Path(path).parent.lstat()
    except OSError as error:
        raise RuntimeError("capacity fixture lease parent was replaced: " + task_id) from error
    require(
        stat.S_ISDIR(current_parent.st_mode)
        and not stat.S_ISLNK(current_parent.st_mode)
        and (current_parent.st_dev, current_parent.st_ino) == parent_identity,
        "capacity fixture lease parent was replaced: " + task_id,
    )


class LiveFIFOLease:
    """An identity-pinned FIFO lease with a live entry and parent pin."""

    def __init__(
        self, task_id, pid, path, descriptor, parent_descriptor, identity, parent_identity, owns_parent_descriptor
    ):
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
                task_id=task_id, pid=pid, path=path, nonce=nonce, parent_descriptor=parent_descriptor,
                parent_identity=parent_identity, timeout=timeout, owns_parent_descriptor=True
            )
        except BaseException:
            _close_parent_descriptor(parent_descriptor)
            raise

    @classmethod
    def _acquire_with_parent(
        cls, *, task_id, pid, path, nonce, parent_descriptor, parent_identity, timeout, owns_parent_descriptor
    ):
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
            if (
                not stat.S_ISFIFO(opened.st_mode)
                or opened.st_uid != os.getuid()
                or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino)
            ):
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
                task_id=task_id,
                pid=pid,
                path=path,
                descriptor=descriptor,
                parent_descriptor=parent_descriptor,
                identity=(opened.st_dev, opened.st_ino),
                parent_identity=parent_identity,
                owns_parent_descriptor=owns_parent_descriptor,
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
        _validate_live_parent_pin(self.path, self.parent_descriptor, self.parent_identity, self.task_id)
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
        require(self.descriptor >= 0, "capacity fixture lease entry descriptor is closed: " + self.task_id)
        require(
            parent_descriptor == self.parent_descriptor,
            "capacity fixture lease parent descriptor is not its pinned parent: " + self.task_id,
        )
        _validate_live_parent_pin(self.path, parent_descriptor, self.parent_identity, self.task_id)
        try:
            remove_exact_owned_fifo(
                parent_descriptor, self.descriptor, self.path.name, self.identity, self.parent_identity
            )
        except FileNotFoundError:
            return {"path": str(self.path), "state": "absent"}
        return {"path": str(self.path), "state": "removed", "device": self.identity[0], "inode": self.identity[1]}

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
                leases.append(
                    LiveFIFOLease._acquire_with_parent(
                        task_id=specification["task_id"],
                        pid=specification["pid"],
                        path=path,
                        nonce=nonce,
                        parent_descriptor=parent_pin[0],
                        parent_identity=parent_pin[1],
                        timeout=timeout,
                        owns_parent_descriptor=False,
                    )
                )
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
        return [lease._remove_closed_fifo(lease.parent_descriptor) for lease in tuple(self.leases)]

    def __enter__(self):
        return self

    def __exit__(self, _error_type, _error, _traceback):
        self.close()
