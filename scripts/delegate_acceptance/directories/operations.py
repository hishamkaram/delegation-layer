"""Single directory-token owner and identity-pinned filesystem primitives."""

from __future__ import annotations

if __name__ != "scripts.delegate_acceptance.directories.operations":
    raise ImportError("Import shared owners through scripts.delegate_acceptance")

import ctypes
import os
import secrets
import stat
import sys
import time
from dataclasses import dataclass, field
from scripts.delegate_acceptance.directories.policy import require
from pathlib import Path


_DIRECTORY_OPEN_FLAGS = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_CLOEXEC", 0)


_DIRECTORY_OPEN_FLAGS |= getattr(os, "O_NOFOLLOW", 0)


def _require_fd_cleanup_support():
    required = (os.open, os.stat, os.unlink, os.rmdir, os.rename, os.mkdir)
    if not all(operation in os.supports_dir_fd for operation in required):
        raise RuntimeError("identity-pinned directory cleanup is unsupported on this platform")
    if not hasattr(os, "O_DIRECTORY") or not hasattr(os, "O_NOFOLLOW"):
        raise RuntimeError("identity-pinned directory cleanup lacks O_DIRECTORY/O_NOFOLLOW")
    if os.stat not in os.supports_follow_symlinks:
        raise RuntimeError("identity-pinned directory cleanup lacks no-follow stat")


def _open_parent_directory(parent):
    _require_fd_cleanup_support()
    parent = Path(parent)
    require(parent.is_absolute(), "owned directory parent must be absolute: " + str(parent))
    try:
        descriptor = os.open(parent, _DIRECTORY_OPEN_FLAGS)
    except OSError as error:
        raise RuntimeError("cannot hold owned directory parent: " + str(parent)) from error
    try:
        identity = os.fstat(descriptor)
        require(stat.S_ISDIR(identity.st_mode), "owned directory parent is unsafe: " + str(parent))
        path_identity = os.stat(parent, follow_symlinks=False)
        require((path_identity.st_dev, path_identity.st_ino) ==
                (identity.st_dev, identity.st_ino),
                "owned directory parent changed while opening: " + str(parent))
        return descriptor, (identity.st_dev, identity.st_ino)
    except BaseException:
        os.close(descriptor)
        raise


def _close_parent_descriptor(descriptor):
    try:
        os.close(descriptor)
    except OSError:
        pass


def _directory_name(directory, parent):
    directory = Path(directory)
    parent = Path(parent)
    require(directory.parent == parent,
            "owned directory does not have the expected parent: " + str(directory))
    require(directory.name not in ("", ".", ".."),
            "owned directory name is invalid: " + str(directory))
    return directory.name


@dataclass(slots=True)
class OwnedDirectory:
    """Single-use authority with live pins for its directory and parent."""

    label: str
    directory: Path
    parent: Path
    identity: tuple[int, int]
    parent_identity: tuple[int, int]
    _parent_fd: int = field(repr=False)
    _directory_fd: int = field(default=-1, repr=False)
    _consumed: bool = field(default=False, init=False, repr=False)

    @property
    def parent_fd(self) -> int:
        require(not self._consumed,
                "owned directory authority was already consumed: " + str(self.directory))
        return self._parent_fd

    @property
    def directory_fd(self) -> int:
        require(not self._consumed,
                "owned directory authority was already consumed: " + str(self.directory))
        require(self._directory_fd >= 0,
                "owned directory authority has no pinned directory: " + str(self.directory))
        return self._directory_fd

    def assert_live(self) -> None:
        """Require the original directory to retain its owned pathname."""
        _validate_entry_parent(self)
        name = _directory_name(self.directory, self.parent)
        visible = os.stat(name, dir_fd=self.parent_fd, follow_symlinks=False)
        require(stat.S_ISDIR(visible.st_mode)
                and (visible.st_dev, visible.st_ino) == self.identity,
                "owned directory was replaced: " + str(self.directory))

    def consume(self) -> None:
        require(not self._consumed,
                "owned directory authority was already consumed: " + str(self.directory))
        parent_fd = self._parent_fd
        directory_fd = self._directory_fd
        self._parent_fd = -1
        self._directory_fd = -1
        self._consumed = True
        _close_parent_descriptor(directory_fd)
        _close_parent_descriptor(parent_fd)


