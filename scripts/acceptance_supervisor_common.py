"""Finite process ownership and exact-byte evidence for supervisor acceptance."""
import ctypes
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import secrets
import stat
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass, field
from typing import BinaryIO, Callable, Protocol


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def sha(data):
    return hashlib.sha256(data).hexdigest()


def digest(path):
    value = hashlib.sha256()
    with Path(path).open("rb") as stream:
        while block := stream.read(1024 * 1024):
            value.update(block)
    return value.hexdigest()


def write_json(path, value, replace=False):
    path = Path(path)
    data = (json.dumps(value, sort_keys=True, indent=2) + "\n").encode()
    descriptor, stage_name = tempfile.mkstemp(
        prefix="." + path.name + ".", suffix=".staging", dir=path.parent,
    )
    stage = Path(stage_name)
    parent = -1
    try:
        stream = os.fdopen(descriptor, "wb")
        descriptor = -1
        with stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(stage, 0o600)
        parent = os.open(path.parent, _DIRECTORY_OPEN_FLAGS)
        if replace:
            os.replace(stage, path)
        else:
            _rename_noreplace(parent, stage.name, parent, path.name)
        os.fsync(parent)
    finally:
        _close_parent_descriptor(descriptor)
        _close_parent_descriptor(parent)
        try:
            stage.unlink()
        except FileNotFoundError:
            pass


def read_json(path, bound=1024 * 1024):
    with Path(path).open("rb") as stream:
        data = stream.read(bound + 1)
    require(len(data) <= bound, "JSON evidence exceeds bound: " + str(path))
    def pairs(entries):
        obj = {}
        for key, value in entries:
            require(key not in obj, "duplicate evidence key: " + key)
            obj[key] = value
        return obj
    return json.loads(data, object_pairs_hook=pairs)


def inherited_environment():
    # Each included coordinate is inherited without relocation or PATH shadowing.
    keys = {"HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL",
            "LC_CTYPE", "TZ", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
            "XDG_RUNTIME_DIR", "__CF_USER_TEXT_ENCODING"}
    return {key: value for key, value in os.environ.items() if key in keys}


def config_for(base):
    base = Path(base)
    return {
        "shared": {
            "pueue_directory": str(base / "state"),
            "runtime_directory": str(base / "run"),
            "unix_socket_path": str(base / "run" / "p.sock"),
            "alias_file": str(base / "aliases.yml"),
            "pid_path": str(base / "run" / "p.pid"),
            "shared_secret_path": str(base / "run" / "secret"),
            "daemon_cert": str(base / "run" / "cert.pem"),
            "daemon_key": str(base / "run" / "key.pem"),
            "use_unix_socket": True, "unix_socket_permissions": 384,
            "host": "127.0.0.1", "port": "6924",
        },
        "client": {
            "restart_in_place": False, "read_local_logs": True,
            "show_confirmation_questions": False, "edit_mode": "toml",
            "show_expanded_aliases": False, "dark_mode": False,
            "max_status_lines": None, "status_time_format": "%H:%M:%S",
            "status_datetime_format": "%Y-%m-%d\n%H:%M:%S",
        },
        "daemon": {
            "pause_group_on_failure": False, "pause_all_on_failure": False,
            "compress_state_file": False, "callback": None, "env_vars": {},
            "callback_log_lines": 10,
            "shell_command": ["/bin/sh", "-c", "{{ pueue_command_string }}"],
        },
        "profiles": {},
    }


class OutputCapture(Protocol):
    """Bounded stdout capture owned by the shared process lifecycle."""

    writer: BinaryIO
    eof: bool
    error: str | None

    def drain(self) -> None:
        ...

    def sanitized(self, code: int) -> bytes:
        ...

    def close(self) -> None:
        ...


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
    """Single-use authority for one directory created by this invocation."""

    label: str
    directory: Path
    parent: Path
    identity: tuple[int, int]
    parent_identity: tuple[int, int]
    _parent_fd: int = field(repr=False)
    _consumed: bool = field(default=False, init=False, repr=False)

    @property
    def parent_fd(self) -> int:
        require(not self._consumed,
                "owned directory authority was already consumed: " + str(self.directory))
        return self._parent_fd

    def consume(self) -> None:
        require(not self._consumed,
                "owned directory authority was already consumed: " + str(self.directory))
        descriptor = self._parent_fd
        self._parent_fd = -1
        self._consumed = True
        _close_parent_descriptor(descriptor)


