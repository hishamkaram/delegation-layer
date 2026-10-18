"""Private invocation placement and identity checks over canonical owners."""

from __future__ import annotations

import os
from pathlib import Path
import secrets
import stat
import tempfile

from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.release_verification.assets import platform_identity
from .contract import Invocation, ReleaseError, ReleaseRequest, integer, text_value
from .receipt import Receipt


def safe_path(path: Path) -> None:
    if not path.is_absolute() or ".." in path.parts:
        raise ReleaseError("release_path_not_absolute_or_clean")
    current = Path(path.anchor)
    for part in path.parts[1:]:
        current /= part
        try:
            info = current.lstat()
        except FileNotFoundError:
            break
        if stat.S_ISLNK(info.st_mode):
            raise ReleaseError("release_path_symlink")


def state_base(checkout: Path) -> Path:
    base = Path.home().resolve() / ".local" / "state" / "delegation-layer-release"
    safe_path(base)
    temporary = (Path("/tmp").resolve(), Path(tempfile.gettempdir()).resolve())
    if (base.is_relative_to(checkout) or checkout.is_relative_to(base)
            or any(base.is_relative_to(root) for root in temporary)):
        raise ReleaseError("release_state_placement_unsafe")
    return base


def validate_identity(identity: dict[str, object], path: Path) -> None:
    safe_path(path)
    if (identity.get("path") != str(path) or identity.get("parent") != str(path.parent)):
        raise ReleaseError("release_directory_record_mismatch", incomplete=True)
    current, parent = path.lstat(), path.parent.lstat()
    expected = (integer(identity.get("device")), integer(identity.get("inode")))
    expected_parent = (integer(identity.get("parent_device")), integer(identity.get("parent_inode")))
    if (not stat.S_ISDIR(current.st_mode) or not stat.S_ISDIR(parent.st_mode)
            or (current.st_dev, current.st_ino) != expected
            or (parent.st_dev, parent.st_ino) != expected_parent):
        raise ReleaseError("release_directory_identity_changed", incomplete=True)


def create_receipt(checkout: Path, request: ReleaseRequest, argv: list[str]) -> Receipt:
    base = state_base(checkout)
    base.mkdir(mode=0o700, parents=True, exist_ok=True)
    safe_path(base)
    invocation_id = secrets.token_hex(16)
    root = base / f"{request.tag}-{invocation_id}"
    platform = platform_identity()
    owner = DirectoryOwner()
    try:
        token = owner.create("native release invocation", root, base)
        invocation = Invocation(
            invocation_id=invocation_id, request=request, checkout=checkout, root=root,
            platform_os=text_value(platform["os"]), platform_arch=text_value(platform["arch"]),
            directory_identity=directory_identity_record(token),
        )
        receipt = Receipt.create(invocation, argv)
        os.chmod(receipt.path, 0o600)
        return receipt
    finally:
        owner.release()


def load_receipt(path: Path) -> Receipt:
    safe_path(path)
    receipt = Receipt.load(path)
    invocation = receipt.invocation
    if path.parent.parent != state_base(invocation.checkout):
        raise ReleaseError("release_receipt_outside_invocation_root")
    validate_identity(invocation.directory_identity, invocation.root)
    return receipt
