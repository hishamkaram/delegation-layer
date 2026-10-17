"""Private CLI test-owned capture and release-gate fixtures."""

from pathlib import Path
import tempfile
from scripts import acceptance_private_cli as private_cli
from scripts import acceptance_supervisor_common as common


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
