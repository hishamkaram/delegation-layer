"""Finalizer references for one private CLI fixture; canonical owners keep resources.

The entrypoint mutates this ledger as it acquires resources. Scenario and cleanup
operations read the same handles, so a failed setup cannot lose a resource.
"""

from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
import subprocess
from typing import TypedDict, NotRequired, Literal

from scripts.delegate_acceptance.directories.operations import DirectoryOwner, OwnedDirectory
from scripts.delegate_acceptance.private_cli.leases import DisposalLeases, LockedReleaseGate, _HeldDisposal
from scripts.delegate_acceptance.private_cli.supervisor import PredecessorShutdownTarget, SelectedOwnerSubject


class SupervisorCleanup(TypedDict):
    delegate: Path
    client: Path
    config: Path
    env: dict[str, str]
    state: Path
    task_ids: list[str]
    root_id: NotRequired[str]
    owner: NotRequired[SelectedOwnerSubject | None | Literal["unresolved"]]
    daemon: NotRequired[tuple[int, Path, tuple[int, int]]]


class PredecessorCleanup(TypedDict):
    """A pre-retarget generation; it replaces the mutable primary cleanup for its root."""

    client: Path
    env: dict[str, str]
    task_ids: list[str]
    predecessor: PredecessorShutdownTarget


class ClosedRoot(TypedDict):
    """Shutdown's authorizing closure borrowed under the exact still-held disposal lease."""

    closure: dict[str, object]
    disposal: _HeldDisposal


@dataclass
class FixtureLifetime:
    ownership: DirectoryOwner = field(default_factory=DirectoryOwner)
    old_layout_ownership: DirectoryOwner = field(default_factory=DirectoryOwner)
    old_layout_owned: tuple[OwnedDirectory, ...] = ()
    base: Path | None = None
    base_token: OwnedDirectory | None = None
    # Private control capture files live in this owned child, never directly
    # in base: base is an ancestor of the provider execution views.
    capture_token: OwnedDirectory | None = None
    inspection_process: subprocess.Popen[str] | None = None
    env: dict[str, str] = field(default_factory=dict)
    private_config: Path | None = None
    gate: Path | None = None
    release_gate: LockedReleaseGate | None = None
    # The installation owner supplies this path; cleanup never infers it from Pueue or PATH.
    delegate: Path | None = None
    disposal: DisposalLeases = field(default_factory=DisposalLeases)
    old_install: Path | None = None
    old_libexec: Path | None = None
    upgraded_libexec: Path | None = None
    supervisor_cleanups: list[SupervisorCleanup | PredecessorCleanup] = field(default_factory=list)
    managed_state_roots: list[tuple[Path, list[str]]] = field(default_factory=list)
    # Scenario cleanup adds a root only after its owner and daemon fully close.
    # Finalization reads this ledger to avoid new observation or double teardown.
    closed_roots: dict[Path, ClosedRoot | None] = field(default_factory=dict)
    handoff_cleanup_bindings: list[SupervisorCleanup] = field(default_factory=list)
    attempted_task_ids: list[str] = field(default_factory=list)
    # Bound only by a composition whose selected Delegate CLI and attempted IDs
    # are known created records for the primary root; None keeps passive cleanup.
    # It takes a task ID and the absolute monotonic cleanup deadline.
    primary_observer: Callable[[str, float], str] | None = None
    # Scenario-owned preparation runs before primary readiness, under the same
    # absolute deadline. It must close child roots before parent disposal.
    prepare_cleanup: Callable[[float, DisposalLeases], None] | None = None
    # The canonical reader remains the default. A scenario may bind its
    # explicit canonical Go never-launched proof port; no result is cached.
    closure_reader: Callable[[Path, tuple[str, ...]], dict[str, object]] | None = None