def _capture_created_directory(label, directory, parent, parent_fd, parent_identity):
    name = _directory_name(directory, parent)
    directory_fd = -1
    try:
        directory_fd = os.open(name, _DIRECTORY_OPEN_FLAGS, dir_fd=parent_fd)
        held = os.fstat(directory_fd)
        require(stat.S_ISDIR(held.st_mode) and not stat.S_ISLNK(held.st_mode),
                "owned directory is unsafe: " + str(directory))
        result = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
        require(stat.S_ISDIR(result.st_mode) and not stat.S_ISLNK(result.st_mode) and
                (result.st_dev, result.st_ino) == (held.st_dev, held.st_ino),
                "owned directory changed while being adopted: " + str(directory))
        return OwnedDirectory(str(label), Path(directory), Path(parent),
                              (held.st_dev, held.st_ino), parent_identity,
                              parent_fd, directory_fd)
    except BaseException:
        _close_parent_descriptor(directory_fd)
        raise


def _rename_noreplace(source_fd, source_name, destination_fd, destination_name):
    """Publish one directory name without ever replacing an existing entry."""
    library = ctypes.CDLL(None, use_errno=True)
    source = os.fsencode(source_name)
    destination = os.fsencode(destination_name)
    if sys.platform.startswith("linux"):
        operation = getattr(library, "renameat2", None)
        if operation is None:
            raise RuntimeError("atomic no-replace rename is unavailable on this Linux runtime")
        operation.argtypes = (
            ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint,
        )
        operation.restype = ctypes.c_int
        result = operation(source_fd, source, destination_fd, destination, 1)
    elif sys.platform == "darwin":
        operation = getattr(library, "renameatx_np", None)
        if operation is None:
            raise RuntimeError("atomic no-replace rename is unavailable on this macOS runtime")
        operation.argtypes = (
            ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint,
        )
        operation.restype = ctypes.c_int
        result = operation(source_fd, source, destination_fd, destination, 0x00000004)
    else:
        raise RuntimeError("atomic no-replace rename is unsupported on this platform")
    if result != 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error), str(destination_name))


def _create_one_directory(label, directory, parent, mode):
    directory = Path(directory)
    parent = Path(parent)
    name = _directory_name(directory, parent)
    parent_fd, parent_identity = _open_parent_directory(parent)
    staging_name = ".delegate-create-" + secrets.token_hex(16)
    staging_fd = -1
    staging_identity = None
    published = False
    try:
        os.mkdir(staging_name, mode=mode, dir_fd=parent_fd)
        staging_fd = os.open(staging_name, _DIRECTORY_OPEN_FLAGS, dir_fd=parent_fd)
        held = os.fstat(staging_fd)
        require(stat.S_ISDIR(held.st_mode),
                "staged owned directory is unsafe: " + str(directory))
        staging_identity = (held.st_dev, held.st_ino)
        visible = os.stat(staging_name, dir_fd=parent_fd, follow_symlinks=False)
        require(stat.S_ISDIR(visible.st_mode) and not stat.S_ISLNK(visible.st_mode) and
                (visible.st_dev, visible.st_ino) == staging_identity,
                "staged owned directory changed before publication: " + str(directory))
        _rename_noreplace(parent_fd, staging_name, parent_fd, name)
        published = True
        os.fsync(parent_fd)
        final = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
        require(stat.S_ISDIR(final.st_mode) and not stat.S_ISLNK(final.st_mode) and
                (final.st_dev, final.st_ino) == staging_identity,
                "owned directory changed during publication: " + str(directory))
        entry = OwnedDirectory(
            str(label), directory, parent, staging_identity, parent_identity,
            parent_fd, staging_fd,
        )
        staging_fd = -1
        return entry
    except BaseException:
        _close_parent_descriptor(staging_fd)
        if not published and staging_identity is not None:
            try:
                current = os.stat(staging_name, dir_fd=parent_fd, follow_symlinks=False)
                if ((current.st_dev, current.st_ino) == staging_identity
                        and stat.S_ISDIR(current.st_mode) and not stat.S_ISLNK(current.st_mode)):
                    os.rmdir(staging_name, dir_fd=parent_fd)
                    os.fsync(parent_fd)
            except OSError:
                pass
        _close_parent_descriptor(parent_fd)
        raise