def _capture_created_directory(label, directory, parent, parent_fd, parent_identity):
    name = _directory_name(directory, parent)
    try:
        result = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
    except OSError:
        raise
    require(stat.S_ISDIR(result.st_mode) and not stat.S_ISLNK(result.st_mode),
            "owned directory is unsafe: " + str(directory))
    return OwnedDirectory(str(label), Path(directory), Path(parent),
                          (result.st_dev, result.st_ino), parent_identity, parent_fd)


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
        _close_parent_descriptor(staging_fd)
        staging_fd = -1
        return OwnedDirectory(
            str(label), directory, parent, staging_identity, parent_identity, parent_fd,
        )
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
                     deadline=None):
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
    descriptor = -1
    try:
        _check_cleanup_deadline(deadline)
        _validate_entry_parent(token)
        name = _directory_name(token.directory, token.parent)
        result = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
        require(stat.S_ISDIR(result.st_mode) and not stat.S_ISLNK(result.st_mode),
                "owned directory is unsafe: " + str(token.directory))
        require((result.st_dev, result.st_ino) == token.identity,
                "owned directory was replaced: " + str(token.directory))
        descriptor = os.open(name, _DIRECTORY_OPEN_FLAGS, dir_fd=parent_fd)
        held = os.fstat(descriptor)
        require((held.st_dev, held.st_ino) == token.identity,
                "owned directory changed while opening: " + str(token.directory))
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
        _close_parent_descriptor(descriptor)
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


_CLEANUP_JOURNAL_VERSION = 1
_CLEANUP_JOURNAL_KIND = "owned-directory-cleanup"
_CLEANUP_STATES = {"prepared", "isolated", "contents-removed", "complete"}
_CLEANUP_HEX = frozenset("0123456789abcdef")
_CLEANUP_JOURNAL_BOUND = 64 * 1024


def directory_identity_record(token: OwnedDirectory) -> dict[str, object]:
    """Return the serializable identity carried by one ownership token."""
    require(isinstance(token, OwnedDirectory), "owned directory identity token is required")
    return {
        "path": str(token.directory.absolute()),
        "parent": str(token.parent.absolute()),
        "device": token.identity[0],
        "inode": token.identity[1],
        "parent_device": token.parent_identity[0],
        "parent_inode": token.parent_identity[1],
    }


def _cleanup_identity(value, label):
    require(isinstance(value, dict), label + " identity is invalid")
    required = {"path", "parent", "device", "inode", "parent_device", "parent_inode"}
    require(set(value) == required, label + " identity fields are invalid")
    path = Path(value["path"])
    parent = Path(value["parent"])
    require(path.is_absolute() and parent.is_absolute() and path.parent == parent and
            path.name not in ("", ".", "..") and ".." not in path.parts,
            label + " identity path is invalid")
    require(all(type(value[name]) is int and value[name] >= 0 for name in
                ("device", "inode", "parent_device", "parent_inode")),
            label + " identity numbers are invalid")
    return {
        "path": str(path), "parent": str(parent),
        "device": value["device"], "inode": value["inode"],
        "parent_device": value["parent_device"], "parent_inode": value["parent_inode"],
    }


def _cleanup_identity_matches_token(identity, token):
    return (token.directory == Path(identity["path"])
            and token.parent == Path(identity["parent"])
            and token.identity == (identity["device"], identity["inode"])
            and token.parent_identity ==
            (identity["parent_device"], identity["parent_inode"]))


