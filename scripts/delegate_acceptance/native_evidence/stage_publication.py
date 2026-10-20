"""Identity-pinned, create-once publication of a reconstructed staging plan."""

from __future__ import annotations

from dataclasses import dataclass
import os
from pathlib import Path
import stat

from ..directories.journal import (
    acquire_exclusive_file_lock, directory_identity_record, release_exclusive_file_lock,
)
from ..directories.operations import (
    DirectoryOwner, OwnedDirectory, discard_creation_temporary, is_creation_temporary_name,
)
from ..evidence.files import is_staging_file_for, write_bytes
from .contract import EvidenceError, MAX_RECEIPT_BYTES, PUBLIC_STAGING_MAX_ENTRIES
from .filesystem import _bounded_file_bytes, _reject_path_components


@dataclass(frozen=True)
class StageFile:
    name: str
    data: bytes
    limit: int


@dataclass(frozen=True)
class StageTemporary:
    parent: str
    name: str
    directory: bool


class StagePlan:
    """The staging builder alone adds bounded, immutable expected file bytes."""

    def __init__(self, output: Path):
        self.output = output
        self._files: dict[str, StageFile] = {}

    def put(self, path: Path, data: bytes, limit: int = MAX_RECEIPT_BYTES) -> None:
        name = path.relative_to(self.output).as_posix()
        if (name in self._files or ".." in Path(name).parts or Path(name).is_absolute()
                or len(data) > limit or len(self._files) >= PUBLIC_STAGING_MAX_ENTRIES):
            raise EvidenceError("stage_plan_invalid")
        self._files[name] = StageFile(name, data, limit)

    def files(self) -> tuple[StageFile, ...]:
        return tuple(self._files.values())


def _directories(files: tuple[StageFile, ...]) -> set[str]:
    result = {""}
    for item in files:
        parent = Path(item.name).parent
        while parent != Path("."):
            result.add(parent.as_posix())
            parent = parent.parent
    return result


def _open_directory(owner: DirectoryOwner, path: Path) -> OwnedDirectory:
    if path.exists() or path.is_symlink():
        return owner.adopt_created("native stage directory", path, path.parent)
    return owner.create("native stage directory", path, path.parent)


def _temporary_link_is_owned(path: Path, metadata: os.stat_result, destination: Path) -> bool:
    if metadata.st_nlink == 1:
        return True
    if metadata.st_nlink != 2:
        return False
    try:
        committed = destination.lstat()
    except FileNotFoundError:
        return False
    return (stat.S_ISREG(committed.st_mode)
            and (metadata.st_dev, metadata.st_ino) == (committed.st_dev, committed.st_ino)
            and path.parent == destination.parent)


def _inventory(root: OwnedDirectory, files: tuple[StageFile, ...]) -> tuple[set[str], list[StageTemporary]]:
    expected = {item.name: item for item in files}
    directories = _directories(files)
    observed: set[str] = set()
    temporary: list[StageTemporary] = []
    pending = [(root.directory, "")]
    visited = 0
    while pending:
        path, relative = pending.pop()
        root.assert_live()
        with os.scandir(path) as entries:
            for entry in entries:
                visited += 1
                if visited > PUBLIC_STAGING_MAX_ENTRIES:
                    raise EvidenceError("stage_inventory_too_large")
                name = str(Path(relative) / entry.name)
                metadata = entry.stat(follow_symlinks=False)
                if stat.S_ISLNK(metadata.st_mode):
                    raise EvidenceError("stage_inventory_symlink")
                if stat.S_ISDIR(metadata.st_mode) and name in directories:
                    pending.append((Path(entry.path), name))
                elif stat.S_ISDIR(metadata.st_mode) and is_creation_temporary_name(entry.name):
                    with os.scandir(entry.path) as children:
                        if next(children, None) is not None:
                            raise EvidenceError("stage_directory_temporary_nonempty")
                    temporary.append(StageTemporary(relative, entry.name, True))
                elif stat.S_ISREG(metadata.st_mode) and name in expected:
                    item = expected[name]
                    if _bounded_file_bytes(Path(entry.path), item.limit) != item.data:
                        raise EvidenceError("stage_file_conflict")
                    observed.add(name)
                elif stat.S_ISREG(metadata.st_mode):
                    matches = [item for item in files if Path(item.name).parent == Path(relative)
                               and is_staging_file_for(Path(item.name), entry.name)]
                    if (len(matches) != 1 or metadata.st_size > matches[0].limit
                            or not _temporary_link_is_owned(
                                Path(entry.path), metadata, root.directory / matches[0].name)):
                        raise EvidenceError("stage_inventory_unexpected")
                    temporary.append(StageTemporary(relative, entry.name, False))
                else:
                    raise EvidenceError("stage_inventory_unexpected")
    root.assert_live()
    return observed, temporary