def _validate_entry_parent(token):
    try:
        path_identity = os.stat(token.parent, follow_symlinks=False)
    except OSError as error:
        raise RuntimeError("owned directory parent is unavailable: " + str(token.parent)) from error
    require(stat.S_ISDIR(path_identity.st_mode) and not stat.S_ISLNK(path_identity.st_mode),
            "owned directory parent is unsafe: " + str(token.parent))
    require((path_identity.st_dev, path_identity.st_ino) == token.parent_identity,
            "owned directory parent was replaced: " + str(token.parent))
    held_identity = os.fstat(token.parent_fd)
    require(stat.S_ISDIR(held_identity.st_mode) and
            (held_identity.st_dev, held_identity.st_ino) == token.parent_identity,
            "held owned directory parent changed: " + str(token.parent))
    held_directory = os.fstat(token.directory_fd)
    require(stat.S_ISDIR(held_directory.st_mode) and not stat.S_ISLNK(held_directory.st_mode)
            and (held_directory.st_dev, held_directory.st_ino) == token.identity,
            "held owned directory changed: " + str(token.directory))


def _create_cleanup_quarantine(parent_fd):
    for _attempt in range(16):
        name = ".delegate-cleanup-" + secrets.token_hex(16)
        try:
            os.mkdir(name, mode=0o700, dir_fd=parent_fd)
        except FileExistsError:
            continue
        descriptor = os.open(name, _DIRECTORY_OPEN_FLAGS, dir_fd=parent_fd)
        identity = os.fstat(descriptor)
        require(stat.S_ISDIR(identity.st_mode), "cleanup quarantine is unsafe: " + name)
        return name, descriptor, (identity.st_dev, identity.st_ino)
    raise RuntimeError("could not allocate a unique cleanup quarantine")


def _check_cleanup_deadline(deadline):
    if deadline is not None and time.monotonic() >= deadline:
        raise RuntimeError("acceptance cleanup deadline expired")


def _remove_empty_cleanup_quarantine(parent_fd, name, descriptor, expected_identity,
                                     deadline=None):
    _check_cleanup_deadline(deadline)
    require(not os.listdir(descriptor), "cleanup quarantine is not empty: " + name)
    current = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
    require(stat.S_ISDIR(current.st_mode) and not stat.S_ISLNK(current.st_mode) and
            (current.st_dev, current.st_ino) == expected_identity,
            "cleanup quarantine was replaced: " + name)
    os.rmdir(name, dir_fd=parent_fd)


def _remove_entry_at(parent_fd, name, expected_identity, require_directory=False,
                     deadline=None, *, _allow_fifo=False):
    """Atomically isolate one exact entry before deleting any of its bytes."""
    _check_cleanup_deadline(deadline)
    quarantine_name, quarantine_fd, quarantine_identity = \
        _create_cleanup_quarantine(parent_fd)
    moved = False
    try:
        _check_cleanup_deadline(deadline)
        os.rename(name, "entry", src_dir_fd=parent_fd, dst_dir_fd=quarantine_fd)
        moved = True
        result = os.stat("entry", dir_fd=quarantine_fd, follow_symlinks=False)
        identity = (result.st_dev, result.st_ino)
        require(identity == expected_identity,
                "owned entry was replaced during cleanup; retained in quarantine: " +
                quarantine_name)
        is_directory = stat.S_ISDIR(result.st_mode) and not stat.S_ISLNK(result.st_mode)
        if require_directory:
            require(is_directory, "owned directory is unsafe: " + name)
        if is_directory:
            descriptor = os.open("entry", _DIRECTORY_OPEN_FLAGS, dir_fd=quarantine_fd)
            try:
                held = os.fstat(descriptor)
                require((held.st_dev, held.st_ino) == expected_identity,
                        "owned directory changed while opening: " + name)
                for child in os.listdir(descriptor):
                    _check_cleanup_deadline(deadline)
                    child_result = os.stat(
                        child, dir_fd=descriptor, follow_symlinks=False)
                    child_identity = (child_result.st_dev, child_result.st_ino)
                    _remove_entry_at(descriptor, child, child_identity,
                                     deadline=deadline)
                current = os.stat(
                    "entry", dir_fd=quarantine_fd, follow_symlinks=False)
                require((current.st_dev, current.st_ino) == expected_identity,
                        "owned directory changed during cleanup: " + name)
                os.rmdir("entry", dir_fd=quarantine_fd)
            finally:
                _close_parent_descriptor(descriptor)
        else:
            if _allow_fifo:
                require(stat.S_ISFIFO(result.st_mode) and result.st_uid == os.getuid(),
                        "owned FIFO is unsafe: " + name)
            else:
                require(stat.S_ISREG(result.st_mode) or stat.S_ISLNK(result.st_mode),
                        "unsupported unsafe entry in owned directory: " + name)
            current = os.stat("entry", dir_fd=quarantine_fd, follow_symlinks=False)
            require((current.st_dev, current.st_ino) == expected_identity,
                    "owned directory entry changed during cleanup: " + name)
            os.unlink("entry", dir_fd=quarantine_fd)
        _remove_empty_cleanup_quarantine(
            parent_fd, quarantine_name, quarantine_fd, quarantine_identity,
            deadline=deadline)
    except BaseException:
        if not moved:
            try:
                _remove_empty_cleanup_quarantine(
                    parent_fd, quarantine_name, quarantine_fd, quarantine_identity)
            except (OSError, RuntimeError):
                pass
        raise
    finally:
        _close_parent_descriptor(quarantine_fd)