def _cleanup_path_state(identity):
    path = Path(identity["path"])
    try:
        current = path.lstat()
    except FileNotFoundError:
        return "absent"
    except OSError as error:
        raise RuntimeError("cleanup path is unreadable: " + str(path)) from error
    if (stat.S_ISDIR(current.st_mode) and not stat.S_ISLNK(current.st_mode)
            and (current.st_dev, current.st_ino) ==
            (identity["device"], identity["inode"])):
        return "exact"
    return "changed"


def _cleanup_open_parent(identity):
    descriptor, observed = _open_parent_directory(Path(identity["parent"]))
    try:
        require(observed == (identity["parent_device"], identity["parent_inode"]),
                "cleanup parent identity changed: " + identity["parent"])
        return descriptor
    except BaseException:
        os.close(descriptor)
        raise


def _cleanup_open_directory(identity):
    parent_fd = _cleanup_open_parent(identity)
    try:
        descriptor = os.open(Path(identity["path"]).name, _DIRECTORY_OPEN_FLAGS,
                             dir_fd=parent_fd)
        held = os.fstat(descriptor)
        require((held.st_dev, held.st_ino) == (identity["device"], identity["inode"]),
                "cleanup directory identity changed: " + identity["path"])
        return parent_fd, descriptor
    except BaseException:
        os.close(parent_fd)
        raise


def _cleanup_clear_contents(descriptor):
    """Idempotently clear one already-isolated exact directory."""
    for name in os.listdir(descriptor):
        current = os.stat(name, dir_fd=descriptor, follow_symlinks=False)
        identity = (current.st_dev, current.st_ino)
        if stat.S_ISDIR(current.st_mode) and not stat.S_ISLNK(current.st_mode):
            child = os.open(name, _DIRECTORY_OPEN_FLAGS, dir_fd=descriptor)
            try:
                held = os.fstat(child)
                require((held.st_dev, held.st_ino) == identity,
                        "isolated cleanup child changed while opening: " + name)
                _cleanup_clear_contents(child)
                observed = os.stat(name, dir_fd=descriptor, follow_symlinks=False)
                require((observed.st_dev, observed.st_ino) == identity,
                        "isolated cleanup child was replaced: " + name)
                os.rmdir(name, dir_fd=descriptor)
            finally:
                os.close(child)
        else:
            require(stat.S_ISREG(current.st_mode) or stat.S_ISLNK(current.st_mode),
                    "isolated cleanup contains an unsupported entry: " + name)
            observed = os.stat(name, dir_fd=descriptor, follow_symlinks=False)
            require((observed.st_dev, observed.st_ino) == identity,
                    "isolated cleanup entry was replaced: " + name)
            os.unlink(name, dir_fd=descriptor)
    os.fsync(descriptor)
    require(not os.listdir(descriptor), "isolated cleanup directory is not empty")


def _cleanup_validate_journal_path(path):
    path = Path(path).absolute()
    require(path.name not in ("", ".", "..") and ".." not in path.parts,
            "cleanup journal path is invalid")
    parent_fd, _ = _open_parent_directory(path.parent)
    os.close(parent_fd)
    try:
        current = path.lstat()
    except FileNotFoundError:
        pass
    else:
        require(stat.S_ISREG(current.st_mode) and not stat.S_ISLNK(current.st_mode),
                "cleanup journal is unsafe: " + str(path))
    return path


def _cleanup_lock(path):
    return acquire_exclusive_file_lock(path.with_name(path.name + ".lock"))


def acquire_exclusive_file_lock(path):
    """Acquire one path-stable advisory lock and return its owned descriptor."""
    path = Path(path).absolute()
    require(path.name not in ("", ".", "..") and ".." not in path.parts,
            "exclusive lock path is invalid")
    parent_fd, _ = _open_parent_directory(path.parent)
    flags = os.O_RDWR | os.O_CREAT | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    descriptor = -1
    try:
        descriptor = os.open(path.name, flags, 0o600, dir_fd=parent_fd)
        current = os.fstat(descriptor)
        visible = os.stat(path.name, dir_fd=parent_fd, follow_symlinks=False)
        require(stat.S_ISREG(current.st_mode) and not stat.S_ISLNK(visible.st_mode)
                and (current.st_dev, current.st_ino) == (visible.st_dev, visible.st_ino),
                "exclusive lock file is unsafe")
        fcntl.flock(descriptor, fcntl.LOCK_EX)
        return descriptor
    except BaseException:
        _close_parent_descriptor(descriptor)
        raise
    finally:
        os.close(parent_fd)


