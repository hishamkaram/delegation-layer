"""Finite control response fixture; it never opens a state root or launches work."""

import argparse
import json
import re
import sys


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("root_id")
    parser.add_argument("task_id")
    args = parser.parse_args()
    if any(re.fullmatch(r"[0-9a-f]{32}", value) is None for value in (args.root_id, args.task_id)):
        parser.error("root and task identities must be 32 lowercase hexadecimal characters")
    # An explicit --id remains unknown even when resource pressure refuses the
    # invocation: another creator could own that ID. No retry is authorized.
    print(json.dumps({
        "schema_version": 1, "command": "dispatch", "root_id": args.root_id, "task_id": args.task_id,
        "task_record": "unknown", "admission": "unknown", "liveness": "undetermined", "publication": "unknown",
        # Fixed protocol evidence, not measurements or another admission check.
        "resource_admission": {"status": "pressure", "diagnostics": {"host_cpu": {
            "counter_unit": "ticks" if sys.platform == "linux" else "seconds",
            "busy_threshold": 0.95, "required_streak": 3,
            "intervals": [
                {"elapsed_ms": 1000, "busy_delta": 95, "total_delta": 100, "busy_fraction": 0.95, "classification": "hot"},
                {"elapsed_ms": 1002, "busy_delta": 198, "total_delta": 200, "busy_fraction": 0.99, "classification": "hot"},
                {"elapsed_ms": 1001, "busy_delta": 297, "total_delta": 300, "busy_fraction": 0.99, "classification": "hot"},
            ],
        }}},
        "failure": {"code": "resource_pressure", "stage": "check-resources",
                    "reason_code": "host_cpu_sustained", "next_action": "stop_and_report"},
        "error": "Delegate refused new work because resource pressure was confirmed. Pressure: host_cpu (sustained).",
    }))
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