def remove_exact_owned_fifo(parent_fd, entry_fd, name, expected_identity,
                            expected_parent_identity, deadline=None):
    """Remove one explicitly token-owned FIFO through the shared quarantine owner."""
    require(type(parent_fd) is int and parent_fd >= 0,
            "owned FIFO parent descriptor is invalid")
    require(type(entry_fd) is int and entry_fd >= 0,
            "owned FIFO entry descriptor is invalid")
    require(isinstance(name, str) and name not in ("", ".", "..")
            and Path(name).name == name and Path(name).parent == Path("."),
            "owned FIFO entry name is invalid")
    require(isinstance(expected_identity, tuple) and len(expected_identity) == 2
            and all(type(value) is int and value >= 0 for value in expected_identity),
            "owned FIFO identity is invalid")
    require(isinstance(expected_parent_identity, tuple) and len(expected_parent_identity) == 2
            and all(type(value) is int and value >= 0 for value in expected_parent_identity),
            "owned FIFO parent identity is invalid")
    _check_cleanup_deadline(deadline)
    try:
        held_parent = os.fstat(parent_fd)
    except OSError as error:
        raise RuntimeError("owned FIFO parent descriptor is not live") from error
    require(stat.S_ISDIR(held_parent.st_mode)
            and (held_parent.st_dev, held_parent.st_ino) == expected_parent_identity,
            "owned FIFO parent descriptor was replaced")
    try:
        held_entry = os.fstat(entry_fd)
    except OSError as error:
        raise RuntimeError("owned FIFO entry descriptor is not live") from error
    require(stat.S_ISFIFO(held_entry.st_mode) and held_entry.st_uid == os.getuid()
            and (held_entry.st_dev, held_entry.st_ino) == expected_identity,
            "owned FIFO entry descriptor was replaced")
    current = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
    require(stat.S_ISFIFO(current.st_mode) and current.st_uid == os.getuid()
            and (current.st_dev, current.st_ino) == expected_identity,
            "owned FIFO was replaced: " + name)
    _remove_entry_at(
        parent_fd, name, expected_identity, deadline=deadline, _allow_fifo=True,
    )
    return True


def remove_owned_directory(token: OwnedDirectory, deadline=None) -> bool:
    """Remove one token-owned directory through its held parent descriptor."""
    require(isinstance(token, OwnedDirectory), "owned directory identity token is required")
    parent_fd = token.parent_fd
    try:
        _check_cleanup_deadline(deadline)
        _validate_entry_parent(token)
        name = _directory_name(token.directory, token.parent)
        try:
            result = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
        except FileNotFoundError as error:
            raise RuntimeError("owned directory was replaced or removed: " +
                               str(token.directory)) from error
        require(stat.S_ISDIR(result.st_mode) and not stat.S_ISLNK(result.st_mode),
                "owned directory is unsafe: " + str(token.directory))
        require((result.st_dev, result.st_ino) == token.identity,
                "owned directory was replaced: " + str(token.directory))
        _remove_entry_at(parent_fd, name, token.identity, require_directory=True,
                         deadline=deadline)
        return True
    finally:
        token.consume()