def _cleanup_unlock(descriptor):
    release_exclusive_file_lock(descriptor)


def release_exclusive_file_lock(descriptor):
    """Release one descriptor returned by acquire_exclusive_file_lock."""
    try:
        fcntl.flock(descriptor, fcntl.LOCK_UN)
    finally:
        os.close(descriptor)


def _cleanup_read_journal(path):
    current = path.lstat()
    require(stat.S_ISREG(current.st_mode) and not stat.S_ISLNK(current.st_mode)
            and current.st_size <= _CLEANUP_JOURNAL_BOUND,
            "cleanup journal is invalid: " + str(path))
    value = read_json(path, _CLEANUP_JOURNAL_BOUND)
    require(isinstance(value, dict), "cleanup journal is malformed")
    required = {
        "schema_version", "kind", "transaction_id", "label", "authorization_sha256",
        "state", "target", "quarantine",
    }
    require(set(value) == required and value.get("schema_version") == _CLEANUP_JOURNAL_VERSION
            and value.get("kind") == _CLEANUP_JOURNAL_KIND
            and isinstance(value.get("transaction_id"), str)
            and len(value["transaction_id"]) == 32
            and set(value["transaction_id"]) <= _CLEANUP_HEX
            and isinstance(value.get("label"), str) and 0 < len(value["label"]) <= 128
            and isinstance(value.get("authorization_sha256"), str)
            and len(value["authorization_sha256"]) == 64
            and set(value["authorization_sha256"]) <= _CLEANUP_HEX
            and value.get("state") in _CLEANUP_STATES,
            "cleanup journal fields are invalid")
    value["target"] = _cleanup_identity(value["target"], "cleanup target")
    value["quarantine"] = _cleanup_identity(value["quarantine"], "cleanup quarantine")
    require(value["target"]["path"] != value["quarantine"]["path"],
            "cleanup target and quarantine must differ")
    require((value["target"]["device"], value["target"]["inode"]) ==
            (value["quarantine"]["device"], value["quarantine"]["inode"]),
            "cleanup quarantine identity is not bound to its target")
    return value


def _sync_cleanup_journal(path):
    """Make one validated journal inode and its directory entry durable."""
    metadata = path.lstat()
    require(stat.S_ISREG(metadata.st_mode) and not stat.S_ISLNK(metadata.st_mode)
            and metadata.st_size <= _CLEANUP_JOURNAL_BOUND,
            "cleanup journal is invalid: " + str(path))
    descriptor = -1
    parent_fd = -1
    try:
        descriptor = os.open(
            path,
            os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0),
        )
        opened = os.fstat(descriptor)
        require(stat.S_ISREG(opened.st_mode)
                and (opened.st_dev, opened.st_ino) == (metadata.st_dev, metadata.st_ino),
                "cleanup journal changed before synchronization")
        os.fsync(descriptor)
        parent_fd, _ = _open_parent_directory(path.parent)
        visible = os.stat(path.name, dir_fd=parent_fd, follow_symlinks=False)
        require(stat.S_ISREG(visible.st_mode) and not stat.S_ISLNK(visible.st_mode)
                and (visible.st_dev, visible.st_ino) == (opened.st_dev, opened.st_ino),
                "cleanup journal changed during synchronization")
        os.fsync(parent_fd)
    finally:
        _close_parent_descriptor(descriptor)
        _close_parent_descriptor(parent_fd)


def cleanup_journal_digest(path) -> str:
    """Return the digest of one validated cleanup journal."""
    path = _cleanup_validate_journal_path(path)
    _cleanup_read_journal(path)
    value = hashlib.sha256()
    with path.open("rb") as stream:
        data = stream.read(_CLEANUP_JOURNAL_BOUND + 1)
    require(len(data) <= _CLEANUP_JOURNAL_BOUND, "cleanup journal exceeds its bound")
    value.update(data)
    return value.hexdigest()


