"""Bounded CPU snapshot hints; roles never prove process ownership or causality."""

import math
from pathlib import Path

from scripts.acceptance.evidence.validation import _task_identity


CPU_SNAPSHOT_BYTES = 256 * 1024
CPU_SNAPSHOT_ROWS = 4096
CPU_SNAPSHOT_TIMEOUT_SECONDS = 5
CPU_SNAPSHOT_ARGV = ("/bin/ps", "-A", "-o", "pcpu=,comm=")
CPU_SNAPSHOT_ROLES = frozenset((
    "delegate", "delegate-run", "pueue", "pueued", "ccr", "claude", "node",
    "python", "python3", "python3.12", "ps", "mds", "mds_stores", "mdworker_shared",
    "syspolicyd", "xprotectservice", "amfid", "kernel_task",
    "runner.worker", "runner.listener", "trustd", "xprotect",
    "go", "compile", "link", "nested-lifetime-fixture", "pi",
))


def confirmed_host_cpu_response(response: object, task_id: object, root: object) -> bool:
    if not isinstance(response, dict) or not _task_identity(task_id):
        return False
    resource = response.get("resource_admission")
    return (type(response.get("schema_version")) is int and response.get("schema_version") == 1
            and response.get("command") == "dispatch" and _task_identity(response.get("root_id"))
            and (root is None or (_task_identity(root) and response.get("root_id") == root))
            and response.get("task_id") == task_id and response.get("task_record") == "unknown"
            and response.get("admission") == "unknown" and response.get("liveness") == "undetermined"
            and response.get("publication") == "unknown"
            and response.get("failure") == {"code": "resource_pressure", "stage": "check-resources",
                                            "reason_code": "host_cpu_sustained", "next_action": "stop_and_report"}
            and isinstance(resource, dict) and resource.get("status") == "pressure")


def confirmed_host_cpu_refusal(observation: object, root: object) -> bool:
    if not isinstance(observation, dict):
        return False
    return (observation.get("natural_wait") is True and type(observation.get("exit_code")) is int
            and observation.get("exit_code") == 1
            and confirmed_host_cpu_response(observation.get("response"), observation.get("task_id"), root))


def cpu_snapshot_rows(text: str) -> dict[str, object]:
    """Project one native historical-percent listing, never identity or closure."""
    if len(text.encode("utf-8")) > CPU_SNAPSHOT_BYTES:
        raise ValueError("cpu_snapshot_byte_limit")
    lines = text.splitlines()
    if not lines or len(lines) > CPU_SNAPSHOT_ROWS:
        raise ValueError("cpu_snapshot_row_limit_or_empty")
    rows: list[tuple[float, str]] = []
    counts: dict[str, int] = {}
    for line in lines:
        fields = line.split(maxsplit=1)
        if len(fields) != 2:
            raise ValueError("cpu_snapshot_malformed_row")
        percent = float(fields[0])
        if not math.isfinite(percent) or percent < 0:
            raise ValueError("cpu_snapshot_invalid_percent")
        basename = Path(fields[1]).name.lower()
        if not basename:
            raise ValueError("cpu_snapshot_missing_command")
        role = basename if basename in CPU_SNAPSHOT_ROLES else "other"
        rows.append((percent, role))
        counts[role] = counts.get(role, 0) + 1
    rows.sort(key=lambda row: row[0], reverse=True)
    return {"listed_process_count": counts,
            "top_cpu": [{"role": role, "native_cpu_percent": percent} for percent, role in rows[:8]]}
