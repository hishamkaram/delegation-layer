"""Durable cleanup transactions and replay through held directory identities."""

from __future__ import annotations

import fcntl
import hashlib
import os
import secrets
import stat
from scripts.delegate_acceptance.directories.operations import (
    DirectoryOwner,
    OwnedDirectory,
    _DIRECTORY_OPEN_FLAGS,
    _close_parent_descriptor,
    _open_parent_directory,
    _rename_noreplace,
)
from scripts.delegate_acceptance.directories.policy import (
    _cleanup_identity,
    _cleanup_identity_matches_token,
    require,
)
from scripts.delegate_acceptance.evidence.files import read_json, write_json
from pathlib import Path


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
        try:
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
            finally:
                os.close(quarantine_parent_fd)
            target_owner.release()
            return value
        finally:
            if not target_token._consumed:
                target_token.consume()
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