def release_owned_directory(token: OwnedDirectory, deadline=None) -> bool:
    """Consume cleanup authority while deliberately retaining the directory."""
    require(isinstance(token, OwnedDirectory), "owned directory identity token is required")
    parent_fd = token.parent_fd
    try:
        _check_cleanup_deadline(deadline)
        _validate_entry_parent(token)
        name = _directory_name(token.directory, token.parent)
        result = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
        require(stat.S_ISDIR(result.st_mode) and not stat.S_ISLNK(result.st_mode),
                "owned directory is unsafe: " + str(token.directory))
        require((result.st_dev, result.st_ino) == token.identity,
                "owned directory was replaced: " + str(token.directory))
        return True
    finally:
        token.consume()


def clear_owned_directory(token: OwnedDirectory, deadline=None) -> bool:
    """Remove an owned directory's contents while retaining its exact root."""
    require(isinstance(token, OwnedDirectory), "owned directory identity token is required")
    parent_fd = token.parent_fd
    descriptor = token.directory_fd
    try:
        _check_cleanup_deadline(deadline)
        _validate_entry_parent(token)
        name = _directory_name(token.directory, token.parent)
        result = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
        require(stat.S_ISDIR(result.st_mode) and not stat.S_ISLNK(result.st_mode),
                "owned directory is unsafe: " + str(token.directory))
        require((result.st_dev, result.st_ino) == token.identity,
                "owned directory was replaced: " + str(token.directory))
        for child in os.listdir(descriptor):
            _check_cleanup_deadline(deadline)
            child_result = os.stat(child, dir_fd=descriptor, follow_symlinks=False)
            _remove_entry_at(
                descriptor, child, (child_result.st_dev, child_result.st_ino),
                deadline=deadline,
            )
        current = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
        require((current.st_dev, current.st_ino) == token.identity,
                "owned directory changed during cleanup: " + str(token.directory))
        require(not os.listdir(descriptor),
                "owned directory is not empty after cleanup: " + str(token.directory))
        return True
    finally:
        token.consume()


def _transfer_child_ownership(token: OwnedDirectory,
                              parent_token: OwnedDirectory) -> bool:
    """Move one direct child's cleanup authority into its owned parent tree."""
    require(isinstance(token, OwnedDirectory) and isinstance(parent_token, OwnedDirectory),
            "owned child and parent identity tokens are required")
    require(token.parent == parent_token.directory and
            token.parent_identity == parent_token.identity,
            "owned child is not directly contained by the parent token")
    _validate_entry_parent(parent_token)
    parent_name = _directory_name(parent_token.directory, parent_token.parent)
    parent_result = os.stat(
        parent_name, dir_fd=parent_token.parent_fd, follow_symlinks=False)
    require(stat.S_ISDIR(parent_result.st_mode) and not stat.S_ISLNK(parent_result.st_mode) and
            (parent_result.st_dev, parent_result.st_ino) == parent_token.identity,
            "owned parent directory was replaced: " + str(parent_token.directory))
    return release_owned_directory(token)