def read_directory_cleanup(journal_path, authorization_sha256=None):
    """Read one cleanup journal without advancing it."""
    journal_path = _cleanup_validate_journal_path(journal_path)
    lock_fd = _cleanup_lock(journal_path)
    try:
        value = _cleanup_read_journal(journal_path)
        if authorization_sha256 is not None:
            require(value["authorization_sha256"] == authorization_sha256,
                    "cleanup authorization digest changed")
        return value
    finally:
        _cleanup_unlock(lock_fd)


def prepare_directory_cleanup(journal_path, label, target_identity, quarantine_parent,
                              authorization_sha256):
    """Create or validate one durable exact-directory cleanup transaction."""
    journal_path = _cleanup_validate_journal_path(journal_path)
    target = _cleanup_identity(target_identity, "cleanup target")
    quarantine_parent = Path(quarantine_parent).absolute()
    require(isinstance(authorization_sha256, str) and len(authorization_sha256) == 64
            and set(authorization_sha256) <= _CLEANUP_HEX,
            "cleanup authorization digest is invalid")
    require(not (Path(target["path"]) == journal_path
                 or Path(target["path"]) in journal_path.parents),
            "cleanup journal must be outside the deletion target")
    lock_fd = _cleanup_lock(journal_path)
    try:
        if journal_path.exists():
            value = _cleanup_read_journal(journal_path)
            require(value["label"] == label and value["authorization_sha256"] == authorization_sha256
                    and value["target"] == target,
                    "cleanup journal conflicts with the requested transaction")
            _sync_cleanup_journal(journal_path)
            return value

        target_owner = DirectoryOwner()
        target_token = target_owner.adopt_created(
            "cleanup target", Path(target["path"]), Path(target["parent"]),
        )
        require(_cleanup_identity_matches_token(target, target_token),
                "cleanup target identity changed before preparation")
        transaction_id = secrets.token_hex(16)
        quarantine = quarantine_parent / (".delegate-cleanup-" + transaction_id)
        quarantine_parent_fd, quarantine_parent_identity = _open_parent_directory(
            quarantine_parent,
        )
        try:
            require(target["device"] == quarantine_parent_identity[0],
                    "cleanup quarantine must share the target filesystem")
            try:
                os.stat(quarantine.name, dir_fd=quarantine_parent_fd, follow_symlinks=False)
            except FileNotFoundError:
                pass
            else:
                raise RuntimeError("cleanup quarantine path is already occupied")
            quarantine_identity = {
                "path": str(quarantine.absolute()),
                "parent": str(quarantine_parent),
                "device": target["device"],
                "inode": target["inode"],
                "parent_device": quarantine_parent_identity[0],
                "parent_inode": quarantine_parent_identity[1],
            }
            value = {
                "schema_version": _CLEANUP_JOURNAL_VERSION,
                "kind": _CLEANUP_JOURNAL_KIND,
                "transaction_id": transaction_id,
                "label": str(label),
                "authorization_sha256": authorization_sha256,
                "state": "prepared",
                "target": target,
                "quarantine": quarantine_identity,
            }
            write_json(journal_path, value)
        except BaseException:
            target_owner.release()
            raise
        finally:
            os.close(quarantine_parent_fd)
        target_owner.release()
        return value
    finally:
        _cleanup_unlock(lock_fd)


