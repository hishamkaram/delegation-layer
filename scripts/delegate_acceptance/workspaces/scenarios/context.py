"""Mutable state and exact cleanup ownership for one workspace run."""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
import threading
import time

import scripts.delegate_acceptance.private_cli.leases as private_cli_leases
import scripts.delegate_acceptance.private_cli.supervisor as private_cli_supervisor
from scripts.delegate_acceptance.private_cli.fixture_lifetime import SupervisorCleanup
import scripts.delegate_acceptance.private_cli.root_cleanup as private_cli_root_cleanup
from scripts.delegate_acceptance.processes.contracts import command_closure_unproven
from scripts.delegate_acceptance.directories.operations import (
    DirectoryOwner, OwnedDirectory, release_owned_directories,
)
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.directories.policy import require
from .model import TASK_ID_RE
from .policy import cleanup_decision

# Matches the private CLI fixture's post-release closure wait (private_cli/finalization.py).
MANAGED_LIFETIME_CLOSURE_TIMEOUT = 45


class RetainedDirectories:
    """Hold one explicit handoff of a DirectoryOwner inventory until idempotent terminal release.

    The tuple stays here through every remover call, guard and authorization so no token is
    unreachable; terminal release consumes only still-live pins and never deletes a directory.
    """

    def __init__(self, owner: DirectoryOwner):
        self._owner = owner
        self._entries: tuple[OwnedDirectory, ...] | None = None

    def take(self) -> tuple[OwnedDirectory, ...]:
        """Transfer the inventory on first use; later calls return the same tuple."""
        if self._entries is None:
            self._entries = self._owner.transfer()
        return self._entries

    def release(self) -> None:
        live = tuple(entry for entry in self.take() if not entry.consumed)
        try:
            release_owned_directories(live)
        finally:
            self._entries = tuple(entry for entry in self.take() if not entry.consumed)


class AcceptanceSetupFailure(RuntimeError):
    """Carry partial exact-directory ownership to finalization."""

    def __init__(
        self, cause: BaseException, owner: DirectoryOwner, root_token: OwnedDirectory,
        processes: Processes | None = None,
    ):
        super().__init__(str(cause))
        self.cause = cause
        self.owner = owner
        self.root_token = root_token
        self.processes = processes
        self.directories = RetainedDirectories(owner)

    def retained_directories(self) -> tuple[OwnedDirectory, ...]:
        return self.directories.take()

    def release_retained(self) -> None:
        """Relinquish the finished setup's directory pins; never deletes or ends a child."""
        self.directories.release()

    def cleanup(self) -> dict[str, object]:
        """Remove the exact root only after the one process registry shows no pending child."""
        entries = self.retained_directories()
        paths = [str(entry.directory) for entry in entries]
        refusal = self._process_refusal(paths)
        if refusal is not None:
            return refusal
        try:
            private_cli_root_cleanup.remove_private_acceptance_root(self.root_token, entries)
        except (OSError, RuntimeError) as error:
            # Removal may have started: paths are the creation inventory, presence is unverified.
            return {
                "authorized": False,
                "decision": "setup-cleanup-failed-evidence-retained",
                "error": str(error),
                "retained_owned_directories": paths,
                "retained_owned_directories_status": "unverified",
            }
        return {
            "authorized": True,
            "decision": "setup-failure-exact-root-removed",
            "removed_owned_directories": paths,
        }

    def _process_refusal(self, paths: list[str]) -> dict[str, object] | None:
        if command_closure_unproven(self.cause):
            return {
                "authorized": False,
                "decision": "setup-installation-closure-unproven",
                "retained_owned_directories": paths,
            }
        if self.processes is None:
            return None
        try:
            pending = self.processes.pending()
        except (OSError, RuntimeError) as error:
            return {
                "authorized": False,
                "decision": "setup-process-closure-unproven",
                "error": str(error),
                "retained_owned_directories": paths,
            }
        if not pending:
            return None
        _allowed, decision = cleanup_decision(
            "failed", lifetime_closed=True, daemon_closed=True, task_count=0,
            pending_process_count=len(pending),
        )
        return {
            "authorized": False,
            "decision": decision,
            "pending_processes": pending,
            "retained_owned_directories": paths,
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
    directories: RetainedDirectories = field(init=False, repr=False)

    def __post_init__(self) -> None:
        self.directories = RetainedDirectories(self.owner)

    def retained_directories(self) -> tuple[OwnedDirectory, ...]:
        return self.directories.take()

    def release_retained(self) -> None:
        """Relinquish the finished run's directory pins; never deletes or ends a child."""
        self.directories.release()

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
        evidence: list[dict[str, object]] = []
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
        """Release only fixture gates, then authorize exact cleanup if safe.

        One disposal owner holds the state root's lease from the authorized shutdown
        through exact root deletion; deletion takes it within this cleanup's deadline
        when the daemon was already closed.
        """
        deadline = time.monotonic() + MANAGED_LIFETIME_CLOSURE_TIMEOUT
        with private_cli_leases.DisposalLeases() as disposal:
            return self._cleanup_with_disposal(verdict, disposal, deadline)

    def _cleanup_with_disposal(
        self, verdict: str, disposal: private_cli_leases.DisposalLeases, deadline: float
    ) -> dict[str, object]:
        self.release_gates()
        cohort_evidence = self.close_cohorts(remove_closed=False)
        lifetime_closed = False
        lifetime_error = None
        daemon_closed = not self.private_config.exists()
        daemon_error = None
        # Observed once: a state present here stays a managed root through removal, so a later
        # rename cannot drop the remover's closure and custody recheck.
        managed_state_roots = ((self.state, tuple(self.task_ids)),) if self.state.exists() else ()
        if managed_state_roots:
            # A worker released by the gates may still be closing; wait for canonical closure
            # so a late close cannot skip the private daemon's shutdown.
            try:
                private_cli_supervisor.await_managed_lifetime_closure(
                    self.state, tuple(self.task_ids), timeout=MANAGED_LIFETIME_CLOSURE_TIMEOUT,
                )
                lifetime_closed = True
            except (RuntimeError, OSError) as error:
                lifetime_error = str(error)
        elif not self.attempted_task_ids:
            lifetime_closed = True
        pending: SupervisorCleanup | None = None
        if lifetime_closed and managed_state_roots:
            pending = {"delegate": self.delegate, "client": self.pueue, "config": self.private_config,
                       "env": self.environment, "state": self.state, "task_ids": list(self.task_ids)}
            try:
                private_cli_supervisor.shutdown_pending_private_supervisor(
                    pending, [pending], capture_root=self.root_token, disposal=disposal, deadline=deadline)

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
        if pending is not None and "owner" in pending:
            result["terminal_cleanup"] = private_cli_supervisor.terminal_cleanup_evidence(pending)
        if lifetime_error:
            result["lifetime_error"] = lifetime_error
        if daemon_error:
            result["daemon_error"] = daemon_error
        if not can_cleanup:
            entries = self.retained_directories()
            result["retained_owned_directories"] = [str(entry.directory) for entry in entries]
            return result
        try:
            entries = self.retained_directories()
            private_cli_root_cleanup.remove_private_acceptance_root(
                self.root_token,
                entries,
                managed_state_roots=managed_state_roots,
                disposal=disposal,
                deadline=deadline,
            )
            self.cleanup_done = True
            result["removed_owned_directories"] = [str(entry.directory) for entry in entries]
            return result
        except (RuntimeError, OSError) as error:
            result["authorized"] = False
            result["decision"] = "cleanup-failed-evidence-retained"
            result["error"] = str(error)
            return result
