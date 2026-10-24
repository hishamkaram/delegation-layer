#!/usr/bin/env python3
"""Observe one pull request's development readiness from trusted main and post it as a commit status."""

from __future__ import annotations

from pathlib import Path
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.release_verification.candidate_readiness_check import main


if __name__ == "__main__":
    raise SystemExit(main())
