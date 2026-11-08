"""Run native scheduler acceptance with an owned CLI/runner source installation.

Usage: python3 scripts/acceptance/private_cli/scheduler/cli.py --scenario all --output FRESH_DIRECTORY
The successive case covers three actual UTC ticks, failure and restart within
five minutes; overlap/stop covers two ticks within four minutes.
The controlled pi fixture performs no AI requests. See --help for --source-root
and selected native supervisor paths.
"""

import sys
from pathlib import Path

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[4])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]





from scripts.acceptance.private_cli.scheduler.acceptance import main


if __name__ == "__main__":
    raise SystemExit(main())
