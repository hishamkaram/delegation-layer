"""Pure cleanup identity rules and directory assertion contract."""

from __future__ import annotations

from pathlib import Path
import stat

# Group and world permission bits a private directory must not carry.
PRIVATE_DIRECTORY_SHARED_BITS = 0o077


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def require_private_directory(info, message, *, owner_uid):
    """Go's requirePrivateInfo over a stat result: a real directory of ``owner_uid`` without group or world bits."""
    require(
        stat.S_ISDIR(info.st_mode)
        and info.st_uid == owner_uid
        and not stat.S_IMODE(info.st_mode) & PRIVATE_DIRECTORY_SHARED_BITS,
        message,
    )


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
