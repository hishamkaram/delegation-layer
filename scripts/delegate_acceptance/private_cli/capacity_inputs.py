"""Immutable invocation inputs and observations for one private capacity cohort."""

from dataclasses import dataclass
from pathlib import Path
from scripts.delegate_acceptance.directories.operations import OwnedDirectory
from scripts.delegate_acceptance.private_cli.leases import LockedReleaseGate, LiveFIFOLeaseCohort
from scripts.delegate_acceptance.private_cli.receipt import AcceptanceReceipt


@dataclass(frozen=True)
class CapacityRequest:
    delegate: Path
    state: Path
    workspace: Path
    brief: Path
    release_gate: LockedReleaseGate
    waiter_directory: Path
    env: dict[str, str]
    old_layout: Path
    old_layout_owned: tuple[OwnedDirectory, ...]
    client: Path
    config: Path
    parallel_tasks: int
    dispatch_workers: int
    response_directory: Path
    evidence: dict[str, object] | None
    lease_nonce: str
    client_call_log: Path
    attempted_task_ids: list[str]
    capture_root: OwnedDirectory
    run_receipt: AcceptanceReceipt | None


@dataclass(frozen=True)
class CapacityDispatchResult:
    responses: list[dict[str, object]]
    task_ids: list[str]
    expected_labels: set[str]
    dispatch_batch: dict[str, int | float]


@dataclass
class CapacityLeaseState:
    """Finalizer reference to the one canonical cohort; never a second lease registry."""

    cohort: LiveFIFOLeaseCohort | None = None