def _cleanup_isolate(value):
    target = value["target"]
    quarantine = value["quarantine"]
    target_state = _cleanup_path_state(target)
    quarantine_state = _cleanup_path_state(quarantine)
    require(quarantine_state in {"absent", "exact"},
            "cleanup quarantine identity changed")
    quarantine_parent_fd = _cleanup_open_parent(quarantine)
    target_parent_fd = _cleanup_open_parent(target)
    try:
        if target_state == "exact":
            require(quarantine_state == "absent",
                    "cleanup target and quarantine both exist before isolation")
            source = os.stat(Path(target["path"]).name, dir_fd=target_parent_fd,
                             follow_symlinks=False)
            require((source.st_dev, source.st_ino) == (target["device"], target["inode"]),
                    "cleanup target identity changed before isolation")
            _rename_noreplace(
                target_parent_fd, Path(target["path"]).name,
                quarantine_parent_fd, Path(quarantine["path"]).name,
            )
            os.fsync(target_parent_fd)
            os.fsync(quarantine_parent_fd)
        elif target_state == "absent":
            require(quarantine_state == "exact",
                    "cleanup target disappeared before durable isolation")
        else:
            raise RuntimeError("cleanup target was replaced before isolation")
        os.fsync(target_parent_fd)
        os.fsync(quarantine_parent_fd)
        moved = os.stat(
            Path(quarantine["path"]).name,
            dir_fd=quarantine_parent_fd,
            follow_symlinks=False,
        )
        require(stat.S_ISDIR(moved.st_mode) and not stat.S_ISLNK(moved.st_mode)
                and (moved.st_dev, moved.st_ino) == (target["device"], target["inode"]),
                "cleanup quarantine does not contain the exact target")
    finally:
        _close_parent_descriptor(target_parent_fd)
        os.close(quarantine_parent_fd)


def _cleanup_empty(value):
    target = value["target"]
    quarantine = value["quarantine"]
    require(_cleanup_path_state(target) == "absent",
            "cleanup target path is occupied after isolation")
    require(_cleanup_path_state(quarantine) == "exact",
            "cleanup quarantine identity changed after isolation")
    quarantine_parent_fd, quarantine_fd = _cleanup_open_directory(quarantine)
    try:
        _cleanup_clear_contents(quarantine_fd)
    finally:
        os.close(quarantine_fd)
        os.close(quarantine_parent_fd)


def _cleanup_remove_empty(value):
    target = value["target"]
    quarantine = value["quarantine"]
    require(_cleanup_path_state(target) == "absent",
            "cleanup target path is occupied before completion")
    quarantine_state = _cleanup_path_state(quarantine)
    if quarantine_state == "absent":
        parent_fd = _cleanup_open_parent(quarantine)
        try:
            os.fsync(parent_fd)
        finally:
            os.close(parent_fd)
        return
    require(quarantine_state == "exact", "cleanup quarantine was replaced before completion")
    quarantine_parent_fd, quarantine_fd = _cleanup_open_directory(quarantine)
    try:
        require(not os.listdir(quarantine_fd), "cleanup quarantine is not empty")
        os.fsync(quarantine_fd)
        current = os.stat(Path(quarantine["path"]).name, dir_fd=quarantine_parent_fd,
                          follow_symlinks=False)
        require((current.st_dev, current.st_ino) ==
                (quarantine["device"], quarantine["inode"]),
                "cleanup quarantine changed before removal")
        os.close(quarantine_fd)
        quarantine_fd = -1
        os.rmdir(Path(quarantine["path"]).name, dir_fd=quarantine_parent_fd)
        os.fsync(quarantine_parent_fd)
    finally:
        _close_parent_descriptor(quarantine_fd)
        os.close(quarantine_parent_fd)


def resume_directory_cleanup(journal_path, authorization_sha256, expected_target_identity):
    """Advance one cleanup journal through the shared crash-recovery policy."""
    journal_path = _cleanup_validate_journal_path(journal_path)
    expected_target = _cleanup_identity(expected_target_identity, "expected cleanup target")
    lock_fd = _cleanup_lock(journal_path)
    try:
        value = _cleanup_read_journal(journal_path)
        require(value["authorization_sha256"] == authorization_sha256,
                "cleanup authorization digest changed")
        require(value["target"] == expected_target,
                "cleanup target identity changed")
        _sync_cleanup_journal(journal_path)
        while value["state"] != "complete":
            if value["state"] == "prepared":
                _cleanup_isolate(value)
                value = {**value, "state": "isolated"}
            elif value["state"] == "isolated":
                _cleanup_empty(value)
                value = {**value, "state": "contents-removed"}
            elif value["state"] == "contents-removed":
                _cleanup_remove_empty(value)
                value = {**value, "state": "complete"}
            else:
                raise RuntimeError("cleanup journal state is unsupported")
            write_json(journal_path, value, replace=True)
        return value
    finally:
        _cleanup_unlock(lock_fd)


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


