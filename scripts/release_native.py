#!/usr/bin/env python3
"""Execute local native release acceptance with durable, observation-only resume."""

from pathlib import Path
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.native_release.cli import main


if __name__ == "__main__":
    raise SystemExit(main())
