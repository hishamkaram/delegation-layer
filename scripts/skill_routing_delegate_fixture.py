#!/usr/bin/env python3
"""Record routing calls; use real Delegate validation without provider executables."""
import json
import os
from pathlib import Path
import subprocess
import sys

root = Path(os.environ["DELEGATE_ROUTING_FIXTURE"])
argv = sys.argv[1:]
with (root / "calls.jsonl").open("a", encoding="utf-8") as stream:
    stream.write(json.dumps(argv) + "\n")
runtime = json.loads((root / "runtime.json").read_text(encoding="utf-8"))
# Only this child loses provider executables. The initiating harness keeps its
# normal profile, PATH, and competing integrations, so bypasses remain observable.
result = subprocess.run([runtime["delegate"], *argv],
                        env=dict(os.environ, PATH=runtime["path"]),
                        capture_output=True, text=True, check=False)
try:
    response = json.loads(result.stdout)
except ValueError:
    response = None
# Keep model selection deterministic, but only after real parsing accepted a
# CCR discovery request. Syntax errors retain their actual error response.
if isinstance(response, dict) and response.get("command") == "models" \
        and response.get("provider") == "ccr:print" \
        and response.get("reason_code") == "discovery_unavailable" \
        and result.returncode == 2:
    response.update(status="available", reason_code="ok", complete=True,
                    source="ccr model list --json", efforts=None,
                    models=[{"id": "local-review", "name": "Local review model", "efforts": None}])
    print(json.dumps(response))
    sys.exit(0)
sys.stdout.write(result.stdout)
sys.stderr.write(result.stderr)
sys.exit(result.returncode)