class DirectoryOwner:
    """Own exact disposable directories until rollback or explicit transfer."""

    def __init__(self):
        self._entries = []
        self._transferred = False

    def _append_created(self, label, directory, parent, parent_fd=None,
                        parent_identity=None):
        require(not self._transferred, "directory ownership was already transferred")
        directory = Path(directory)
        parent = Path(parent)
        require(all(existing.directory != directory for existing in self._entries),
                "owned directory was registered twice: " + str(directory))
        if parent_fd is None or parent_identity is None:
            parent_fd, parent_identity = _open_parent_directory(parent)
            close_after = True
        else:
            close_after = False
        try:
            entry = _capture_created_directory(label, directory, parent, parent_fd,
                                               parent_identity)
            self._entries.append(entry)
            return entry
        except BaseException as error:
            if close_after:
                _close_parent_descriptor(parent_fd)
            raise

    def create(self, label, directory, parent, mode=0o700, parents=False):
        """Create and immediately own one exact directory path."""
        require(not self._transferred, "directory ownership was already transferred")
        directory = Path(directory)
        parent = Path(parent)
        require(all(existing.directory != directory for existing in self._entries),
                "owned directory was registered twice: " + str(directory))
        try:
            os.lstat(directory)
        except FileNotFoundError:
            pass
        else:
            raise FileExistsError("owned directory already exists: " + str(directory))

        if not parents:
            entry = _create_one_directory(label, directory, parent, mode)
            self._entries.append(entry)
            return entry

        require(parent == directory.parent or parent in directory.parents,
                "owned directory parent is not an ancestor: " + str(parent))
        missing = []
        candidate = directory.parent
        while True:
            try:
                result = os.stat(candidate, follow_symlinks=False)
            except FileNotFoundError:
                missing.append(candidate)
                next_candidate = candidate.parent
                require(next_candidate != candidate,
                        "owned directory parent chain reached the filesystem root")
                candidate = next_candidate
                continue
            require(stat.S_ISDIR(result.st_mode) and not stat.S_ISLNK(result.st_mode),
                    "owned directory ancestor is unsafe: " + str(candidate))
            break
        for path in reversed(missing):
            path_label = str(label) + " parent"
            entry = _create_one_directory(path_label, path, path.parent, mode)
            self._entries.append(entry)
        entry = _create_one_directory(label, directory, directory.parent, mode)
        self._entries.append(entry)
        return entry

    def adopt_created(self, label, directory, parent):
        """Adopt a directory created immediately before this call."""
        require(not self._transferred, "directory ownership was already transferred")
        directory = Path(directory)
        parent = Path(parent)
        require(all(existing.directory != directory for existing in self._entries),
                "owned directory was registered twice: " + str(directory))
        return self._append_created(label, directory, parent)

    def transfer(self):
        require(not self._transferred, "directory ownership was already transferred")
        self._transferred = True
        entries = tuple(self._entries)
        self._entries.clear()
        return entries

    def transfer_child_to(self, parent_token: OwnedDirectory) -> bool:
        """Transfer one direct child into an already owned recursive tree."""
        require(not self._transferred, "directory ownership was already transferred")
        require(len(self._entries) == 1,
                "child ownership transfer requires exactly one directory")
        _transfer_child_ownership(self._entries[0], parent_token)
        self._entries.clear()
        self._transferred = True
        return True

    def release(self) -> bool:
        """Retain owned directories while consuming this owner's authorities."""
        require(not self._transferred, "directory ownership was already transferred")
        failures = []
        for entry in reversed(self._entries):
            try:
                release_owned_directory(entry)
            except (OSError, RuntimeError) as error:
                failures.append(entry.label + ": " + str(error))
        self._entries.clear()
        self._transferred = True
        if failures:
            raise RuntimeError("directory ownership release failed: " + "; ".join(failures))
        return True

    def clear(self) -> bool:
        """Delete owned contents while retaining each exact directory root."""
        require(not self._transferred, "transferred directory ownership cannot be cleared")
        failures = []
        for entry in reversed(self._entries):
            try:
                clear_owned_directory(entry)
            except (OSError, RuntimeError) as error:
                failures.append(entry.label + ": " + str(error))
        self._entries.clear()
        self._transferred = True
        if failures:
            raise RuntimeError("directory ownership clear failed: " + "; ".join(failures))
        return True

    def rollback(self):
        require(not self._transferred, "transferred directory ownership cannot roll back")
        rollback_owned_directories(tuple(self._entries))
        self._entries.clear()
        return True


def _is_ancestor_entry(ancestor: OwnedDirectory, descendant: OwnedDirectory) -> bool:
    return ancestor.directory != descendant.directory and ancestor.directory in descendant.directory.parents


def _retain_after_descendant_failure(entry: OwnedDirectory, failures, deadline=None):
    """Consume an ancestor token without deleting its quarantined descendant."""
    try:
        if deadline is None:
            release_owned_directory(entry)
        else:
            release_owned_directory(entry, deadline=deadline)
    except (OSError, RuntimeError) as error:
        failures.append(entry.label + ": ancestor retained after descendant cleanup failure: " +
                        str(error))


def rollback_owned_directories(entries, deadline=None):
    """Roll back transferred exact-directory ownership in reverse order."""
    failures = []
    failed_entries = []
    for entry in reversed(tuple(entries)):
        try:
            require(isinstance(entry, OwnedDirectory),
                    "directory rollback ownership entry is invalid")
            if any(_is_ancestor_entry(entry, failed) for failed in failed_entries):
                _retain_after_descendant_failure(entry, failures, deadline)
                continue
            if deadline is None:
                remove_owned_directory(entry)
            else:
                remove_owned_directory(entry, deadline=deadline)
        except (OSError, RuntimeError) as error:
            label = entry.label if isinstance(entry, OwnedDirectory) else "invalid"
            failures.append(label + ": " + str(error))
            if isinstance(entry, OwnedDirectory):
                failed_entries.append(entry)
    if failures:
        raise RuntimeError("directory rollback failed: " + "; ".join(failures))
    return True
