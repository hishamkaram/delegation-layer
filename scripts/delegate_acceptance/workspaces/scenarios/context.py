"""Mutable state and exact cleanup ownership for one workspace run."""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
import threading

import scripts.delegate_acceptance.private_cli.leases as private_cli_leases
import scripts.delegate_acceptance.private_cli.supervisor as private_cli_supervisor
import scripts.delegate_acceptance.private_cli.root_cleanup as private_cli_root_cleanup
from scripts.delegate_acceptance.directories.operations import DirectoryOwner, OwnedDirectory
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.directories.policy import require
from .model import TASK_ID_RE
from .policy import cleanup_decision


class AcceptanceSetupFailure(RuntimeError):
    """Carry partial exact-directory ownership to finalization."""

    def __init__(self, cause: BaseException, owner: DirectoryOwner, root_token: OwnedDirectory):
        super().__init__(str(cause))
        self.cause = cause
        self.owner = owner
        self.root_token = root_token

    def cleanup(self) -> dict[str, object]:
        entries = self.owner.transfer()
        paths = [str(entry.directory) for entry in entries]
        try:
            private_cli_root_cleanup.remove_private_acceptance_root(self.root_token, entries)
        except (OSError, RuntimeError) as error:
            return {
                "authorized": False,
                "decision": "setup-cleanup-failed-evidence-retained",
                "error": str(error),
                "retained_owned_directories": paths,
            }
        return {
            "authorized": True,
            "decision": "setup-failure-exact-root-removed",
            "removed_owned_directories": paths,
        }


@dataclass
class WorkspaceContext:
    owner: DirectoryOwner
    root_token: OwnedDirectory
    base: Path
    state: Path
    source: Path
    brief_success: Path
    brief_success_reuse: Path
    brief_timeout: Path
    brief_timeout_resume: Path
    brief_capacity: Path
    delegate: Path
    pueue: Path
    pueued: Path
    private_config: Path
    gate_path: Path
    waiter_directory: Path
    fixture_session_marker: Path
    response_directory: Path
    capture_root: OwnedDirectory
    processes: Processes
    environment: dict[str, str]
    source_baseline: dict[str, str]
    lease_nonce: str
    workspace_budget: str
    task_ids: list[str] = field(default_factory=list)
    attempted_task_ids: list[str] = field(default_factory=list)
    workspace_ids: list[str] = field(default_factory=list)
    container_parent_token: OwnedDirectory | None = None
    container_token: OwnedDirectory | None = None
    gates: list[private_cli_leases.LockedReleaseGate] = field(default_factory=list)
    cohorts: list[private_cli_leases.LiveFIFOLeaseCohort] = field(default_factory=list)
    native_proofs: list[dict[str, object]] = field(default_factory=list)
    command_count: int = 0
    command_lock: threading.Lock = field(default_factory=threading.Lock)
    cleanup_done: bool = False
    retained_entries: tuple[OwnedDirectory, ...] = ()

    def command_receipt(self, task_id: str | None, label: str) -> Path:
        safe_task = task_id if task_id and TASK_ID_RE.fullmatch(task_id) else "control"
        with self.command_lock:
            directory = self.response_directory / safe_task
            directory.mkdir(mode=0o700, exist_ok=True)
            index = self.command_count
            self.command_count += 1
        return directory / ("%04d-" % index + label + ".json")

    def add_task(self, task_id: str) -> None:
        require(TASK_ID_RE.fullmatch(task_id) is not None, "task ID is not canonical")
        with self.command_lock:
            if task_id not in self.task_ids:
                self.task_ids.append(task_id)

    def add_workspace(self, workspace_id: str) -> None:
        require(TASK_ID_RE.fullmatch(workspace_id) is not None, "workspace ID is not canonical")
        with self.command_lock:
            if workspace_id not in self.workspace_ids:
                self.workspace_ids.append(workspace_id)

    def release_gates(self) -> None:
        for gate in tuple(self.gates):
            gate.release()
        self.gates.clear()

    def close_cohorts(self, remove_closed: bool) -> list[dict[str, object]]:
        evidence = []
        for cohort in tuple(self.cohorts):
            try:
                if remove_closed:
                    evidence.extend(cohort.remove_closed_fifos())
                cohort.close()
            except (OSError, RuntimeError) as error:
                evidence.append({"error": str(error)})
        self.cohorts.clear()
        return evidence

    def cleanup(self, verdict: str) -> dict[str, object]:
        """Release only fixture gates, then authorize exact cleanup if safe."""
        self.release_gates()
        cohort_evidence = self.close_cohorts(remove_closed=False)
        lifetime_closed = False
        lifetime_error = None
        daemon_closed = not self.private_config.exists()
        daemon_error = None
        if self.state.exists():
            try:
                private_cli_supervisor.authorize_managed_lifetime_closure(
                    self.state, tuple(self.task_ids),
                )
                lifetime_closed = True
            except (RuntimeError, OSError) as error:
                lifetime_error = str(error)
        elif not self.attempted_task_ids:
            lifetime_closed = True
        if lifetime_closed and self.private_config.exists():
            try:
                private_cli_supervisor.shutdown_private_supervisor(
                    self.pueue,
                    self.private_config,
                    self.environment,
                    state=self.state,
                    task_ids=tuple(self.task_ids),
                    capture_root=self.root_token,
                )
                daemon_closed = True
            except (RuntimeError, OSError) as error:
                daemon_closed = False
                daemon_error = str(error)
        process_pending = self.processes.pending()
        can_cleanup, decision = cleanup_decision(
            verdict,
            lifetime_closed=lifetime_closed,
            daemon_closed=daemon_closed,
            task_count=len(self.task_ids),
            pending_process_count=len(process_pending),
        )
        result: dict[str, object] = {
            "decision": decision,
            "authorized": can_cleanup,
            "lifetime_closed": lifetime_closed,
            "daemon_closed": daemon_closed,
            "pending_processes": process_pending,
            "cohort_closure": cohort_evidence,
        }
        if lifetime_error:
            result["lifetime_error"] = lifetime_error
        if daemon_error:
            result["daemon_error"] = daemon_error
        if not can_cleanup:
            entries = self.owner.transfer()
            self.retained_entries = entries
            result["retained_owned_directories"] = [str(entry.directory) for entry in entries]
            return result
        try:
            entries = self.owner.transfer()
            private_cli_root_cleanup.remove_private_acceptance_root(
                self.root_token,
                entries,
                managed_state_roots=((self.state, tuple(self.task_ids)),)
                if self.state.exists() else (),
            )
            self.cleanup_done = True
            result["removed_owned_directories"] = [str(entry.directory) for entry in entries]
            return result
        except (RuntimeError, OSError) as error:
            result["authorized"] = False
            result["decision"] = "cleanup-failed-evidence-retained"
            result["error"] = str(error)
            return result