def failure_cleanup_deadline(timeout):
    """Return one finite monotonic deadline for failure-path observation."""
    require(type(timeout) in (int, float) and math.isfinite(timeout) and timeout > 0,
            "failure cleanup timeout must be finite and positive")
    return time.monotonic() + float(timeout)


def wait_for_failure_cleanup(deadline, delay):
    """Sleep within a cleanup deadline; return false when its budget is spent."""
    require(type(deadline) in (int, float) and math.isfinite(deadline),
            "failure cleanup deadline is invalid")
    require(type(delay) in (int, float) and math.isfinite(delay) and delay >= 0,
            "failure cleanup delay is invalid")
    remaining = float(deadline) - time.monotonic()
    if remaining <= 0:
        return False
    time.sleep(min(float(delay), remaining))
    return True


def finalize_acceptance_artifacts(
        output: Path,
        supervisor_closed: bool,
        owned_directories: tuple[OwnedDirectory, ...],
        success: bool,
        remove_failed_process_captures: bool = False,
        raw_process_captures_token: OwnedDirectory | None = None,
        timeout: float = 30) -> bool:
    """Clean one completed acceptance run without crossing an ownership boundary.

    ``owned_directories`` is the invocation's explicit creation-order sequence
    of identity tokens. When raw captures exist, ``raw_process_captures_token``
    must be that exact token and must already be present in the sequence. A
    private supervisor must have been positively observed closed before any
    token is removed. The compact evidence directory remains; a successful run
    drops only its large raw process captures. Failed runs may opt in to the
    same removal after a caller has persisted an independent, bounded
    diagnostic snapshot.
    """
    output = Path(output)
    require(type(timeout) in (int, float) and math.isfinite(timeout) and timeout > 0,
            "acceptance finalization timeout must be finite and positive")
    require(type(remove_failed_process_captures) is bool,
            "failed process capture cleanup mode is invalid")
    if raw_process_captures_token is not None:
        require(isinstance(raw_process_captures_token, OwnedDirectory),
                "raw process capture identity token is invalid")
    require(not remove_failed_process_captures or not success,
            "failed process capture cleanup is only valid for failed runs")
    try:
        output_mode = output.lstat().st_mode
        require(stat.S_ISDIR(output_mode) and not stat.S_ISLNK(output_mode),
                "acceptance evidence directory is unsafe: " + str(output))
        entries = tuple(owned_directories)
        require(all(isinstance(entry, OwnedDirectory) for entry in entries),
                "acceptance cleanup requires identity tokens")
        if raw_process_captures_token is not None:
            require(any(entry is raw_process_captures_token for entry in entries),
                    "raw process capture token is not in the ownership sequence")
        paths = [entry.directory for entry in entries]
        require(len(paths) == len(set(paths)), "acceptance cleanup has duplicate owned directories")
        require(all(directory != output and directory not in output.parents for directory in paths),
                "acceptance cleanup cannot remove its evidence directory")
        raw_path = output / "processes"
        if (raw_path.exists() or raw_path.is_symlink()) and raw_process_captures_token is None:
            raise RuntimeError("raw process capture identity token is required")
        if raw_process_captures_token is not None:
            require(raw_process_captures_token.directory == raw_path,
                    "raw process capture token does not identify output/processes")
            require(raw_process_captures_token.parent == output,
                    "raw process capture token parent does not identify output")
            output_identity = os.stat(output, follow_symlinks=False)
            require((output_identity.st_dev, output_identity.st_ino) ==
                    raw_process_captures_token.parent_identity,
                    "raw process capture token parent identity does not match output")
    except (OSError, RuntimeError):
        # Cleanup is never allowed to replace the result it is preserving.
        # Callers turn this false result into a failed successful run, while a
        # failure path retains its original diagnostic and leaves all state.
        return False
    receipt = {
        "supervisor_shutdown_observed": bool(supervisor_closed),
        "completion": "passed" if success else "failed",
        "removed": [],
        "raw_process_captures": "retained",
    }
    if not supervisor_closed:
        receipt["status"] = "retained"
        receipt["reason"] = "private supervisor shutdown was not positively observed"
        write_json(output / "cleanup.json", receipt, replace=(output / "cleanup.json").exists())
        return False

    deadline = failure_cleanup_deadline(timeout)
    failures = []
    failed_entries = []
    retained = []
    raw_removed = False
    raw_authorized = success or remove_failed_process_captures
    ordered_entries = tuple(reversed(entries))
    for index, entry in enumerate(ordered_entries):
        try:
            _check_cleanup_deadline(deadline)
        except RuntimeError as error:
            failures.append({"label": entry.label, "error": str(error)})
            for retained_entry in ordered_entries[index:]:
                try:
                    retained_entry.consume()
                    retained.append(retained_entry.label)
                except RuntimeError as consume_error:
                    failures.append({"label": retained_entry.label,
                                     "error": str(consume_error)})
            break
        if any(_is_ancestor_entry(entry, failed) for failed in failed_entries):
            try:
                release_owned_directory(entry, deadline=deadline)
            except (OSError, RuntimeError) as error:
                failures.append({
                    "label": entry.label,
                    "error": "ancestor retained after descendant cleanup failure: " + str(error),
                })
            retained.append(entry.label)
            continue
        if entry is raw_process_captures_token and not raw_authorized:
            try:
                release_owned_directory(entry, deadline=deadline)
                retained.append(entry.label)
            except (OSError, RuntimeError) as error:
                failures.append({"label": entry.label, "error": str(error)})
            continue
        try:
            remove_owned_directory(entry, deadline=deadline)
            receipt["removed"].append(entry.label)
            if entry is raw_process_captures_token:
                raw_removed = True
        except (OSError, RuntimeError) as error:
            failures.append({"label": entry.label, "error": str(error)})
            failed_entries.append(entry)
    if raw_process_captures_token is not None and raw_removed:
        receipt["raw_process_captures"] = "removed"
    if retained:
        receipt["retained"] = retained
    if failures:
        receipt["status"] = "partial"
        receipt["failures"] = failures
    else:
        receipt["status"] = "removed"
    write_json(output / "cleanup.json", receipt, replace=(output / "cleanup.json").exists())
    return not failures


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
            executable = Path(self.argv[0])
            require(executable.is_absolute(), "executable must be absolute")
            invocation = {
                "argv": self.argv,
                "cwd": str(cwd),
                "executable_sha256": digest(executable),
                "stdin": "PIPE" if stdin is not None else "DEVNULL",
                "stdin_bytes": len(self._stdin_data) if self._stdin_data is not None else 0,
                "stdin_sha256": sha(self._stdin_data) if self._stdin_data is not None else None,
                "shell": False,
            }
            if environment_keys_only:
                invocation.update({
                    "environment_keys": sorted(self.environment),
                    "environment_values_in_record": False,
                    "harness_environment_preserved": True,
                })
            else:
                invocation["environment_sha256"] = sha(
                    json.dumps(self.environment, sort_keys=True).encode())
            write_json(self.directory / "invocation.json", invocation)
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
            if self.process is not None and getattr(self.process, "stdin", None) is not None:
                try:
                    self.process.stdin.close()
                except (BrokenPipeError, OSError, ValueError):
                    pass
            if self._stdout_capture is not None:
                try:
                    self._stdout_capture.writer.close()
                except (OSError, ValueError):
                    pass
                if self.process is None:
                    self._stdout_capture.close()
            if self.stdout is not None:
                self.stdout.close()
            if self.stderr is not None:
                self.stderr.close()
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


def wait_until(predicate, timeout=15, description="condition"):
    deadline = time.monotonic() + timeout
    while True:
        value = predicate()
        if value:
            return value
        require(time.monotonic() < deadline, "observation expired: " + description)
        time.sleep(0.05)