def _publish_file(root: OwnedDirectory, parent: OwnedDirectory, item: StageFile) -> None:
    root.assert_live()
    parent.assert_live()
    destination = parent.directory / Path(item.name).name
    try:
        destination.lstat()
    except FileNotFoundError:
        pass
    else:
        existing = _bounded_file_bytes(destination, item.limit)
        if existing != item.data:
            raise EvidenceError("stage_file_conflict")
        os.fsync(parent.directory_fd)
        return
    try:
        write_bytes(destination, item.data)
    except (OSError, RuntimeError) as error:
        try:
            committed = _bounded_file_bytes(destination, item.limit)
        except EvidenceError as read_error:
            raise error from read_error
        if committed != item.data:
            raise EvidenceError("stage_file_conflict") from error
    root.assert_live()
    parent.assert_live()
    os.fsync(parent.directory_fd)


def publish_stage(plan: StagePlan, directory_identity: dict[str, object] | None) -> None:
    """A verified destination lease owns reconciliation; manifest is the commit."""
    output, files = plan.output, plan.files()
    if not files or files[-1].name != "manifest.json":
        raise EvidenceError("stage_plan_manifest_missing")
    _reject_path_components(output)
    if directory_identity is None:
        output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    lease = acquire_exclusive_file_lock(output.parent / ("." + output.name + ".lock"))
    owner = DirectoryOwner()
    try:
        root = (owner.adopt_created("native stage directory", output, output.parent)
                if directory_identity is not None else _open_directory(owner, output))
        if directory_identity is not None and directory_identity_record(root) != directory_identity:
            raise EvidenceError("stage_directory_identity_changed")
        if directory_identity is None and any(output.iterdir()):
            raise EvidenceError("stage_output_not_empty")
        observed, temporary = _inventory(root, files)
        committed = "manifest.json" in observed
        if committed and observed != {item.name for item in files}:
            raise EvidenceError("stage_committed_inventory_incomplete")
        pins = {"": root}
        for name in sorted(_directories(files) - {""}, key=lambda value: len(Path(value).parts)):
            root.assert_live()
            pins[name] = _open_directory(owner, output / name)
        for entry in temporary:
            root.assert_live()
            parent = pins[entry.parent]
            parent.assert_live()
            if entry.directory:
                discard_creation_temporary(parent, entry.name)
            else:
                os.unlink(entry.name, dir_fd=parent.directory_fd)
                os.fsync(parent.directory_fd)
        for item in files:
            if item.name == "manifest.json":
                actual, leftovers = _inventory(root, files)
                required = {entry.name for entry in files if entry.name != "manifest.json"}
                if actual - {"manifest.json"} != required or leftovers:
                    raise EvidenceError("stage_precommit_inventory_invalid")
                os.fsync(root.directory_fd)
            parent = Path(item.name).parent
            parent_name = "" if parent == Path(".") else parent.as_posix()
            _publish_file(root, pins[parent_name], item)
        actual, leftovers = _inventory(root, files)
        if actual != {item.name for item in files} or leftovers:
            raise EvidenceError("stage_committed_inventory_invalid")
    finally:
        try:
            owner.release()
        finally:
            release_exclusive_file_lock(lease)
