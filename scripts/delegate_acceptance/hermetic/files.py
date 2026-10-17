"""Fixture input files and executable observations."""

from pathlib import Path
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.hermetic.contract import require_true
import os
import shutil
import uuid


def write_bytes(path, data, replace=False):
    """Write one bounded evidence/input file with a durable close."""
    path = Path(path)
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    if replace:
        stage = path.with_name(path.name + ".staging")
        with stage.open("xb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(stage, path)
    else:
        with path.open("xb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
    path.chmod(0o600)


def canonical_executable(path):
    path = Path(path)
    require_true(path.is_absolute(), "acceptance executable must be absolute")
    resolved = path.resolve(strict=True)
    require_true(resolved == path, "acceptance executable must be canonical: " + str(path))
    require_true(path.is_file() and os.access(path, os.X_OK),
                 "acceptance executable is not executable: " + str(path))
    return path


def copy_executable(source, destination):
    source = canonical_executable(source)
    destination = Path(destination)
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    shutil.copyfile(source, destination)
    destination.chmod(0o700)
    require_true(digest(source) == digest(destination),
                 "copied executable digest changed: " + str(destination))
    require_true(destination.resolve(strict=True) == destination,
                 "copied executable is not canonical: " + str(destination))
    return destination


def task_id():
    return uuid.uuid4().hex


def owned_directory_path(token):
    """Return the path carried by one transferred ownership token."""
    return Path(getattr(token, "directory", token))
