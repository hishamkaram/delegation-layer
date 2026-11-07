"""Single mutable receipt owner for one private acceptance invocation."""

from pathlib import Path
from scripts.delegate_acceptance.private_cli.fingerprints import REPOSITORY
from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.fingerprints import descriptor_evidence
from scripts.delegate_acceptance.private_cli.fingerprints import file_fingerprint
from scripts.delegate_acceptance.private_cli.fingerprints import installed_executable_observation
import json
import math
import platform
import threading
import time
from typing import TypeGuard


def _finite_diagnostic_number(value: object) -> TypeGuard[int | float]:
    return ((type(value) is int and value.bit_length() <= 64)
            or (type(value) is float and math.isfinite(value)))


def _host_cpu_evidence(value: object) -> dict[str, object] | None:
    """Allowlist the tested public representation; never recalculate admission policy."""
    if not isinstance(value, dict):
        return None
    unit, threshold, streak, intervals = (value.get(key) for key in
                                         ("counter_unit", "busy_threshold", "required_streak", "intervals"))
    if (unit not in ("ticks", "seconds") or not _finite_diagnostic_number(threshold)
            or not 0 <= threshold <= 1 or type(streak) is not int or not 1 <= streak <= 3
            or not isinstance(intervals, list) or not 1 <= len(intervals) <= 3):
        return None
    projected = []
    for interval in intervals:
        if not isinstance(interval, dict):
            return None
        elapsed, classification = interval.get("elapsed_ms"), interval.get("classification")
        if (type(elapsed) is not int or not 0 <= elapsed < 2**63
                or classification not in ("clear", "hot", "unavailable")):
            return None
        item = {"elapsed_ms": elapsed, "classification": classification}
        for key in ("busy_delta", "total_delta", "busy_fraction"):
            if key in interval:
                number = interval[key]
                if not _finite_diagnostic_number(number) or (key == "busy_fraction" and not 0 <= number <= 1):
                    return None
                item[key] = number
        if classification == "unavailable":
            reason = interval.get("unavailable_reason")
            if ("busy_fraction" in item or reason not in
                    ("unreadable", "malformed", "counter_reset", "no_elapsed_time", "deadline", "unsupported_platform")):
                return None
            item["unavailable_reason"] = reason
        elif (not all(key in item for key in ("busy_delta", "total_delta", "busy_fraction"))
              or item["busy_delta"] < 0 or item["total_delta"] <= 0 or item["busy_delta"] > item["total_delta"]):
            return None
        projected.append(item)
    return {"counter_unit": unit, "busy_threshold": threshold, "required_streak": streak, "intervals": projected}


def resource_admission_evidence(response: object) -> dict[str, object] | None:
    """Retain only public status and optional bounded CPU refusal evidence."""
    if not isinstance(response, dict):
        return None
    resource = response.get("resource_admission")
    if not isinstance(resource, dict) or resource.get("status") not in ("clear", "pressure", "unknown"):
        return None
    evidence = {"status": resource["status"]}
    failure, diagnostics = response.get("failure"), resource.get("diagnostics")
    if (resource["status"] == "pressure" and isinstance(failure, dict)
            and failure.get("code") == "resource_pressure" and failure.get("reason_code") == "host_cpu_sustained"
            and isinstance(diagnostics, dict)):
        host_cpu = _host_cpu_evidence(diagnostics.get("host_cpu"))
        if host_cpu is not None:
            evidence["diagnostics"] = {"host_cpu": host_cpu}
    return evidence


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
            "binaries": {role: installed_executable_observation(path) for role, path in sorted(binaries.items())},
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

    def observe_installations(self, binaries):
        observations = {role: installed_executable_observation(path) for role, path in binaries.items()}
        with self.lock:
            self.fingerprints["binaries"].update(observations)

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
