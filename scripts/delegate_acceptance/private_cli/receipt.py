"""Single mutable receipt owner for one private acceptance invocation."""

from pathlib import Path
from scripts.delegate_acceptance.private_cli.fingerprints import REPOSITORY
from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.fingerprints import descriptor_evidence
from scripts.delegate_acceptance.private_cli.fingerprints import file_fingerprint
import json
import platform
import threading
import time


class AcceptanceReceipt:
    """Own the compact allowlisted receipt for one harness invocation."""

    def __init__(self, output, requested_tasks, dispatch_workers, binaries):
        self.output = Path(output)
        self.started = time.monotonic()
        self.phase_started = self.started
        self.current_phase = "prerequisites"
        self.safe_stage = "invocation-validated"
        self.phase_seconds = {}
        self.counts = {
            "requested_task_count": requested_tasks,
            "dispatch_worker_count": dispatch_workers,
            "dispatch_completed_count": 0,
            "admitted_task_count": 0,
            "running_task_count": 0,
            "live_lease_count": 0,
            "collected_task_count": 0,
        }
        self.queue = {}
        self.details = {}
        self.lock = threading.Lock()
        self.fingerprints = {
            "source": {
                "acceptance_private_cli.py": file_fingerprint(REPOSITORY / "scripts" / "acceptance_private_cli.py")
            },
            "binaries": {role: file_fingerprint(path) for role, path in sorted(binaries.items())},
        }

    def phase(self, name):
        now = time.monotonic()
        with self.lock:
            elapsed = max(0.0, now - self.phase_started)
            self.phase_seconds[self.current_phase] = round(self.phase_seconds.get(self.current_phase, 0.0) + elapsed, 6)
            self.current_phase = name
            self.phase_started = now

    def stage(self, name):
        with self.lock:
            self.safe_stage = name

    def update_counts(self, **values):
        with self.lock:
            self.counts.update(values)

    def increment(self, name, amount=1):
        with self.lock:
            self.counts[name] = self.counts.get(name, 0) + amount

    def update_queue(self, *, task_count=None, running_count=None, status_bytes=None):
        with self.lock:
            if task_count is not None:
                self.queue["task_count"] = task_count
            if running_count is not None:
                self.queue["running_count"] = running_count
            if status_bytes is not None:
                self.queue["status_bytes"] = status_bytes

    def add_details(self, **values):
        with self.lock:
            self.details.update(values)

    def write(self, result, cleanup, error=None):
        now = time.monotonic()
        with self.lock:
            phase_seconds = dict(self.phase_seconds)
            phase_seconds[self.current_phase] = round(
                phase_seconds.get(self.current_phase, 0.0) + max(0.0, now - self.phase_started), 6
            )
            payload = {
                "schema_version": 2,
                "gate": "private-cli-acceptance",
                "result": result,
                "status": {"PASS": "passed", "FAIL": "failed", "BLOCKED": "blocked"}[result],
                "phase": self.current_phase,
                "safe_stage": self.safe_stage,
                "timings_seconds": {"elapsed": round(max(0.0, now - self.started), 6), "phases": phase_seconds},
                "platform": {
                    "system": platform.system(),
                    "kernel": platform.release(),
                    "architecture": platform.machine(),
                },
                "file_descriptors": descriptor_evidence(),
                "counts": dict(self.counts),
                "fingerprints": self.fingerprints,
                "cleanup": cleanup,
            }
            if self.queue:
                payload["queue"] = dict(self.queue)
            payload.update(self.details)
        if error is not None:
            failure_phase = self.details.get("failure_phase", self.current_phase)
            code = (
                error.code
                if isinstance(error, AcceptanceBlocked)
                else (
                    "cleanup_failed"
                    if cleanup.get("status") == "failed"
                    else failure_phase.replace("_", "-") + "-failed"
                )
            )
            payload["failure"] = {"code": code, "exception_type": type(error).__name__, "phase": failure_phase}
        self.output.write_text(json.dumps(payload, sort_keys=True, separators=(",", ":")) + "\n")
