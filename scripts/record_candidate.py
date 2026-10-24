#!/usr/bin/env python3
"""Record one maintainer record (register, intent, triage, install or freeze), or check readiness without one."""

from __future__ import annotations

from pathlib import Path
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.release_verification.candidate_record import main


if __name__ == "__main__":
    raise SystemExit(main())
