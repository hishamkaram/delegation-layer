"""Retain the Antigravity run owner until failure cleanup is proven."""

from __future__ import annotations

import sys
from pathlib import Path
from typing import TYPE_CHECKING, Protocol

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.processes.observation import (
    failure_cleanup_deadline,
    wait_for_failure_cleanup,
)
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.provider_drivers.agy.constants import WATCH_SECONDS
from scripts.delegate_acceptance.provider_drivers.agy.queue import pueue_command, status_jobs

if TYPE_CHECKING:
    from scripts.delegate_acceptance.processes.operations import Process, Processes


class ShutdownRun(Protocol):
    """Shutdown's view of the run; only its completion flag is writable."""

    daemon_done: bool

    @property
    def processes(self) -> Processes: ...

    @property
    def daemon(self) -> Process | None: ...

    @property
    def dispatch_attempts(self) -> set[str]: ...

    @property
    def final_queue(self) -> dict[str, object] | None: ...

    @property
    def pueue_config(self) -> Path | None: ...

    @property
    def pueue(self) -> Path: ...

    @property
    def output(self) -> Path: ...

    def failure_queue_finished(self, value: dict[str, object]) -> bool: ...


def failure_shutdown(run: ShutdownRun, repo_root: Path) -> bool:
    """Retain actual handles until reaped; only shut down a proven idle queue.

    A lost dispatch response is not absence. Every attempted dispatch must
    have a matching Done row before automatic failure cleanup is allowed.
    Unknown commands/queue state keep this owner alive for inspection.
    """
    print("Acceptance failed; retaining existing process handles until safe shutdown.",
          file=sys.stderr, flush=True)
    observation = None
    queue_finished = False
    shutdown = next((entry for entry in run.processes.entries
                     if getattr(entry, "argv", [])[-1:] == ["shutdown"]), None)
    deadline = failure_cleanup_deadline(WATCH_SECONDS)
    while True:
        pending = run.processes.pending(exclude=(run.daemon,) if run.daemon else ())
        if pending:
            if not _wait_failure_cleanup(
                    run, deadline, 0.2, "owned acceptance processes did not finish naturally"):
                return False
            continue
        if run.daemon is None:
            require(not run.dispatch_attempts, "dispatch attempts exist without an owned supervisor")
            run.daemon_done = True
            return True
        daemon_result = run.daemon.poll()
        if daemon_result is not None:
            terminal = _handle_daemon_exit(run, daemon_result, queue_finished, deadline)
            if terminal is not None:
                return terminal
            continue
        if shutdown is not None or run.pueue_config is None:
            if not _wait_failure_cleanup(
                    run, deadline, 0.2, "private supervisor shutdown did not complete"):
                return False
            continue
        try:
            if observation is None:
                observation = run.processes.start(
                    "failure-status",
                    pueue_command(run.pueue, run.pueue_config, "status", "--json"),
                    repo_root)
            result = observation.poll()
            if result is None:
                if not _wait_failure_cleanup(
                        run, deadline, 0.2, "private queue observation did not finish"):
                    return False
                continue
            if result["exit_code"] == 0:
                value = status_jobs(observation)
                if run.failure_queue_finished(value):
                    queue_finished = True
                    write_json(run.output / "failure-final-queue.json", value)
                    shutdown = run.processes.start(
                        "failure-pueue-shutdown",
                        pueue_command(run.pueue, run.pueue_config, "shutdown"), repo_root)
            observation = None
        except (AcceptanceFailure, OSError, ValueError) as error:
            write_json(run.output / "retention-error.json", {"error": str(error)}, replace=True)
            # A failed observation may still own a live child. The pending
            # guard above observes that same handle before any next read.
            observation = None
        if not _wait_failure_cleanup(
                run, deadline, 0.5, "private queue did not reach a proven terminal state"):
            return False


def _wait_failure_cleanup(run: ShutdownRun, deadline: float,
                          delay: float, reason: str) -> bool:
    if wait_for_failure_cleanup(deadline, delay):
        return True
    write_json(run.output / "retention-error.json", {
        "error": reason,
        "timeout_seconds": WATCH_SECONDS,
        "signals_sent": 0,
    }, replace=True)
    return False


def _handle_daemon_exit(run: ShutdownRun, daemon_result: dict[str, object],
                        queue_finished: bool, deadline: float) -> bool | None:
    if daemon_result.get("natural_wait") is not True:
        marker = run.output / "supervisor-exited-by-signal.json"
        if not marker.exists():
            write_json(marker, {
                "supervisor_pid": run.daemon.pid,
                "natural_wait": False,
                "queue_termination": "unknown",
                "cleanup_complete": False,
                "signals_sent": 0,
            })
        return False
    if queue_finished or (run.final_queue is not None and
                          run.failure_queue_finished(run.final_queue)):
        run.daemon_done = True
        return True
    marker = run.output / "supervisor-exited-queue-unresolved.json"
    if not marker.exists():
        write_json(marker, {
            "supervisor_pid": run.daemon.pid,
            "queue_termination": "unknown",
            "dispatch_attempts": sorted(run.dispatch_attempts),
            "cleanup_complete": False,
            "signals_sent": 0,
        })
    # A daemon exit is not positive evidence about its jobs. Keep the
    # owner/evidence available; do not fabricate cleanup.
    if _wait_failure_cleanup(
            run, deadline, 0.5, "private queue termination remained unknown after daemon exit"):
        return None
    return False
