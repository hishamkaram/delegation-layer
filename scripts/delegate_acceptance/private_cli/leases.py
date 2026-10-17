"""Single owners of fixture gate locks and live FIFO lease descriptors."""

from pathlib import Path
from scripts.delegate_acceptance.directories.operations import _close_parent_descriptor
from scripts.delegate_acceptance.directories.operations import _open_parent_directory
from scripts.delegate_acceptance.directories.operations import remove_exact_owned_fifo
from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.private_cli.configuration import LEASE_HANDSHAKE_TIMEOUT_SECONDS
from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
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
