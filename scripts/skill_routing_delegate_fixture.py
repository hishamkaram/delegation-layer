#!/usr/bin/env python3
"""Routing-only Delegate double. Never launches provider work."""
import argparse
import json
import os
from pathlib import Path
import sys

root = Path(os.environ["DELEGATE_ROUTING_FIXTURE"])
argv = sys.argv[1:]
with (root / "calls.jsonl").open("a", encoding="utf-8") as stream:
    stream.write(json.dumps(argv) + "\n")
parser = argparse.ArgumentParser(add_help=False)
for flag in ("--root", "--runner", "--pueue-config"):
    parser.add_argument(flag)
parser.add_argument("--json", action="store_true")
_, command = parser.parse_known_args(argv)
if command in (["--version"], ["version"]):
    print("delegate routing-fixture")
    sys.exit(0)
if command == ["providers"]:
    print((root / "catalog.json").read_text(encoding="utf-8"))
    sys.exit(0)
print(json.dumps({
    "schema_version": 1, "command": next((a for a in argv if a in
        ("dispatch", "preflight", "models", "capabilities")), "unknown"),
    "task_record": "not_created", "admission": "unknown", "liveness": "undetermined",
    "publication": "unknown", "failure": {"code": "provider_unavailable",
        "stage": "prepare-provider", "next_action": "stop_and_report"},
    "error": "Routing-only test fixture: no provider work is launched."}))
sys.exit(2)
