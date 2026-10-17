"""Immutable installed fixture paths and initial private-supervisor observations."""

from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class FixtureInstallation:
    delegate_binary: Path
    runner_binary: Path
    upgraded_delegate_binary: Path
    upgraded_runner_binary: Path
    pueue_binary: Path
    pueued_binary: Path
    workspace: Path
    provider_bin: Path
    brief: Path
    upgraded_install: Path
    old_daemon_marker: Path
    upgraded_daemon_marker: Path
    state: Path
    waiter_directory: Path
    lease_nonce: str


@dataclass(frozen=True)
class InitialObservation:
    dispatch: dict[str, object]
    task_id: str
    initial_pair_directory: Path | None
    initial_daemon: Path
    identity_path: Path
    old_digest: str
    upgraded_daemon: Path
    upgraded_daemon_digest: str
    packaged_evidence: dict[str, object]
    inspection_group: str
    inspection_prefix: str
    models_args: list[str]
