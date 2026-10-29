"""Private CLI test-owned capture, release-gate, and custody pool layout fixtures."""

from contextlib import contextmanager
import os
from pathlib import Path
import stat
import tempfile
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts import acceptance_supervisor_common as common


# Go's verifiedexec pool marker: its file name and the exact mode taskdir.CheckPrivateStat accepts.
POOL_MARKER_NAME = ".identity"
POOL_MARKER_MODE = 0o600
# The mode verifiedexec creates pool root, views and owner directories with; Go
# accepts any mode without group or world bits.
POOL_DIRECTORY_MODE = 0o700


def _release_live_directory_tokens(entries):
    """Release only unconsumed test authorities through the shared owner."""
    for entry in reversed(tuple(entries)):
        if not getattr(entry, "_consumed", False):
            common.release_owned_directory(entry)


def set_up_private_capture_root(test_case):
    temporary = tempfile.TemporaryDirectory(prefix="delegate-capture-root-test-")
    test_case.addCleanup(temporary.cleanup)
    root = Path(temporary.name)
    test_case.capture_owner = private_cli.DirectoryOwner()
    test_case.capture_root = test_case.capture_owner.create(
        "private control capture root", root / "capture-root", root,
    )
    test_case.addCleanup(test_case.capture_owner.rollback)


class RecordingReleaseGate:
    def __init__(self, path):
        self.path = Path(path)

    def release(self):
        self.path.touch(mode=0o600, exist_ok=True)


def create_private_pool_directory(path, mode=POOL_DIRECTORY_MODE):
    """Create a pool root, views or owner directory and its missing parents; give it mode, private by default."""
    path = Path(path)
    path.mkdir(parents=True)
    path.chmod(mode)
    return path


@contextmanager
def reported_as_another_user(path):
    """Report path's lstat with another owner, which a test cannot chown; every other path keeps its metadata."""
    real_lstat = Path.lstat
    target = Path(path)

    def lstat(self):
        info = real_lstat(self)
        if self != target:
            return info
        fields = list(info[:10])
        fields[stat.ST_UID] = os.geteuid() + 1
        return os.stat_result(fields)

    with patch.object(Path, "lstat", lstat):
        yield


def canonical_pool_marker(pool_id):
    """The bytes Go's encodePoolIdentity publishes, written out literally rather than through the reader under test."""
    return b'{"version":1,"pool_id":"%s"}' % pool_id.encode()


def write_pool_marker(pool_root, raw, mode=POOL_MARKER_MODE):
    """Replace pool_root's marker with raw bytes in a regular file of this user with mode, private by default."""
    marker = Path(pool_root) / POOL_MARKER_NAME
    marker.unlink(missing_ok=True)
    marker.write_bytes(raw)
    marker.chmod(mode)
    return marker
