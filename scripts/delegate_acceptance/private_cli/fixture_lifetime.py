"""Finalizer references for one private CLI fixture; canonical owners keep resources.

The entrypoint mutates this ledger as it acquires resources. Scenario and cleanup
operations read the same handles, so a failed setup cannot lose a resource.
"""

from dataclasses import dataclass, field
from pathlib import Path
import subprocess
from typing import TypedDict

from scripts.delegate_acceptance.directories.operations import DirectoryOwner, OwnedDirectory
from scripts.delegate_acceptance.private_cli.leases import LockedReleaseGate


class SupervisorCleanup(TypedDict):
    client: Path
    config: Path
    env: dict[str, str]
    state: Path
    task_ids: list[str]


@dataclass
class FixtureLifetime:
    ownership: DirectoryOwner = field(default_factory=DirectoryOwner)
    old_layout_ownership: DirectoryOwner = field(default_factory=DirectoryOwner)
    old_layout_owned: tuple[OwnedDirectory, ...] = ()
    base: Path | None = None
    base_token: OwnedDirectory | None = None
    inspection_process: subprocess.Popen[str] | None = None
    env: dict[str, str] = field(default_factory=dict)
    private_config: Path | None = None
    gate: Path | None = None
    release_gate: LockedReleaseGate | None = None
    old_install: Path | None = None
    old_libexec: Path | None = None
    upgraded_libexec: Path | None = None
    supervisor_cleanups: list[SupervisorCleanup] = field(default_factory=list)
    managed_state_roots: list[tuple[Path, list[str]]] = field(default_factory=list)
    handoff_cleanup_bindings: list[SupervisorCleanup] = field(default_factory=list)
    attempted_task_ids: list[str] = field(default_factory=list)
