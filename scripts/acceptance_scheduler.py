"""Run native scheduler acceptance with an owned CLI/runner source installation.

Usage: python3 scripts/acceptance_scheduler.py --scenario all --output FRESH_DIRECTORY
The successive case covers three actual UTC ticks, failure and restart within
five minutes; overlap/stop covers two ticks within four minutes.
The controlled pi fixture performs no AI requests. See --help for --source-root
and selected native supervisor paths.
"""

from pathlib import Path
import sys


if __package__ in {None, ""}:
    _REPOSITORY_ROOT = str(Path(__file__).resolve().parents[1])
    if _REPOSITORY_ROOT not in sys.path:
        sys.path.insert(0, _REPOSITORY_ROOT)

from scripts.delegate_acceptance.private_cli.scheduler_acceptance import main


if __name__ == "__main__":
    raise SystemExit(main())
