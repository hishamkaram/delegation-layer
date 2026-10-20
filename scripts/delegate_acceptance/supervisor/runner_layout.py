"""Pure layout of the runner path a managed admission queues and binds."""

from __future__ import annotations

from scripts.delegate_acceptance.evidence.validation import _hex_digest, require
from pathlib import Path
from typing import Literal


# Go's PrepareAdmissionExecutables (internal/supervisor/setup_executables.go)
# returns the selected bundle runner for an external supervisor config. Only
# the private state-rooted supervisor installs a managed state copy.
RunnerPlacement = Literal["bundle", "state-installed"]
BUNDLE_RUNNER: RunnerPlacement = "bundle"
STATE_INSTALLED_RUNNER: RunnerPlacement = "state-installed"
RUNNER_PLACEMENTS = frozenset({BUNDLE_RUNNER, STATE_INSTALLED_RUNNER})

# Go's InstallStateRunner names the immutable copy after the source digest
# (internal/supervisor/executable_install.go, executable_records.go).
MANAGED_RUNNER_PREFIX = "delegate-run-"


def managed_state_runner(state: Path, runner_sha256: str) -> Path:
    """Return the state-installed runner path for one managed runner digest."""
    require(_hex_digest(runner_sha256), "managed runner digest is invalid")
    return Path(state) / ".supervisor" / (MANAGED_RUNNER_PREFIX + runner_sha256)
